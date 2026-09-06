package contacts

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"

	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
)

// props.go — PROPPATCH, which go-webdav v0.7.0 answers 405 per property,
// and the shared MKCOL commit the backend's CreateAddressBook calls. The
// properties the store keeps are DAV:displayname and
// CARDDAV:addressbook-description; anything else is answered 403 per
// property, which clients tolerate.

const (
	nsDAV     = "DAV:"
	nsCardDAV = "urn:ietf:params:xml:ns:carddav"
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

// propKey names a property the store keeps; "" ⇒ unsupported.
func propKey(n xml.Name) string {
	switch n.Space + " " + n.Local {
	case nsDAV + " displayname":
		return "displayname"
	case nsCardDAV + " addressbook-description":
		return "description"
	}
	return ""
}

// readProps parses a PROPPATCH body into (accepted values, accepted
// names, rejected names).
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
			if k == "" {
				bad = append(bad, p.XMLName)
				continue
			}
			vals[k] = strings.TrimSpace(p.Value)
			ok = append(ok, p.XMLName)
		}
	}
	for _, s := range u.Remove {
		for _, p := range s.Prop.Any {
			k := propKey(p.XMLName)
			if k == "" {
				bad = append(bad, p.XMLName)
				continue
			}
			vals[k] = ""
			ok = append(ok, p.XMLName)
		}
	}
	return vals, ok, bad, nil
}

// createAddressbook is the shared MKCOL commit: policy, ensure, observe.
// status 0 ⇒ created.
func (c *Controller) createAddressbook(ctx context.Context, pr principal, name string, props map[string]string) (chcon.Addressbook, int, string) {
	if !chcon.ValidAddressbookName(name) {
		return chcon.Addressbook{}, http.StatusForbidden, "address book name is not a URL segment"
	}
	if _, found, err := c.store.GetAddressbook(ctx, pr.tenant, pr.username, name); err != nil {
		return chcon.Addressbook{}, http.StatusServiceUnavailable, "temporary failure"
	} else if found {
		return chcon.Addressbook{}, http.StatusMethodNotAllowed, "address book exists"
	}
	m := mutation{tenant: pr.tenant, account: pr.username, op: opMkaddressbook,
		addressbook: abRef{Name: name, DisplayName: props["displayname"]}, props: props, clientIP: pr.clientIP}
	if status, msg := c.gate(nil, &pr.acct, chcon.VerbMkaddressbook, &m); status != 0 {
		return chcon.Addressbook{}, status, msg
	}
	order, _ := strconv.Atoi(props["order"])
	ab, _, err := c.store.EnsureAddressbook(ctx, chcon.Addressbook{Tenant: pr.tenant, Username: pr.username, Name: name,
		DisplayName: props["displayname"], Description: props["description"], SortOrder: order})
	if err != nil {
		return chcon.Addressbook{}, http.StatusServiceUnavailable, "temporary failure"
	}
	m.addressbook = refOf(ab)
	c.after(nil, &pr.acct, chcon.VerbMkaddressbook, m)
	return ab, 0, ""
}

func (c *Controller) serveProppatch(w http.ResponseWriter, r *http.Request, pr principal, parts []string) {
	if len(parts) != 3 || parts[1] != "addressbooks" {
		http.Error(w, "properties can be set on an address book only", http.StatusForbidden)
		return
	}
	ab, found, err := c.store.GetAddressbook(r.Context(), pr.tenant, pr.username, parts[2])
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
	m := mutation{tenant: pr.tenant, account: pr.username, op: opProppatch, addressbook: refOf(ab), props: props, clientIP: pr.clientIP}
	if status, msg := c.gate(&ab, &pr.acct, chcon.VerbProppatch, &m); status != 0 {
		http.Error(w, msg, status)
		return
	}
	if len(okNames) > 0 {
		var dn, desc *string
		if v, ok := props["displayname"]; ok {
			dn = &v
		}
		if v, ok := props["description"]; ok {
			desc = &v
		}
		if err := c.store.SetAddressbookProps(r.Context(), ab.ID, dn, desc, nil); err != nil {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		c.after(&ab, &pr.acct, chcon.VerbProppatch, m)
	}
	writeMultistatus(w, c.prefix+"/"+pr.username+"/addressbooks/"+ab.Name+"/", okNames, badNames)
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
