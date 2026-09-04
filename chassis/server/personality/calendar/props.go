package calendar

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
)

// props.go — MKCALENDAR and PROPPATCH, which go-webdav v0.7.0 does not
// serve (405 and 501). Both carry a <prop> of client-set properties; the
// ones the store keeps are DAV:displayname, CALDAV:calendar-description,
// CALDAV:calendar-timezone (the TZID of the embedded VTIMEZONE), and
// Apple's calendar-color / calendar-order. Anything else is answered 403
// per property, which clients tolerate.

const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsApple  = "http://apple.com/ns/ical/"
)

type propElem struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
	Inner   []byte `xml:",innerxml"`
}

type propBlock struct {
	Any []propElem `xml:",any"`
}

type propSet struct {
	Prop propBlock `xml:"prop"`
}

type propUpdate struct {
	XMLName xml.Name
	Set     []propSet `xml:"set"`
	Remove  []propSet `xml:"remove"`
}

var colorRE = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)

// propKey names a property the store keeps; "" ⇒ unsupported.
func propKey(n xml.Name) string {
	switch n.Space + " " + n.Local {
	case nsDAV + " displayname":
		return "displayname"
	case nsCalDAV + " calendar-description":
		return "description"
	case nsCalDAV + " calendar-timezone":
		return "timezone"
	case nsApple + " calendar-color":
		return "color"
	case nsApple + " calendar-order":
		return "order"
	}
	return ""
}

// readProps parses a MKCALENDAR / PROPPATCH body into (accepted values,
// accepted names, rejected names).
func readProps(r *http.Request, max int64) (vals map[string]string, ok, bad []xml.Name, err error) {
	vals = map[string]string{}
	body, err := io.ReadAll(io.LimitReader(r.Body, max))
	if err != nil {
		return nil, nil, nil, err
	}
	if strings.TrimSpace(string(body)) == "" {
		return vals, nil, nil, nil
	}
	var u propUpdate
	if err := xml.Unmarshal(body, &u); err != nil {
		return nil, nil, nil, err
	}
	for _, s := range u.Set {
		for _, p := range s.Prop.Any {
			k := propKey(p.XMLName)
			v := strings.TrimSpace(p.Value)
			switch k {
			case "":
				bad = append(bad, p.XMLName)
				continue
			case "color":
				if v != "" && !colorRE.MatchString(v) {
					bad = append(bad, p.XMLName)
					continue
				}
			case "order":
				if v != "" {
					if _, perr := strconv.Atoi(v); perr != nil {
						bad = append(bad, p.XMLName)
						continue
					}
				}
			case "timezone":
				v = tzidOf(v)
			}
			vals[k] = v
			ok = append(ok, p.XMLName)
		}
	}
	for _, s := range u.Remove {
		for _, p := range s.Prop.Any {
			k := propKey(p.XMLName)
			if k == "" || k == "timezone" {
				bad = append(bad, p.XMLName)
				continue
			}
			vals[k] = ""
			ok = append(ok, p.XMLName)
		}
	}
	return vals, ok, bad, nil
}

// tzidOf pulls the TZID out of a calendar-timezone value (a VCALENDAR with
// one VTIMEZONE); "" when it is not one the chassis knows.
func tzidOf(v string) string {
	for _, line := range strings.Split(strings.ReplaceAll(v, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "TZID:") {
			id := strings.TrimSpace(strings.TrimPrefix(line, "TZID:"))
			if _, err := loadLocation(id); err == nil {
				return id
			}
			return ""
		}
	}
	return ""
}

// createCalendar is the shared MKCALENDAR / MKCOL commit: policy, ensure,
// observe. status 0 ⇒ created.
func (c *Controller) createCalendar(ctx context.Context, pr principal, name string, props map[string]string) (chcal.Calendar, int, string) {
	if !chcal.ValidCalendarName(name) {
		return chcal.Calendar{}, http.StatusForbidden, "calendar name is not a URL segment"
	}
	if _, found, err := c.store.GetCalendar(ctx, pr.tenant, pr.username, name); err != nil {
		return chcal.Calendar{}, http.StatusServiceUnavailable, "temporary failure"
	} else if found {
		return chcal.Calendar{}, http.StatusMethodNotAllowed, "calendar exists"
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opMkcalendar,
		calendar: calRef{Name: name, DisplayName: props["displayname"], Timezone: props["timezone"]}, props: props, clientIP: pr.clientIP}
	if status, msg := c.gate(nil, &pr.acct, chcal.VerbMkcalendar, &m); status != 0 {
		return chcal.Calendar{}, status, msg
	}
	order, _ := strconv.Atoi(props["order"])
	cal, _, err := c.store.EnsureCalendar(ctx, chcal.Calendar{Tenant: pr.tenant, Username: pr.username, Name: name,
		DisplayName: props["displayname"], Description: props["description"], Color: props["color"], SortOrder: order, Timezone: props["timezone"]})
	if err != nil {
		return chcal.Calendar{}, http.StatusServiceUnavailable, "temporary failure"
	}
	m.calendar = refOf(cal)
	c.after(nil, &pr.acct, chcal.VerbMkcalendar, m)
	return cal, 0, ""
}

