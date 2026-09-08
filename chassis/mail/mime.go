package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/jhillyerd/enmime/v2"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/hxid"
)

// calendarPart is `_sendmail.calendar = {method, ical}`: an iTIP payload the
// message carries TWICE — as a `text/calendar; method=<M>` part inside the
// alternative (what Gmail, Outlook and Apple Mail read to show Accept /
// Decline) and as an `application/ics` attachment named invite.ics (what a
// client without native iTIP hands to a calendar app). That is the shape
// Google Calendar itself sends. The chassis renders nothing here: the caller
// authored the iCalendar text, ORGANIZER and ATTENDEE lines included.
type calendarPart struct {
	method string
	ical   []byte
}

// attachment is one entry of `_sendmail.attachments[]`:
// {filename, content_type, content_b64}. The sanctioned way to add a MIME
// part — `content-type` stays a denylisted HEADER on purpose.
type attachment struct {
	name  string
	ctype string
	data  []byte
}

type mimeExtras struct {
	calendar    *calendarPart
	attachments []attachment
}

const (
	maxAttachments    = 5
	maxAttachmentSize = 1 << 20 // 1 MiB each; the ical text shares the cap
)

var calendarMethods = map[string]bool{"REQUEST": true, "CANCEL": true, "REPLY": true, "PUBLISH": true}

