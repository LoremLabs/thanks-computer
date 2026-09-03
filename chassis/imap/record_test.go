package imap

import (
	"bytes"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func sample() *Record {
	return &Record{
		Headers: map[string]string{
			"from":         "Paris the Pony <paris@example.com>",
			"To":           "Owner <owner@example.com>",
			"subject":      "Hello — welcome",
			"content-type": "text/evil", // structural: dropped
		},
		Text:  "Hi there,\r\nthis is your pony.\n",
		HTML:  "<p>Hi there,<br>this is your <b>pony</b>.</p>",
		Parts: []PartRef{{Name: "notes.pdf", Type: "application/pdf", Size: 1234}},
	}
}

func TestCanonicalIsDeterministicAndNormalised(t *testing.T) {
	a := sample()
	b := sample()
	b.Headers["FROM"] = b.Headers["from"] // same header, different spelling
	delete(b.Headers, "from")
	ca, sa, err := a.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	cb, sb, _ := b.Canonical()
	if !bytes.Equal(ca, cb) || sa != sb {
		t.Errorf("canonical differs:\n%s\n%s", ca, cb)
	}
	if strings.Contains(string(ca), "Content-Type") || strings.Contains(string(ca), "text/evil") {
		t.Errorf("structural header survived: %s", ca)
	}
	if a.Version != FormatVersion || a.Parts[0].N != 1 {
		t.Errorf("normalise: v=%d n=%d", a.Version, a.Parts[0].N)
	}
	if _, _, err := (&Record{}).Canonical(); err == nil {
		t.Error("empty record must fail")
	}
	back, err := ParseRecord(ca)
	if err != nil || back.Headers["From"] != a.Headers["From"] || back.Text != a.Text {
		t.Errorf("round trip: %+v err=%v", back, err)
	}
}

func TestRenderIsDeterministicAndParses(t *testing.T) {
	rec := sample()
	_, sha, _ := rec.Canonical()
	opt := RenderOptions{ObjectKey: "msg:1", Domain: "example.com", InternalDate: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)}
	out1, err := Render(rec, sha, opt)
	if err != nil {
		t.Fatal(err)
	}
	out2, _ := Render(rec, sha, opt)
	if !bytes.Equal(out1, out2) {
		t.Error("render not deterministic")
	}
	// Every line ends in CRLF and no bare LF exists.
	if bytes.Contains(bytes.ReplaceAll(out1, []byte("\r\n"), nil), []byte("\n")) {
		t.Errorf("bare LF in output:\n%q", out1)
	}
	m, err := mail.ReadMessage(bytes.NewReader(out1))
	if err != nil {
		t.Fatalf("not parseable: %v\n%s", err, out1)
	}
	if got := m.Header.Get("Message-ID"); !strings.HasPrefix(got, "<txco-") || !strings.HasSuffix(got, "@example.com>") {
		t.Errorf("Message-ID = %q", got)
	}
	if got := m.Header.Get("Date"); got != "Thu, 03 Sep 2026 12:00:00 +0000" {
		t.Errorf("Date = %q", got)
	}
	if got := m.Header.Get("Subject"); !strings.HasPrefix(got, "=?utf-8?q?") {
		t.Errorf("non-ASCII subject not encoded: %q", got)
	}
	if got := m.Header.Get("From"); got != `"Paris the Pony" <paris@example.com>` {
		t.Errorf("From = %q", got)
	}
	if !strings.Contains(m.Header.Get("Content-Type"), `multipart/mixed; boundary="=_txco_`+sha[:24]+`_mix"`) {
		t.Errorf("Content-Type = %q", m.Header.Get("Content-Type"))
	}
	s := string(out1)
	// Header order is fixed: Date, From, To, Subject, Message-ID.
	if !(strings.Index(s, "Date:") < strings.Index(s, "From:") && strings.Index(s, "From:") < strings.Index(s, "To:") &&
		strings.Index(s, "To:") < strings.Index(s, "Subject:") && strings.Index(s, "Subject:") < strings.Index(s, "Message-ID:")) {
		t.Errorf("header order:\n%s", s)
	}
	if !strings.Contains(s, "multipart/alternative") || !strings.Contains(s, "text/html") {
		t.Error("alternative part missing")
	}
	// The stub is quoted-printable (soft-wrapped at 76 columns), so check
	// the decoded text rather than the wire bytes.
	if !strings.Contains(s, "[Attachment 1: notes.pdf (application/pdf, 1234 bytes)") {
		t.Errorf("attachment stub missing:\n%s", s)
	}
	dec, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(s[strings.Index(s, "[Attachment 1"):])))
	if !strings.Contains(string(dec), "— not retained]") {
		t.Errorf("attachment stub decoded = %q", dec)
	}
	// Text body survives QP: decode by eye on the well-known token.
	if !strings.Contains(s, "this is your pony.") {
		t.Errorf("text body missing:\n%s", s)
	}

	// A record with a Date and Message-ID keeps them verbatim.
	rec2 := &Record{Headers: map[string]string{"Date": "Mon, 01 Jun 2026 08:00:00 +0200", "Message-ID": "<abc@x>", "Subject": "plain"}, Text: "just text"}
	_, sha2, _ := rec2.Canonical()
	out, _ := Render(rec2, sha2, RenderOptions{})
	m2, err := mail.ReadMessage(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if m2.Header.Get("Message-ID") != "<abc@x>" || m2.Header.Get("Date") != "Mon, 01 Jun 2026 08:00:00 +0200" {
		t.Errorf("headers rewritten: %v", m2.Header)
	}
	if !strings.HasPrefix(m2.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("single part expected: %q", m2.Header.Get("Content-Type"))
	}
	// No Date and no internal date is an error, never a silent now().
	if _, err := Render(rec2, sha2, RenderOptions{}); err != nil {
		t.Errorf("date present, unexpected err %v", err)
	}
	rec3 := &Record{Text: "x"}
	_, sha3, _ := rec3.Canonical()
	if _, err := Render(rec3, sha3, RenderOptions{}); err == nil {
		t.Error("missing Date and InternalDate must fail")
	}
}

func TestExcerpt(t *testing.T) {
	r := &Record{HTML: "<p>Hello   <b>world</b></p>"}
	if got := r.Excerpt(100); got != "Hello world" {
		t.Errorf("excerpt = %q", got)
	}
	r2 := &Record{Text: "héllo wörld"}
	if got := r2.Excerpt(2); got != "h" {
		t.Errorf("rune-boundary cut = %q", got)
	}
}