func (c *Controller) serveMkcalendar(w http.ResponseWriter, r *http.Request, pr principal, parts []string) {
	if len(parts) != 3 || parts[1] != "calendars" {
		http.Error(w, "calendars live under "+c.prefix+"/"+pr.username+"/calendars/", http.StatusForbidden)
		return
	}
	props, _, _, err := readProps(r, 64<<10)
	if err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}
	if _, status, msg := c.createCalendar(r.Context(), pr, parts[2], props); status != 0 {
		http.Error(w, msg, status)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (c *Controller) serveProppatch(w http.ResponseWriter, r *http.Request, pr principal, parts []string) {
	if len(parts) != 3 || parts[1] != "calendars" {
		http.Error(w, "properties can be set on a calendar only", http.StatusForbidden)
		return
	}
	cal, found, err := c.store.GetCalendar(r.Context(), pr.tenant, pr.username, parts[2])
	if err != nil {
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	props, okNames, badNames, err := readProps(r, 64<<10)
	if err != nil {
		http.Error(w, "malformed request body", http.StatusBadRequest)
		return
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opProppatch, calendar: refOf(cal), props: props, clientIP: pr.clientIP}
	if status, msg := c.gate(&cal, &pr.acct, chcal.VerbProppatch, &m); status != 0 {
		http.Error(w, msg, status)
		return
	}
	if len(okNames) > 0 {
		var dn, desc, color *string
		var order *int
		if v, ok := props["displayname"]; ok {
			dn = &v
		}
		if v, ok := props["description"]; ok {
			desc = &v
		}
		if v, ok := props["color"]; ok {
			color = &v
		}
		if v, ok := props["order"]; ok {
			n, _ := strconv.Atoi(v)
			order = &n
		}
		if err := c.store.SetCalendarProps(r.Context(), cal.ID, dn, desc, color, order); err != nil {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		if tz, ok := props["timezone"]; ok && tz != "" {
			_, _, _ = c.store.EnsureCalendar(r.Context(), chcal.Calendar{Tenant: pr.tenant, Username: pr.username, Name: cal.Name, Timezone: tz})
		}
		c.after(&cal, &pr.acct, chcal.VerbProppatch, m)
	}
	writeMultistatus(w, c.prefix+"/"+pr.username+"/calendars/"+cal.Name+"/", okNames, badNames)
}

// multistatus shapes for the PROPPATCH reply.
type msProp struct {
	Any []propElem `xml:",any"`
}

type msPropstat struct {
	Prop   msProp `xml:"DAV: prop"`
	Status string `xml:"DAV: status"`
}

type msResponse struct {
	Href     string       `xml:"DAV: href"`
	Propstat []msPropstat `xml:"DAV: propstat"`
}

type multistatus struct {
	XMLName  xml.Name     `xml:"DAV: multistatus"`
	Response []msResponse `xml:"DAV: response"`
}

func writeMultistatus(w http.ResponseWriter, href string, okNames, badNames []xml.Name) {
	resp := msResponse{Href: href}
	if len(okNames) > 0 {
		ps := msPropstat{Status: "HTTP/1.1 200 OK"}
		for _, n := range okNames {
			ps.Prop.Any = append(ps.Prop.Any, propElem{XMLName: n})
		}
		resp.Propstat = append(resp.Propstat, ps)
	}
	if len(badNames) > 0 {
		ps := msPropstat{Status: "HTTP/1.1 403 Forbidden"}
		for _, n := range badNames {
			ps.Prop.Any = append(ps.Prop.Any, propElem{XMLName: n})
		}
		resp.Propstat = append(resp.Propstat, ps)
	}
	body, err := xml.Marshal(multistatus{Response: []msResponse{resp}})
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
