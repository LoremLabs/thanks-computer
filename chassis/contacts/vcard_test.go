package contacts

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-vcard"
)

var stamp = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func TestRenderParseRoundTrip(t *testing.T) {
	c := Card{UID: "bob-example.com.paris@example.com", FN: "Bob Example",
		Name: &Name{Family: "Example", Given: "Bob"}, Org: "Acme; Inc", Title: "Ops",
		Emails: []Typed{{Value: "Bob@Example.com", Type: []string{"home"}, Pref: true}, {Value: "bob@work.example", Type: []string{"work"}}},
		Phones: []Typed{{Value: "+1 555 0100", Type: []string{"cell"}}},
		URL:    "https://example.com/bob", Birthday: "1980-04-15", Note: "Line one\nLine two, with; punctuation"}
	b, err := Render(c, stamp)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:" + ProdID + "\r\n", "UID:bob-example.com.paris@example.com\r\n",
		"N:Example;Bob;;;\r\n", "FN:Bob Example\r\n", "ORG:Acme\\; Inc\r\n", "TITLE:Ops\r\n",
		"EMAIL;TYPE=INTERNET,HOME,PREF:Bob@Example.com\r\n", "EMAIL;TYPE=INTERNET,WORK:bob@work.example\r\n",
		"TEL;TYPE=CELL:+1 555 0100\r\n", "URL:https://example.com/bob\r\n", "BDAY:1980-04-15\r\n",
		"NOTE:Line one\\nLine two\\, with\\; punctuation\r\n", "REV:20260905T120000Z\r\nEND:VCARD\r\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("render lacks %q:\n%s", want, s)
		}
	}
	p, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != c.UID || p.FN != "Bob Example" || p.Name == nil || p.Name.Family != "Example" || p.Name.Given != "Bob" ||
		p.Org != "Acme; Inc" || p.Title != "Ops" || p.URL != c.URL || p.Birthday != "1980-04-15" ||
		p.Note != "Line one\nLine two, with; punctuation" || p.Version != "3.0" || p.Rev != "2026-09-05T12:00:00Z" ||
		p.Kind != "individual" || len(p.Emails) != 2 || p.Emails[0].Value != "Bob@Example.com" || !p.Emails[0].Pref ||
		strings.Join(p.Emails[0].Type, ",") != "internet,home" || p.Emails[1].Pref ||
		len(p.Phones) != 1 || p.Phones[0].Value != "+1 555 0100" || strings.Join(p.Phones[0].Type, ",") != "cell" ||
		strings.Join(p.Addresses, ",") != "bob@example.com,bob@work.example" {
		t.Errorf("parsed = %+v (name %+v)", p, p.Name)
	}
	// Re-render the parsed card an hour later: same content (REV aside) and
	// byte-identical apart from the REV line.
	b2, err := Render(p, stamp.Add(time.Hour))
	if err != nil || !SameContent(b, b2) {
		t.Errorf("re-render not same content (%v):\n%s\n---\n%s", err, b, b2)
	}
	if ETagOf(b) == ETagOf(b2) {
		t.Error("a new REV must change the bytes (the store's no-op rule is what ignores it)")
	}
	// Normalize is a fixed point on rendered bytes.
	if string(Normalize(b)) != s {
		t.Error("Normalize changed rendered bytes")
	}
}

