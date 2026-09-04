package calendar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// ProdID identifies chassis-rendered calendars.
const ProdID = "-//loremlabs//txco calendar//EN"

// Recur is the parsed RRULE — rrule-go's option set in JSON clothing so a
// sandboxed compute can read a rule without an iCalendar parser.
type Recur struct {
	Freq       string   `json:"freq"`
	Interval   int      `json:"interval"`
	ByDay      []string `json:"byday"`
	ByMonthDay []int    `json:"bymonthday"`
	ByMonth    []int    `json:"bymonth"`
	ByHour     []int    `json:"byhour"`
	ByMinute   []int    `json:"byminute"`
	BySetPos   []int    `json:"bysetpos"`
	Count      int      `json:"count"`
	Until      string   `json:"until"`
}

// Event is the chassis's structured form of one VEVENT: generic iCalendar
// vocabulary, no product words. On the way IN (txco://calendar/put WITH
// event, @calendar.res.event) the first block is read; on the way OUT
// (@calendar.event, txco://calendar/get) the facts are filled in as well.
//
// Times: `start`/`end` are "YYYY-MM-DD" (all-day), RFC3339 ("…Z" or with
// an offset, stored as UTC), or a local "YYYY-MM-DDTHH:MM:SS" together with
// `tzid` (an IANA name). Floating times are refused. `duration` is an
// RFC 5545 duration ("PT30M"); give `end` or `duration`, not both; neither
// means PT1H (P1D for all-day). `rrule` is an RFC 5545 RECUR value
// ("FREQ=DAILY"), `exdate` entries take the same forms as `start`.
type Event struct {
	UID         string   `json:"uid,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	Status      string   `json:"status,omitempty"`
	URL         string   `json:"url,omitempty"`
	Start       string   `json:"start,omitempty"`
	End         string   `json:"end,omitempty"`
	Duration    string   `json:"duration,omitempty"`
	TZID        string   `json:"tzid,omitempty"`
	RRule       string   `json:"rrule,omitempty"`
	ExDate      []string `json:"exdate,omitempty"`

	// Facts the chassis fills in when parsing; ignored when rendering.
	Component string `json:"component,omitempty"`
	AllDay    bool   `json:"all_day"`
	StartUTC  string `json:"start_utc,omitempty"`
	EndUTC    string `json:"end_utc,omitempty"`
	Recurs    bool   `json:"recurs"`
	Recur     *Recur `json:"recur,omitempty"`
	Sequence  int64  `json:"sequence"`
	DTStamp   string `json:"dtstamp,omitempty"`
}

// EventFromJSON reads an Event from its JSON form (the WITH event{} param or
// a stack's @calendar.res.event).
func EventFromJSON(raw []byte) (Event, error) {
	var ev Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return Event{}, fmt.Errorf("event: %w", err)
	}
	return ev, nil
}

const (
	dateForm      = "2006-01-02"
	localForm     = "2006-01-02T15:04:05"
	localFormNoSS = "2006-01-02T15:04"
)

// when is one parsed start/end/exdate value.
type when struct {
	t      time.Time // in loc (or UTC)
	allDay bool
	loc    *time.Location // nil ⇒ UTC instant
}

func parseWhen(v, tzid string) (when, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return when{}, errors.New("empty time")
	}
	if len(v) == len(dateForm) {
		t, err := time.ParseInLocation(dateForm, v, time.UTC)
		if err != nil {
			return when{}, fmt.Errorf("%q is not a date (YYYY-MM-DD)", v)
		}
		return when{t: t, allDay: true}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return when{t: t.UTC()}, nil
	}
	for _, f := range []string{localForm, localFormNoSS} {
		if len(v) != len(f) {
			continue
		}
		if tzid == "" {
			return when{}, fmt.Errorf("%q is a floating time; give tzid (an IANA zone) or a Z/offset suffix", v)
		}
		loc, err := time.LoadLocation(tzid)
		if err != nil || tzid == "Local" {
			return when{}, fmt.Errorf("unknown tzid %q", tzid)
		}
		t, err := time.ParseInLocation(f, v, loc)
		if err != nil {
			return when{}, fmt.Errorf("%q is not a local time (YYYY-MM-DDTHH:MM:SS)", v)
		}
		if loc == time.UTC || strings.EqualFold(tzid, "UTC") {
			return when{t: t.UTC()}, nil
		}
		return when{t: t, loc: loc}, nil
	}
	return when{}, fmt.Errorf("%q is not a date, an RFC3339 time, or a local time", v)
}

func setWhen(props ical.Props, name string, w when) {
	p := ical.NewProp(name)
	switch {
	case w.allDay:
		p.SetDate(w.t)
	case w.loc != nil:
		p.SetDateTime(w.t.In(w.loc))
	default:
		p.SetDateTime(w.t.UTC())
	}
	props.Set(p)
}

var statuses = map[string]bool{"CONFIRMED": true, "TENTATIVE": true, "CANCELLED": true}

// Render encodes ev as a canonical VCALENDAR (one VEVENT, a VTIMEZONE when
// the event carries a TZID). now is the DTSTAMP. Deterministic apart from
// DTSTAMP and the VTIMEZONE's yearly window, which the store's no-op rule
// ignores or tolerates.
func Render(ev Event, now time.Time) ([]byte, error) {
	if strings.TrimSpace(ev.UID) == "" {
		return nil, errors.New("event: uid is required")
	}
	if strings.TrimSpace(ev.Start) == "" {
		return nil, errors.New("event: start is required")
	}
	start, err := parseWhen(ev.Start, ev.TZID)
	if err != nil {
		return nil, fmt.Errorf("event.start: %w", err)
	}
	if ev.End != "" && ev.Duration != "" {
		return nil, errors.New("event: give end or duration, not both")
	}
	comp := ical.NewComponent(ical.CompEvent)
	comp.Props.SetText(ical.PropUID, strings.TrimSpace(ev.UID))
	comp.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC().Truncate(time.Second))
	if ev.Sequence < 0 {
		return nil, errors.New("event: sequence must be >= 0")
	}
	seq := ical.NewProp(ical.PropSequence)
	seq.Value = strconv.FormatInt(ev.Sequence, 10)
	comp.Props.Set(seq)
	for name, v := range map[string]string{
		ical.PropSummary: ev.Summary, ical.PropDescription: ev.Description, ical.PropLocation: ev.Location,
	} {
		if v != "" {
			comp.Props.SetText(name, v)
		}
	}
	if ev.Status != "" {
		st := strings.ToUpper(strings.TrimSpace(ev.Status))
		if !statuses[st] {
			return nil, fmt.Errorf("event.status: %q is not CONFIRMED, TENTATIVE or CANCELLED", ev.Status)
		}
		comp.Props.SetText(ical.PropStatus, st)
	}
	if ev.URL != "" {
		u, err := url.Parse(strings.TrimSpace(ev.URL))
		if err != nil || u.Scheme == "" {
			return nil, fmt.Errorf("event.url: %q is not an absolute URL", ev.URL)
		}
		comp.Props.SetURI(ical.PropURL, u)
	}
	setWhen(comp.Props, ical.PropDateTimeStart, start)
	switch {
	case ev.End != "":
		end, err := parseWhen(ev.End, ev.TZID)
		if err != nil {
			return nil, fmt.Errorf("event.end: %w", err)
		}
		if end.allDay != start.allDay {
			return nil, errors.New("event: start and end must both be dates or both be times")
		}
		if !end.t.After(start.t) {
			return nil, errors.New("event: end must be after start")
		}
		setWhen(comp.Props, ical.PropDateTimeEnd, end)
	case ev.Duration != "":
		d := ical.NewProp(ical.PropDuration)
		d.Value = strings.ToUpper(strings.TrimSpace(ev.Duration))
		if _, err := d.Duration(); err != nil {
			return nil, fmt.Errorf("event.duration: %q is not an RFC 5545 duration (PT30M, P1D)", ev.Duration)
		}
		comp.Props.Set(d)
	case start.allDay:
		setWhen(comp.Props, ical.PropDateTimeEnd, when{t: start.t.AddDate(0, 0, 1), allDay: true})
	default:
		d := ical.NewProp(ical.PropDuration)
		d.Value = "PT1H"
		comp.Props.Set(d)
	}
	if ev.RRule != "" {
		rule := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(ev.RRule), "RRULE:")))
		if _, err := rrule.StrToROption(rule); err != nil {
			return nil, fmt.Errorf("event.rrule: %q: %v", ev.RRule, err)
		}
		p := ical.NewProp(ical.PropRecurrenceRule)
		p.Value = rule
		comp.Props.Set(p)
	}
	for _, x := range ev.ExDate {
		w, err := parseWhen(x, ev.TZID)
		if err != nil {
			return nil, fmt.Errorf("event.exdate: %w", err)
		}
		if w.allDay != start.allDay {
			return nil, fmt.Errorf("event.exdate: %q must be the same kind as start", x)
		}
		p := ical.NewProp(ical.PropExceptionDates)
		switch {
		case w.allDay:
			p.SetDate(w.t)
		case start.loc != nil:
			p.SetDateTime(w.t.In(start.loc))
		default:
			p.SetDateTime(w.t.UTC())
		}
		comp.Props.Add(p)
	}

	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, ProdID)
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropCalendarScale, "GREGORIAN")
	if start.loc != nil && !start.allDay {
		cal.Children = append(cal.Children, VTimezone(start.loc, now))
	}
	cal.Children = append(cal.Children, comp)
	return Encode(cal)
}

// Encode is the canonical encoding: go-ical's encoder (sorted property
// names, children in order, 75-octet folding, CRLF).
func Encode(cal *ical.Calendar) ([]byte, error) {
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, fmt.Errorf("ical: %w", err)
	}
	return buf.Bytes(), nil
}

// Decode parses iCalendar bytes.
func Decode(b []byte) (*ical.Calendar, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(b)).Decode()
	if err != nil {
		return nil, fmt.Errorf("ical: %w", err)
	}
	return cal, nil
}

// Canonical re-encodes bytes into the canonical form.
func Canonical(b []byte) ([]byte, error) {
	cal, err := Decode(b)
	if err != nil {
		return nil, err
	}
	return Encode(cal)
}

// Master returns the object's master component: the first VEVENT without
// a RECURRENCE-ID, else the first VTODO. Overrides (VEVENTs with a
// RECURRENCE-ID) are returned separately.
func Master(cal *ical.Calendar) (master *ical.Component, overrides []*ical.Component, err error) {
	var todo *ical.Component
	for _, ch := range cal.Children {
		switch ch.Name {
		case ical.CompEvent:
			if ch.Props.Get(ical.PropRecurrenceID) != nil {
				overrides = append(overrides, ch)
				continue
			}
			if master == nil {
				master = ch
			}
		case ical.CompToDo:
			if todo == nil {
				todo = ch
			}
		}
	}
	if master == nil {
		master = todo
	}
	if master == nil {
		return nil, nil, errors.New("ical: no VEVENT (or VTODO) in the calendar")
	}
	return master, overrides, nil
}

// Parse reads the facts of a calendar object: the master component's
// Event with every derived field filled in. The bytes need not be canonical.
func Parse(b []byte) (Event, error) {
	cal, err := Decode(b)
	if err != nil {
		return Event{}, err
	}
	return ParseCalendar(cal)
}

// ParseCalendar is Parse over a decoded calendar.
func ParseCalendar(cal *ical.Calendar) (Event, error) {
	comp, _, err := Master(cal)
	if err != nil {
		return Event{}, err
	}
	ev := Event{Component: comp.Name}
	text := func(name string) string {
		if p := comp.Props.Get(name); p != nil {
			s, err := p.Text()
			if err != nil {
				return p.Value
			}
			return s
		}
		return ""
	}
	ev.UID = text(ical.PropUID)
	if ev.UID == "" {
		return Event{}, errors.New("ical: the component has no UID")
	}
	ev.Summary = text(ical.PropSummary)
	ev.Description = text(ical.PropDescription)
	ev.Location = text(ical.PropLocation)
	ev.Status = strings.ToUpper(text(ical.PropStatus))
	if p := comp.Props.Get(ical.PropURL); p != nil {
		ev.URL = p.Value
	}
	if p := comp.Props.Get(ical.PropSequence); p != nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(p.Value), 10, 64); err == nil && n >= 0 {
			ev.Sequence = n
		}
	}
	if p := comp.Props.Get(ical.PropDateTimeStamp); p != nil {
		if t, err := p.DateTime(time.UTC); err == nil {
			ev.DTStamp = t.UTC().Format(time.RFC3339)
		}
	}

	var start when
	sp := comp.Props.Get(ical.PropDateTimeStart)
	if sp == nil {
		if comp.Name == ical.CompEvent {
			return Event{}, errors.New("ical: VEVENT has no DTSTART")
		}
	} else {
		start, err = whenOf(sp)
		if err != nil {
			return Event{}, fmt.Errorf("ical: DTSTART: %w", err)
		}
		ev.AllDay = start.allDay
		ev.Start, ev.TZID = formatWhen(start)
		ev.StartUTC = start.t.UTC().Format(time.RFC3339)
	}
	if ep := comp.Props.Get(ical.PropDateTimeEnd); ep != nil {
		end, err := whenOf(ep)
		if err != nil {
			return Event{}, fmt.Errorf("ical: DTEND: %w", err)
		}
		ev.End, _ = formatWhen(end)
		ev.EndUTC = end.t.UTC().Format(time.RFC3339)
	} else if dp := comp.Props.Get(ical.PropDuration); dp != nil {
		d, err := dp.Duration()
		if err != nil {
			return Event{}, fmt.Errorf("ical: DURATION: %w", err)
		}
		ev.Duration = strings.ToUpper(dp.Value)
		if sp != nil {
			ev.EndUTC = start.t.Add(d).UTC().Format(time.RFC3339)
		}
	} else if sp != nil {
		// RFC 5545 §3.6.1: a DATE start lasts one day, a DATE-TIME start
		// ends at its own instant.
		if start.allDay {
			ev.EndUTC = start.t.AddDate(0, 0, 1).UTC().Format(time.RFC3339)
		} else {
			ev.EndUTC = ev.StartUTC
		}
	}
	if rp := comp.Props.Get(ical.PropRecurrenceRule); rp != nil {
		ev.RRule = strings.ToUpper(rp.Value)
		ev.Recurs = true
		if ro, err := comp.Props.RecurrenceRule(); err == nil && ro != nil {
			ev.Recur = recurOf(ro)
		}
	}
	if comp.Props.Get(ical.PropRecurrenceDates) != nil {
		ev.Recurs = true
	}
	for _, x := range comp.Props.Values(ical.PropExceptionDates) {
		if t, err := x.DateTime(time.UTC); err == nil {
			ev.ExDate = append(ev.ExDate, t.UTC().Format(time.RFC3339))
		}
	}
	return ev, nil
}

func whenOf(p *ical.Prop) (when, error) {
	isDate := p.ValueType() == ical.ValueDate || (p.ValueType() == ical.ValueDefault && len(p.Value) == 8)
	t, err := p.DateTime(time.UTC)
	if err != nil {
		return when{}, err
	}
	if isDate {
		return when{t: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), allDay: true}, nil
	}
	if tzid := p.Params.Get(ical.ParamTimezoneID); tzid != "" && t.Location() != time.UTC {
		return when{t: t, loc: t.Location()}, nil
	}
	return when{t: t.UTC()}, nil
}

func formatWhen(w when) (value, tzid string) {
	switch {
	case w.allDay:
		return w.t.Format(dateForm), ""
	case w.loc != nil:
		return w.t.In(w.loc).Format(localForm), w.loc.String()
	default:
		return w.t.UTC().Format(time.RFC3339), ""
	}
}

func recurOf(ro *rrule.ROption) *Recur {
	r := &Recur{Freq: ro.Freq.String(), Interval: ro.Interval, Count: ro.Count,
		ByDay: []string{}, ByMonthDay: []int{}, ByMonth: []int{}, ByHour: []int{}, ByMinute: []int{}, BySetPos: []int{}}
	if r.Interval == 0 {
		r.Interval = 1
	}
	for _, d := range ro.Byweekday {
		r.ByDay = append(r.ByDay, d.String())
	}
	r.ByMonthDay = append(r.ByMonthDay, ro.Bymonthday...)
	r.ByMonth = append(r.ByMonth, ro.Bymonth...)
	r.ByHour = append(r.ByHour, ro.Byhour...)
	r.ByMinute = append(r.ByMinute, ro.Byminute...)
	r.BySetPos = append(r.BySetPos, ro.Bysetpos...)
	if !ro.Until.IsZero() {
		r.Until = ro.Until.UTC().Format(time.RFC3339)
	}
	return r
}

// FeedBytes renders an ICS subscription feed: one VCALENDAR carrying every
// live object's components, VTIMEZONEs de-duplicated by TZID.
func FeedBytes(name, description string, objs []Object) ([]byte, error) {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, ProdID)
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropCalendarScale, "GREGORIAN")
	if name != "" {
		cal.Props.Set(&ical.Prop{Name: "X-WR-CALNAME", Params: ical.Params{}, Value: escapeText(name)})
	}
	if description != "" {
		cal.Props.Set(&ical.Prop{Name: "X-WR-CALDESC", Params: ical.Params{}, Value: escapeText(description)})
	}
	seenTZ := map[string]bool{}
	var tzs, comps []*ical.Component
	for _, o := range objs {
		c, err := Decode(o.ICal)
		if err != nil {
			continue // a corrupt row must not take the feed down
		}
		for _, ch := range c.Children {
			if ch.Name == ical.CompTimezone {
				id, _ := ch.Props.Text(ical.PropTimezoneID)
				if id == "" || seenTZ[id] {
					continue
				}
				seenTZ[id] = true
				tzs = append(tzs, ch)
				continue
			}
			comps = append(comps, ch)
		}
	}
	cal.Children = append(append(cal.Children, tzs...), comps...)
	if len(cal.Children) == 0 {
		// The encoder refuses an empty VCALENDAR; a feed may well be empty.
		var b strings.Builder
		b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:" + ProdID + "\r\nCALSCALE:GREGORIAN\r\n")
		if name != "" {
			b.WriteString("X-WR-CALNAME:" + escapeText(name) + "\r\n")
		}
		b.WriteString("END:VCALENDAR\r\n")
		return []byte(b.String()), nil
	}
	return Encode(cal)
}

func escapeText(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`)
	return r.Replace(s)
}
