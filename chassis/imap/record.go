package imap

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"sort"
	"strings"
	"time"
	"unicode"
)

// FormatVersion is the formatter version stamped on every record row at
// append. A message keeps the version it was appended under for life: the
// rendered bytes must be byte-identical on every FETCH (IMAP promises the
// client that a UID's content never changes), so a change to the renderer
// below is a NEW version for new messages, never a re-render of old ones.
const FormatVersion = 1

// Record is the canonical retained representation of a message the platform
// keeps — the thing a stack chose to remember, not the wire form it arrived
// in. RFC 5322 is derived from it by Render on FETCH.
//
// Headers is a subset the stack chose (Message-ID, Date, From, To, Cc,
// Subject, In-Reply-To, References, plus whatever else it kept); Text is the
// normalized plain body; HTML the optional rich body; Parts are attachment
// references — name/type/size always, a sha256 only where retention was
// permitted and the bytes are in the CAS.
type Record struct {
	Version int               `json:"v"`
	Headers map[string]string `json:"headers,omitempty"`
	Text    string            `json:"text,omitempty"`
	HTML    string            `json:"html,omitempty"`
	Parts   []PartRef         `json:"parts,omitempty"`
}

// PartRef is an attachment reference inside a Record.
type PartRef struct {
	N      int    `json:"n"`
	Name   string `json:"name,omitempty"`
	Type   string `json:"type,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// structuralHeaders are MIME framing headers the renderer owns. A record
// may not carry them — they would desynchronise the rendered structure from
// the cached BODYSTRUCTURE.
var structuralHeaders = map[string]bool{
	"Content-Type":              true,
	"Content-Transfer-Encoding": true,
	"Content-Disposition":       true,
	"Content-Id":                true,
	"Mime-Version":              true,
}

// addressHeaders are rendered through net/mail so display names with
// non-ASCII characters are RFC 2047-encoded per address, not as one blob.
var addressHeaders = map[string]bool{
	"From": true, "To": true, "Cc": true, "Bcc": true, "Reply-To": true, "Sender": true,
}

// headerOrder is the fixed leading order of rendered headers; anything else
// follows sorted by name. Fixed order is part of determinism.
var headerOrder = []string{"Date", "From", "To", "Cc", "Reply-To", "Subject", "Message-Id", "In-Reply-To", "References"}

// Normalize canonicalises header keys (textproto form), drops structural
// headers and empty values, strips CR/LF from every value (header injection
// guard) and stamps the format version. Called before Canonical so two
// stacks that spell "message-id" differently produce one sha.
func (r *Record) Normalize() {
	r.Version = FormatVersion
	if len(r.Headers) > 0 {
		h := make(map[string]string, len(r.Headers))
		for k, v := range r.Headers {
			ck := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(k))
			v = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(v))
			if ck == "" || v == "" || structuralHeaders[ck] {
				continue
			}
			h[ck] = v
		}
		r.Headers = h
	}
	for i := range r.Parts {
		if r.Parts[i].N == 0 {
			r.Parts[i].N = i + 1
		}
		r.Parts[i].Type = strings.ToLower(strings.TrimSpace(r.Parts[i].Type))
		r.Parts[i].SHA256 = strings.ToLower(strings.TrimSpace(r.Parts[i].SHA256))
	}
}

// Canonical returns the record's canonical JSON — the bytes stored in the
// CAS and hashed for the row's sha256. encoding/json sorts map keys and
// emits struct fields in declaration order, so equal records give equal
// bytes.
func (r *Record) Canonical() ([]byte, string, error) {
	r.Normalize()
	if r.Text == "" && r.HTML == "" && len(r.Parts) == 0 && len(r.Headers) == 0 {
		return nil, "", errors.New("imap: empty record")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

// ParseRecord decodes canonical record bytes read back from the CAS.
func ParseRecord(b []byte) (*Record, error) {
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("imap: record: %w", err)
	}
	return &r, nil
}

// RenderOptions are the per-message inputs to the formatter that are not
// part of the record itself. All of them are stored on the row, so a
// re-render is reproducible.
type RenderOptions struct {
	// ObjectKey seeds the synthesized Message-ID when the record has none.
	ObjectKey string
	// Domain is the right-hand side of a synthesized Message-ID (the
	// account's domain). Empty falls back to "txco.invalid".
	Domain string
	// InternalDate is the Date: fallback when the record carries none.
	InternalDate time.Time
}

// Render produces the RFC 5322 bytes for a record, deterministically:
// MIME boundaries derive from the record's canonical sha, header order is
// fixed, bodies are quoted-printable, lines end in CRLF. sha is the
// canonical sha256 hex Canonical returned (also the CAS key).
func Render(rec *Record, sha string, opt RenderOptions) ([]byte, error) {
	if rec == nil {
		return nil, errors.New("imap: nil record")
	}
	if len(sha) < 24 {
		return nil, errors.New("imap: render needs the record sha")
	}
	headers := make(map[string]string, len(rec.Headers)+2)
	for k, v := range rec.Headers {
		ck := textproto.CanonicalMIMEHeaderKey(k)
		if structuralHeaders[ck] {
			continue
		}
		headers[ck] = v
	}
	if headers["Date"] == "" {
		d := opt.InternalDate
		if d.IsZero() {
			return nil, errors.New("imap: render needs an internal date when the record has no Date")
		}
		headers["Date"] = d.UTC().Format(time.RFC1123Z)
	}
	if headers["Message-Id"] == "" {
		dom := opt.Domain
		if dom == "" {
			dom = "txco.invalid"
		}
		seed := sha256.Sum256([]byte(opt.ObjectKey))
		headers["Message-Id"] = "<txco-" + hex.EncodeToString(seed[:16]) + "@" + dom + ">"
	}

	var out bytes.Buffer
	seen := map[string]bool{}
	writeHeader := func(k string) {
		v, ok := headers[k]
		if !ok || seen[k] {
			return
		}
		seen[k] = true
		out.WriteString(headerName(k))
		out.WriteString(": ")
		out.WriteString(encodeHeaderValue(k, v))
		out.WriteString("\r\n")
	}
	for _, k := range headerOrder {
		writeHeader(k)
	}
	rest := make([]string, 0, len(headers))
	for k := range headers {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		writeHeader(k)
	}
	out.WriteString("MIME-Version: 1.0\r\n")

	base := "=_txco_" + sha[:24]
	body, err := renderBody(rec, base)
	if err != nil {
		return nil, err
	}
	out.WriteString(body.headers)
	out.WriteString("\r\n")
	out.Write(body.content)
	return out.Bytes(), nil
}

// entity is one rendered MIME entity: its own Content-* headers (each line
// CRLF-terminated) and its encoded content.
type entity struct {
	headers string
	content []byte
}

func renderBody(rec *Record, base string) (entity, error) {
	var text entity
	switch {
	case rec.HTML != "" && rec.Text != "":
		alt := multipart("alternative", base+"_alt", textEntity("text/plain", rec.Text), textEntity("text/html", rec.HTML))
		text = alt
	case rec.HTML != "":
		text = textEntity("text/html", rec.HTML)
	default:
		text = textEntity("text/plain", rec.Text)
	}
	if len(rec.Parts) == 0 {
		return text, nil
	}
	children := []entity{text}
	for _, p := range rec.Parts {
		children = append(children, partStub(p))
	}
	return multipart("mixed", base+"_mix", children...), nil
}

// partStub renders an attachment reference as a small text/plain part. The
// bytes themselves are not inlined in format version 1: a retained part is
// addressable by its sha through the blob plane; an unretained one is
// named so the reader knows it existed.
func partStub(p PartRef) entity {
	var b strings.Builder
	fmt.Fprintf(&b, "[Attachment %d: %s", p.N, orDefault(p.Name, "(unnamed)"))
	if p.Type != "" {
		fmt.Fprintf(&b, " (%s", p.Type)
		if p.Size > 0 {
			fmt.Fprintf(&b, ", %d bytes", p.Size)
		}
		b.WriteString(")")
	} else if p.Size > 0 {
		fmt.Fprintf(&b, " (%d bytes)", p.Size)
	}
	if p.SHA256 != "" {
		fmt.Fprintf(&b, " — retained, sha256 %s", p.SHA256)
	} else {
		b.WriteString(" — not retained")
	}
	b.WriteString("]\n")
	return textEntity("text/plain", b.String())
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func textEntity(ctype, text string) entity {
	return entity{
		headers: "Content-Type: " + ctype + "; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n",
		content: qp(text),
	}
}

func multipart(subtype, boundary string, children ...entity) entity {
	var b bytes.Buffer
	for _, c := range children {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString(c.headers)
		b.WriteString("\r\n")
		b.Write(c.content)
		if !bytes.HasSuffix(c.content, []byte("\r\n")) {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("--" + boundary + "--\r\n")
	return entity{
		headers: "Content-Type: multipart/" + subtype + "; boundary=\"" + boundary + "\"\r\n",
		content: b.Bytes(),
	}
}

// qp quoted-printable-encodes text with CRLF line breaks. Input line
// endings are normalised to LF first so CRLF and LF sources render the
// same bytes.
func qp(text string) []byte {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var buf bytes.Buffer
	w := quotedprintable.NewWriter(&buf)
	_, _ = w.Write([]byte(text))
	_ = w.Close()
	out := buf.Bytes()
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\r\n")) {
		out = append(out, '\r', '\n')
	}
	return out
}

// headerName restores the conventional capitalisation of the few headers
// whose textproto canonical form differs (Message-Id → Message-ID).
func headerName(k string) string {
	switch k {
	case "Message-Id":
		return "Message-ID"
	case "Mime-Version":
		return "MIME-Version"
	}
	return k
}

// encodeHeaderValue renders a header value RFC 2047-safely. Address headers
// go through net/mail (per-address encoding of display names); everything
// else is Q-encoded as a whole when it contains non-ASCII or control bytes.
func encodeHeaderValue(key, v string) string {
	if addressHeaders[key] {
		if list, err := mail.ParseAddressList(v); err == nil && len(list) > 0 {
			parts := make([]string, 0, len(list))
			for _, a := range list {
				parts = append(parts, a.String())
			}
			return strings.Join(parts, ", ")
		}
	}
	if isPrintableASCII(v) {
		return v
	}
	return mime.QEncoding.Encode("utf-8", v)
}

func isPrintableASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || (r < 0x20 && r != '\t') || r == 0x7f {
			return false
		}
	}
	return true
}

// Excerpt returns the first n bytes of the record's plain text (or a
// tag-stripped slice of the HTML when there is no text), for the
// text_excerpt column that column-backed SEARCH BODY/TEXT reads.
func (r *Record) Excerpt(n int) string {
	s := r.Text
	if s == "" {
		s = stripTags(r.HTML)
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		// cut on a rune boundary
		for n > 0 && !isRuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
			b.WriteByte(' ')
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}