func TestRenderDefaultsAndErrors(t *testing.T) {
	b, err := Render(Card{UID: "a@x", Emails: []Typed{{Value: "ann@example.com"}}}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "FN:ann@example.com\r\n") || !strings.Contains(string(b), "N:ann@example.com;;;;\r\n") ||
		!strings.Contains(string(b), "EMAIL;TYPE=INTERNET:ann@example.com\r\n") {
		t.Errorf("defaults:\n%s", b)
	}
	g, err := Render(Card{UID: "g@x", FN: "Team", Kind: "group", Members: []string{"urn:uuid:1", "urn:uuid:2"}}, stamp)
	if err != nil || !strings.Contains(string(g), "X-ADDRESSBOOKSERVER-KIND:group\r\n") || strings.Count(string(g), "X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:") != 2 {
		t.Errorf("group (%v):\n%s", err, g)
	}
	if p, _ := Parse(g); p.Kind != "group" || len(p.Members) != 2 || len(p.Addresses) != 0 {
		t.Errorf("group parsed = %+v", p)
	}
	for name, c := range map[string]Card{
		"no uid":    {FN: "x"},
		"no fn":     {UID: "a"},
		"bad email": {UID: "a", Emails: []Typed{{Value: "not an email"}}},
		"bad url":   {UID: "a", FN: "x", URL: "nope"},
		"bad bday":  {UID: "a", FN: "x", Birthday: "April 15"},
		"bad kind":  {UID: "a", FN: "x", Kind: "robot"},
		"bad phone": {UID: "a", FN: "x", Phones: []Typed{{Value: " "}}},
	} {
		if _, err := Render(c, stamp); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFoldRuneSafe(t *testing.T) {
	long := strings.Repeat("é", 60) // 120 octets
	b, err := Render(Card{UID: "f@x", FN: "F", Note: long}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\r\n"), "\r\n") {
		if len(line) > 75 {
			t.Errorf("line over 75 octets: %d", len(line))
		}
	}
	p, err := Parse(b)
	if err != nil || p.Note != long {
		t.Errorf("folded note did not round-trip (%v): %q", err, p.Note)
	}
}

// appleCard is the shape macOS Contacts sends: 3.0, item groups, Apple's
// TYPE=pref idiom, a PHOTO, X-AB props, and an escaped `\;` in a NOTE.
const appleCard = "BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:-//Apple Inc.//macOS 15.0//EN\r\nN:Example;Bob;;;\r\nFN:Bob Example\r\n" +
	"ORG:Acme;\r\nitem1.EMAIL;type=INTERNET;type=pref:Bob@Example.com\r\nitem1.X-ABLabel:_$!<Other>!$_\r\n" +
	"EMAIL;type=INTERNET;type=WORK:bob@work.example\r\nTEL;type=CELL;type=VOICE;type=pref:+1 (555) 010-0000\r\n" +
	"NOTE:Call first\\; then email\\, please.\r\nPHOTO;ENCODING=b;TYPE=JPEG:/9j/4AAQSkZJRgABAQAAAQABAAD/2wBDAAgGBgcGBQgHBwcJCQgK\r\n DBQNDAsLDBkSEw8UHRofHh0aHBwgJC4nICIsIxwcKDcpLDAxNDQ0Hyc5PTgyPC4zNDL/2wBDAQkJ\r\n" +
	"X-ABUID:8F2C1A34-1111-4C2B-9E5D-ABCDEF012345:ABPerson\r\nUID:8F2C1A34-1111-4C2B-9E5D-ABCDEF012345\r\nREV:2026-09-05T10:00:00Z\r\nEND:VCARD\r\n"

func TestParseAppleCardAndNormalize(t *testing.T) {
	p, err := Parse([]byte(appleCard))
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != "8F2C1A34-1111-4C2B-9E5D-ABCDEF012345" || p.FN != "Bob Example" || p.Org != "Acme" ||
		len(p.Emails) != 2 || !p.Emails[0].Pref || p.Emails[0].Value != "Bob@Example.com" || strings.Join(p.Emails[0].Type, ",") != "internet" ||
		strings.Join(p.Addresses, ",") != "bob@example.com,bob@work.example" ||
		len(p.Phones) != 1 || !p.Phones[0].Pref || strings.Join(p.Phones[0].Type, ",") != "cell,voice" ||
		p.Note != "Call first; then email, please." || p.Rev != "2026-09-05T10:00:00Z" || p.Version != "3.0" || p.Kind != "individual" {
		t.Errorf("parsed = %+v", p)
	}
	// Normalize keeps the client's bytes (CRLF in, CRLF out) byte for byte.
	if string(Normalize([]byte(appleCard))) != appleCard {
		t.Error("Normalize altered CRLF bytes")
	}
	lf := strings.ReplaceAll(appleCard, "\r\n", "\n")
	if string(Normalize([]byte("\ufeff"+lf))) != appleCard {
		t.Error("Normalize did not restore CRLF / drop the BOM")
	}
	// The library view: values unescaped once, params kept, groups kept.
	lib, err := ToLibrary([]byte(appleCard))
	if err != nil {
		t.Fatal(err)
	}
	if lib.Value(vcard.FieldNote) != "Call first; then email, please." {
		t.Errorf("library NOTE = %q", lib.Value(vcard.FieldNote))
	}
	em := lib.Get(vcard.FieldEmail)
	if em == nil || em.Group != "item1" || !em.Params.HasType("pref") || lib.PreferredValue(vcard.FieldEmail) != "Bob@Example.com" {
		t.Errorf("library EMAIL = %+v", em)
	}
	if lib.Value(vcard.FieldVersion) != "3.0" || lib.Get("X-ABUID") == nil {
		t.Error("library card lost VERSION or an X- property")
	}
	// Apple's group card: a KIND with no EMAIL.
	grp := "BEGIN:VCARD\r\nVERSION:3.0\r\nN:Team;;;;\r\nFN:Team\r\nX-ADDRESSBOOKSERVER-KIND:group\r\n" +
		"X-ADDRESSBOOKSERVER-MEMBER:urn:uuid:8F2C1A34-1111-4C2B-9E5D-ABCDEF012345\r\nUID:GROUP-1\r\nEND:VCARD\r\n"
	if g, err := Parse([]byte(grp)); err != nil || g.Kind != "group" || len(g.Members) != 1 || len(g.Addresses) != 0 {
		t.Errorf("group = %+v err=%v", g, err)
	}
}

func TestParseRefusals(t *testing.T) {
	for name, raw := range map[string]string{
		"garbage":    "not a vcard",
		"no version": "BEGIN:VCARD\r\nUID:a\r\nFN:x\r\nEND:VCARD\r\n",
		"no uid":     "BEGIN:VCARD\r\nVERSION:3.0\r\nFN:x\r\nEND:VCARD\r\n",
		"two cards":  "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:a\r\nEND:VCARD\r\nBEGIN:VCARD\r\nVERSION:3.0\r\nUID:b\r\nEND:VCARD\r\n",
		"no end":     "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:a\r\n",
	} {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	_, err := Parse([]byte("BEGIN:VCARD\r\nVERSION:2.1\r\nUID:a\r\nN:x\r\nEND:VCARD\r\n"))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("2.1 err = %v", err)
	}
	// 4.0 is fine, with KIND and PREF=1 and a quoted param.
	p, err := Parse([]byte("BEGIN:VCARD\nVERSION:4.0\nUID:urn:uuid:1\nFN:Ann\nKIND:individual\nEMAIL;PREF=1;TYPE=\"home,work\":ann@x.io\nEND:VCARD\n"))
	if err != nil || p.Version != "4.0" || len(p.Emails) != 1 || !p.Emails[0].Pref || strings.Join(p.Emails[0].Type, ",") != "home,work" || p.Addresses[0] != "ann@x.io" {
		t.Errorf("4.0 parsed = %+v err=%v", p, err)
	}
}

func TestDefaultNames(t *testing.T) {
	if got := DefaultObjectName("bob-example.com.paris@example.com"); got != "bob-example.com.paris.vcf" {
		t.Errorf("DefaultObjectName = %q", got)
	}
	if got := DefaultUID("bob-example.com.vcf", "paris@example.com"); got != "bob-example.com.paris@example.com" {
		t.Errorf("DefaultUID = %q", got)
	}
	o, err := ObjectFromCard("paris@example.com", "bob-example.com.vcf", Card{Emails: []Typed{{Value: "bob@example.com"}}}, stamp)
	if err != nil || o.UID != "bob-example.com.paris@example.com" || o.Name != "bob-example.com.vcf" || o.FN != "bob@example.com" || o.Addresses[0] != "bob@example.com" {
		t.Errorf("ObjectFromCard = %+v err=%v", o, err)
	}
	if _, err := ObjectFromCard("paris@example.com", "", Card{Emails: []Typed{{Value: "bob@example.com"}}}, stamp); err == nil {
		t.Error("no uid and no name accepted")
	}
	v, err := ObjectFromVCard("", []byte(appleCard))
	if err != nil || v.Name != "8F2C1A34-1111-4C2B-9E5D-ABCDEF012345.vcf" || v.Kind != "individual" || v.Version != "3.0" {
		t.Errorf("ObjectFromVCard = %+v err=%v", v, err)
	}
	if _, err := ObjectFromVCard("bad name", []byte(appleCard)); !errors.Is(err, ErrInvalidCard) {
		t.Errorf("bad name err = %v", err)
	}
}
