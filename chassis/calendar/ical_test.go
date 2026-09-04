package calendar

import (
	"strings"
	"testing"
	"time"
)

var stamp = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

func TestRenderParseRoundTripUTC(t *testing.T) {
	ev := Event{UID: "daily-digest.paris@example.com", Summary: "Daily digest", Description: "What changed?",
		Start: "2026-01-01T09:00:00Z", Duration: "PT30M", RRule: "freq=daily", Sequence: 2}
	b, err := Render(ev, stamp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"BEGIN:VCALENDAR\r\n", "PRODID:" + ProdID, "DTSTART:20260101T090000Z\r\n", "DURATION:PT30M\r\n",
		"RRULE:FREQ=DAILY\r\n", "SEQUENCE:2\r\n", "DTSTAMP:20260904T120000Z\r\n", "SUMMARY:Daily digest\r\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("render lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "VTIMEZONE") {
		t.Error("a UTC event must not carry a VTIMEZONE")
	}
	// Canonical: re-encoding is byte-identical.
	c, err := Canonical(b)
	if err != nil || string(c) != s {
		t.Errorf("canonical differs or errored (%v)", err)
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != ev.UID || p.Summary != ev.Summary || p.Description != ev.Description || p.Start != "2026-01-01T09:00:00Z" ||
		p.StartUTC != "2026-01-01T09:00:00Z" || p.EndUTC != "2026-01-01T09:30:00Z" || p.Duration != "PT30M" || p.RRule != "FREQ=DAILY" ||
		!p.Recurs || p.Recur == nil || p.Recur.Freq != "DAILY" || p.Recur.Interval != 1 || len(p.Recur.ByDay) != 0 ||
		p.Sequence != 2 || p.DTStamp != "2026-09-04T12:00:00Z" || p.AllDay || p.TZID != "" || p.Component != "VEVENT" {
		t.Errorf("parsed = %+v", p)
	}
	// Re-render the parsed event: same content (DTSTAMP aside).
	b2, err := Render(p, stamp.Add(time.Hour))
	if err != nil || !SameContent(b, b2) {
		t.Errorf("re-render not same content (%v):\n%s\n---\n%s", err, b, b2)
	}
}

func TestRenderParisWithTimezone(t *testing.T) {
	ev := Event{UID: "x@y", Summary: "Morning", Start: "2026-03-28T08:00:00", TZID: "Europe/Paris", End: "2026-03-28T09:00:00", RRule: "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR"}
	b, err := Render(ev, stamp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"BEGIN:VTIMEZONE\r\nTZID:Europe/Paris\r\n", "DTSTART;TZID=Europe/Paris:20260328T080000\r\n",
		"DTEND;TZID=Europe/Paris:20260328T090000\r\n", "BEGIN:DAYLIGHT", "BEGIN:STANDARD", "TZOFFSETFROM:+0100\r\nTZOFFSETTO:+0200"} {
		if !strings.Contains(s, want) {
			t.Errorf("render lacks %q:\n%s", want, s)
		}
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	// 08:00 Paris on 28 March 2026 (CET, before the 29 March switch) is 07:00Z.
	if p.TZID != "Europe/Paris" || p.Start != "2026-03-28T08:00:00" || p.StartUTC != "2026-03-28T07:00:00Z" || p.EndUTC != "2026-03-28T08:00:00Z" ||
		p.Recur == nil || p.Recur.Freq != "WEEKLY" || strings.Join(p.Recur.ByDay, ",") != "MO,TU,WE,TH,FR" {
		t.Errorf("parsed = %+v", p)
	}
	// The other side of the DST switch: 08:00 Paris on 30 March is 06:00Z.
	ev2 := Event{UID: "x@y", Start: "2026-03-30T08:00:00", TZID: "Europe/Paris"}
	b2, _ := Render(ev2, stamp)
	p2, _ := Parse(b2)
	if p2.StartUTC != "2026-03-30T06:00:00Z" || p2.EndUTC != "2026-03-30T07:00:00Z" {
		t.Errorf("after DST: %+v", p2)
	}
	// Same rendering a day later (same year) is the same content.
	b3, _ := Render(ev, stamp.Add(24*time.Hour))
	if !SameContent(b, b3) {
		t.Error("VTIMEZONE must be stable within a year")
	}
}

func TestRenderAllDayAndErrors(t *testing.T) {
	b, err := Render(Event{UID: "d@y", Summary: "Day", Start: "2026-09-10"}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "DTSTART;VALUE=DATE:20260910\r\n") || !strings.Contains(string(b), "DTEND;VALUE=DATE:20260911\r\n") {
		t.Errorf("all-day render:\n%s", b)
	}
	p, _ := Parse(b)
	if !p.AllDay || p.Start != "2026-09-10" || p.End != "2026-09-11" || p.StartUTC != "2026-09-10T00:00:00Z" || p.EndUTC != "2026-09-11T00:00:00Z" {
		t.Errorf("all-day parsed = %+v", p)
	}
	for name, ev := range map[string]Event{
		"no uid":      {Start: "2026-09-10"},
		"no start":    {UID: "a"},
		"floating":    {UID: "a", Start: "2026-09-10T09:00:00"},
		"bad tz":      {UID: "a", Start: "2026-09-10T09:00:00", TZID: "Mars/Olympus"},
		"end+dur":     {UID: "a", Start: "2026-09-10T09:00:00Z", End: "2026-09-10T10:00:00Z", Duration: "PT1H"},
		"end<start":   {UID: "a", Start: "2026-09-10T09:00:00Z", End: "2026-09-10T08:00:00Z"},
		"mixed kinds": {UID: "a", Start: "2026-09-10", End: "2026-09-10T10:00:00Z"},
		"bad rrule":   {UID: "a", Start: "2026-09-10T09:00:00Z", RRule: "FREQ=SOMETIMES"},
		"bad status":  {UID: "a", Start: "2026-09-10T09:00:00Z", Status: "maybe"},
		"bad dur":     {UID: "a", Start: "2026-09-10T09:00:00Z", Duration: "30 minutes"},
		"bad url":     {UID: "a", Start: "2026-09-10T09:00:00Z", URL: "not a url"},
	} {
		if _, err := Render(ev, stamp); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// Offset times are stored as UTC; status and url survive.
	b, err = Render(Event{UID: "o@y", Start: "2026-09-10T11:00:00+02:00", Status: "tentative", URL: "https://example.com/x"}, stamp)
	if err != nil || !strings.Contains(string(b), "DTSTART:20260910T090000Z\r\n") || !strings.Contains(string(b), "STATUS:TENTATIVE\r\n") || !strings.Contains(string(b), "URL:https://example.com/x\r\n") {
		t.Errorf("offset render (%v):\n%s", err, b)
	}
}

func TestParseClientBytes(t *testing.T) {
	// The shape Apple Calendar sends: VTIMEZONE first, an RRULE, an EXDATE, a
	// RECURRENCE-ID override, and no SEQUENCE.
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Apple Inc.//macOS 15.0//EN\r\nCALSCALE:GREGORIAN\r\n" +
		"BEGIN:VTIMEZONE\r\nTZID:Europe/Paris\r\nBEGIN:DAYLIGHT\r\nTZOFFSETFROM:+0100\r\nRRULE:FREQ=YEARLY;BYMONTH=3;BYDAY=-1SU\r\nDTSTART:19810329T020000\r\nTZNAME:CEST\r\nTZOFFSETTO:+0200\r\nEND:DAYLIGHT\r\n" +
		"BEGIN:STANDARD\r\nTZOFFSETFROM:+0200\r\nRRULE:FREQ=YEARLY;BYMONTH=10;BYDAY=-1SU\r\nDTSTART:19961027T030000\r\nTZNAME:CET\r\nTZOFFSETTO:+0100\r\nEND:STANDARD\r\nEND:VTIMEZONE\r\n" +
		"BEGIN:VEVENT\r\nCREATED:20260904T100000Z\r\nUID:8F2C1A34-1111-4C2B-9E5D-ABCDEF012345\r\nDTEND;TZID=Europe/Paris:20260905T093000\r\n" +
		"TRANSP:OPAQUE\r\nSUMMARY:Daily market scan\r\nDTSTART;TZID=Europe/Paris:20260905T090000\r\nDTSTAMP:20260904T100100Z\r\nRRULE:FREQ=DAILY;INTERVAL=1\r\n" +
		"EXDATE;TZID=Europe/Paris:20260906T090000\r\nDESCRIPTION:Scan the markets.\r\nEND:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:8F2C1A34-1111-4C2B-9E5D-ABCDEF012345\r\nRECURRENCE-ID;TZID=Europe/Paris:20260907T090000\r\nDTSTART;TZID=Europe/Paris:20260907T100000\r\nDTEND;TZID=Europe/Paris:20260907T103000\r\nDTSTAMP:20260904T100200Z\r\nSUMMARY:Daily market scan (moved)\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"
	p, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != "8F2C1A34-1111-4C2B-9E5D-ABCDEF012345" || p.Summary != "Daily market scan" || p.Description != "Scan the markets." ||
		p.TZID != "Europe/Paris" || p.Start != "2026-09-05T09:00:00" || p.StartUTC != "2026-09-05T07:00:00Z" || p.EndUTC != "2026-09-05T07:30:00Z" ||
		p.RRule != "FREQ=DAILY;INTERVAL=1" || p.Recur == nil || p.Recur.Freq != "DAILY" || len(p.ExDate) != 1 || p.ExDate[0] != "2026-09-06T07:00:00Z" ||
		p.Sequence != 0 || !p.Recurs {
		t.Errorf("parsed = %+v", p)
	}
	c, err := Canonical([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := Canonical(c)
	if string(c) != string(c2) {
		t.Error("canonical form is not a fixed point")
	}
	if _, err := Parse([]byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VJOURNAL\r\nUID:j\r\nDTSTAMP:20260904T100000Z\r\nEND:VJOURNAL\r\nEND:VCALENDAR\r\n")); err == nil {
		t.Error("a calendar without VEVENT/VTODO must not parse")
	}
	if _, err := Parse([]byte("not ical")); err == nil {
		t.Error("garbage must not parse")
	}
}

func TestFeedBytes(t *testing.T) {
	b, err := FeedBytes("Paris schedule", "", nil)
	if err != nil || !strings.Contains(string(b), "X-WR-CALNAME:Paris schedule\r\n") || !strings.Contains(string(b), "END:VCALENDAR\r\n") {
		t.Errorf("empty feed (%v):\n%s", err, b)
	}
	a, _ := Render(Event{UID: "a@x", Summary: "A", Start: "2026-09-10T09:00:00", TZID: "Europe/Paris"}, stamp)
	c, _ := Render(Event{UID: "b@x", Summary: "B; c, d", Start: "2026-09-11T09:00:00", TZID: "Europe/Paris"}, stamp)
	b, err = FeedBytes("Paris, schedule", "desc", []Object{{ICal: a}, {ICal: c}, {ICal: []byte("corrupt")}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if strings.Count(s, "BEGIN:VTIMEZONE") != 1 || strings.Count(s, "BEGIN:VEVENT") != 2 || !strings.Contains(s, "X-WR-CALNAME:Paris\\, schedule\r\n") || !strings.Contains(s, "SUMMARY:B\\; c\\, d\r\n") {
		t.Errorf("feed:\n%s", s)
	}
}

func TestVTimezoneCoversWindow(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	tz := VTimezone(loc, stamp)
	if id, _ := tz.Props.Text("TZID"); id != "America/New_York" {
		t.Errorf("tzid = %q", id)
	}
	// 2025..2028: an initial STANDARD plus two transitions a year.
	var std, dst int
	for _, c := range tz.Children {
		switch c.Name {
		case "STANDARD":
			std++
		case "DAYLIGHT":
			dst++
		}
	}
	if dst != 4 || std != 5 {
		t.Errorf("observances: standard=%d daylight=%d", std, dst)
	}
	utc := VTimezone(time.UTC, stamp)
	if len(utc.Children) != 1 || utc.Children[0].Name != "STANDARD" {
		t.Errorf("UTC observances = %d", len(utc.Children))
	}
}