// parseCalendarPart reads `_sendmail.calendar`. Absent → nil, nil.
func parseCalendarPart(v gjson.Result) (*calendarPart, error) {
	if !v.Exists() {
		return nil, nil
	}
	method := strings.ToUpper(strings.TrimSpace(v.Get("method").String()))
	if !calendarMethods[method] {
		return nil, fmt.Errorf("calendar.method must be REQUEST, CANCEL, REPLY or PUBLISH, got %q", method)
	}
	ical := strings.TrimSpace(v.Get("ical").String())
	if !strings.HasPrefix(ical, "BEGIN:VCALENDAR") {
		return nil, errors.New("calendar.ical must be iCalendar text starting with BEGIN:VCALENDAR")
	}
	if len(ical) > maxAttachmentSize {
		return nil, fmt.Errorf("calendar.ical is %d bytes, over the %d cap", len(ical), maxAttachmentSize)
	}
	// Normalize line endings once: RFC 5545 wants CRLF, and a rule that built
	// the text with "\n" should not have to know.
	ical = strings.ReplaceAll(strings.ReplaceAll(ical, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	return &calendarPart{method: method, ical: []byte(ical)}, nil
}

// parseAttachments reads `_sendmail.attachments`. Absent → nil, nil.
func parseAttachments(v gjson.Result) ([]attachment, error) {
	if !v.Exists() {
		return nil, nil
	}
	if !v.IsArray() {
		return nil, errors.New("attachments must be an array of {filename, content_type, content_b64}")
	}
	items := v.Array()
	if len(items) > maxAttachments {
		return nil, fmt.Errorf("%d attachments exceeds the cap of %d", len(items), maxAttachments)
	}
	out := make([]attachment, 0, len(items))
	for i, it := range items {
		name := strings.TrimSpace(it.Get("filename").String())
		ctype := strings.TrimSpace(it.Get("content_type").String())
		if name == "" || ctype == "" {
			return nil, fmt.Errorf("attachments[%d]: filename and content_type are required", i)
		}
		if strings.ContainsAny(name, "\r\n\"/\\") || len(name) > 200 {
			return nil, fmt.Errorf("attachments[%d]: filename %q is not a plain file name", i, name)
		}
		if strings.ContainsAny(ctype, "\r\n;") || !strings.Contains(ctype, "/") {
			return nil, fmt.Errorf("attachments[%d]: content_type %q is not a bare media type", i, ctype)
		}
		data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(it.Get("content_b64").String()))
		if err != nil {
			return nil, fmt.Errorf("attachments[%d]: content_b64 is not base64: %v", i, err)
		}
		if len(data) == 0 {
			return nil, fmt.Errorf("attachments[%d]: content_b64 is empty", i)
		}
		if len(data) > maxAttachmentSize {
			return nil, fmt.Errorf("attachments[%d]: %d bytes, over the %d cap", i, len(data), maxAttachmentSize)
		}
		out = append(out, attachment{name: name, ctype: ctype, data: data})
	}
	return out, nil
}

// composeMIME builds a multipart (text + html) RFC 5322 message and returns
// the encoded bytes plus a generated Message-ID. from/to are parsed
// addresses (display name + bare address). enmime does not auto-add a
// Message-ID, so the one set here is the only one; Postfix stamps Date and
// (if absent) a Received-derived id. msgIDDomain is the From domain.
//
// With extras the tree grows one level: multipart/mixed { multipart/alternative
// { text, html [, text/calendar] } [, application/ics] [, attachments…] }.
func composeMIME(from, to mail.Address, cc []mail.Address, replyTo string, extraHeaders map[string]string, subject, htmlBody, textBody, msgIDDomain string, extras mimeExtras) (msg []byte, messageID string, err error) {
	messageID = "<" + hxid.New().String() + "@" + msgIDDomain + ">"
	b := enmime.Builder().
		From(from.Name, from.Address).
		To(to.Name, to.Address).
		Subject(subject).
		Text([]byte(textBody)).
		HTML([]byte(htmlBody)).
		Header("Message-ID", messageID).
		Header("Auto-Submitted", "auto-generated").
		Header("X-Auto-Response-Suppress", "OOF, AutoReply")
	for _, a := range cc {
		b = b.CC(a.Name, a.Address) // visible Cc header (Bcc is envelope-only, never a header)
	}
	if replyTo != "" {
		b = b.Header("Reply-To", replyTo)
	}
	for k, v := range extraHeaders { // caller pre-filters protected/structural headers
		b = b.Header(k, v)
	}
	if extras.calendar != nil {
		b = b.AddAttachment(extras.calendar.ical, "application/ics", "invite.ics")
	}
	for _, a := range extras.attachments {
		b = b.AddAttachment(a.data, a.ctype, a.name)
	}
	part, berr := b.Build()
	if berr != nil {
		return nil, "", berr
	}
	if extras.calendar != nil {
		// The builder has no "alternative" slot, so the text/calendar part is
		// appended to the alternative container after the build. A client
		// picks the alternative it understands best; calendar-aware ones take
		// this one and show Accept / Decline.
		alt := part
		if !strings.EqualFold(alt.ContentType, "multipart/alternative") {
			alt = part.BreadthMatchFirst(func(p *enmime.Part) bool {
				return strings.EqualFold(p.ContentType, "multipart/alternative")
			})
		}
		if alt != nil {
			cal := enmime.NewPart("text/calendar")
			cal.ContentTypeParams["method"] = extras.calendar.method
			cal.Content = extras.calendar.ical
			alt.AddChild(cal)
		}
	}
	// Force quoted-printable on every text leaf. enmime's default picks 7bit
	// for pure-ASCII content regardless of line length, and drip HTML is one
	// long line — the MTA folds it at ~998 chars, which corrupts any attribute
	// the fold lands in and breaks the DKIM body hash. QP wraps at 76 chars
	// and decodes byte-exact. The root part gets no encoder so top-level
	// headers keep their default encoding.
	forceQP(part, enmime.NewEncoder(enmime.ForceQuotedPrintableCte(true)))
	var buf bytes.Buffer
	if eerr := part.Encode(&buf); eerr != nil {
		return nil, "", eerr
	}
	return buf.Bytes(), messageID, nil
}

// forceQP applies the encoder to every text/* leaf at any depth. The loop
// this replaced touched only the root's children; once an attachment turns
// the root into multipart/mixed, the text and HTML leaves sit one level
// deeper and would silently lose QP — the exact fold-breaks-DKIM failure the
// encoder exists to prevent. Binary attachments keep enmime's default
// (base64).
func forceQP(p *enmime.Part, enc *enmime.Encoder) {
	if p == nil {
		return
	}
	if p.FirstChild == nil {
		if strings.HasPrefix(strings.ToLower(p.ContentType), "text/") {
			p.WithEncoder(enc)
		}
		return
	}
	for c := p.FirstChild; c != nil; c = c.NextSibling {
		forceQP(c, enc)
	}
}
