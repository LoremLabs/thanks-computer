package mail

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhillyerd/enmime/v2"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// ParseMessage takes RFC 5322 bytes and returns a JSON object describing the
// message — the shared inbound-message shape used by BOTH the LMTP inlet
// (under `_txc.lmtp.msg`) and the remote-source watcher (under
// `_txc.source.msg`), so a mail that arrived by delivery and one fetched from
// a remote mailbox produce byte-identical JSON. Populates:
//
//	id           string                     — Message-ID header
//	date         string (RFC3339, optional) — parsed Date header
//	subject      string                     — RFC 2047 decoded
//	from / to / cc  [{name, addr}, ...]
//	text         string                     — text/plain body
//	html         string                     — text/html body
//	headers      { name: [values...] }      — multi-value-safe, keys LOWERCASED
//	attachments  [{name, type, size, sha256, content}, ...]   (content = base64)
//	calendar     {method, uid, partstat?, attendee?, start?, end?, sequence?}
//	             — only when an iTIP part is present
//
// The caller is responsible for the `.raw` field (the b64-encoded original
// bytes) — kept separately as the always-safe escape hatch for rules that
// want to re-deliver, archive, or re-parse.
//
// enmime's parse is forgiving: missing Subject, bad base64, non-UTF-8
// charsets, malformed multipart all produce best-effort output plus non-fatal
// entries in `env.Errors`. We surface those errors to the caller for logging
// but do NOT fail — a partly-parsed envelope is more useful to rules than
// nothing.
func ParseMessage(raw []byte) (jsonOut string, err error) {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}

	out := jsonx.NewObject()

	if id := env.GetHeader("Message-ID"); id != "" {
		out.Set("id", id)
	}
	if d, derr := env.Date(); derr == nil && !d.IsZero() {
		out.Set("date", d.UTC().Format(time.RFC3339))
	}
	if s := env.GetHeader("Subject"); s != "" {
		out.Set("subject", s)
	}

	if addrs := addressList(env, "From"); len(addrs) > 0 {
		out.SetRaw("from", addrsJSON(addrs))
	}
	if addrs := addressList(env, "To"); len(addrs) > 0 {
		out.SetRaw("to", addrsJSON(addrs))
	}
	if addrs := addressList(env, "Cc"); len(addrs) > 0 {
		out.SetRaw("cc", addrsJSON(addrs))
	}

	if env.Text != "" {
		out.Set("text", env.Text)
	}
	if env.HTML != "" {
		out.Set("html", env.HTML)
	}

	out.SetRaw("headers", headersJSON(env))

	if atts := attachmentsJSON(env); atts != "" {
		out.SetRaw("attachments", atts)
	}
	if cal := calendarJSON(env); cal != "" {
		out.SetRaw("calendar", cal)
	}

	return out.String(), nil
}

// addressList wraps env.AddressList with a nil-safe fallback. enmime returns
// an error when the header is missing or non-address; we treat that as "no
// addresses" rather than propagating.
func addressList(env *enmime.Envelope, key string) []*mail.Address {
	addrs, err := env.AddressList(key)
	if err != nil {
		return nil
	}
	return addrs
}

// addrsJSON serializes []*mail.Address into a JSON array of `{name, addr}`
// objects. Names already RFC 2047 decoded by enmime.
func addrsJSON(addrs []*mail.Address) string {
	if len(addrs) == 0 {
		return "[]"
	}
	out := jsonx.NewArray()
	for i, a := range addrs {
		out.Set("-1", map[string]string{})
		out.Set(jsonIdx(i)+".name", a.Name)
		out.Set(jsonIdx(i)+".addr", a.Address)
	}
	return out.String()
}

// jsonIdx renders an integer index for sjson paths.
func jsonIdx(i int) string { return strconv.Itoa(i) }

// headersJSON serializes the message headers as a string-keyed map where each
// value is a JSON array of strings. Multi-valued headers (Received,
// DKIM-Signature, Authentication-Results) preserve order.
//
// Keys are normalized to lowercase to make rule selectors stable —
// `…msg.headers.received` works regardless of whether the sender wrote
// `Received:` or `RECEIVED:`.
func headersJSON(env *enmime.Envelope) string {
	keys := env.GetHeaderKeys()
	if len(keys) == 0 {
		return "{}"
	}
	// Sort keys for byte-deterministic output. Go map iteration is randomized,
	// which would otherwise make the produced JSON reshuffle on every parse —
	// fine for rule evaluation but bad for trace storage, signing, and any
	// downstream consumer that hashes the envelope.
	sort.Strings(keys)
	// NOTE: per-key prepend means the serialized object comes out
	// REVERSE-alphabetical — long-standing sjson-chain fallout that downstream
	// consumers may hash; jsonx reproduces it exactly.
	out := jsonx.NewObject()
	for _, k := range keys {
		lk := strings.ToLower(k)
		vals := env.GetHeaderValues(k)
		if len(vals) == 0 {
			continue
		}
		out.Set(escapeKey(lk), vals)
	}
	return out.String()
}

// escapeKey wraps a header key so sjson treats it as a single literal path
// segment. Headers with `.` (rare but possible in vendor X-headers like
// `X-Example.Foo`) would otherwise be interpreted as nested paths.
func escapeKey(k string) string {
	return strings.ReplaceAll(k, ".", `\.`)
}

// attachmentsJSON serializes Attachments + Inlines as a JSON array. Each
// entry: {name, type, size, sha256, content (b64)}.
//
// Inlines (cid: references in HTML) and Attachments (Content-Disposition:
// attachment) are both surfaced — rules that care about the distinction can
// read the message's `content-disposition` header.
func attachmentsJSON(env *enmime.Envelope) string {
	all := make([]*enmime.Part, 0, len(env.Attachments)+len(env.Inlines))
	all = append(all, env.Attachments...)
	all = append(all, env.Inlines...)
	if len(all) == 0 {
		return ""
	}
	out := jsonx.NewArray()
	for _, p := range all {
		sum := sha256.Sum256(p.Content)
		entry := map[string]interface{}{
			"name":    p.FileName,
			"type":    p.ContentType,
			"size":    len(p.Content),
			"sha256":  hex.EncodeToString(sum[:]),
			"content": base64.StdEncoding.EncodeToString(p.Content),
		}
		out.Set("-1", entry)
	}
	return out.String()
}

// calendarJSON surfaces the iTIP facts of a message that carries a
// text/calendar (or application/ics) part anywhere in its tree — an
// invitation, a cancellation, or the Accept / Decline a mail client sends
// back to the ORGANIZER — as `…msg.calendar = {method, uid, partstat?}`.
// enmime files a text/calendar alternative under OtherParts, which
// attachmentsJSON does not list. A line scan after unfolding, not a parse:
// METHOD (the Content-Type's `method` parameter wins when present), the first
// UID, the first ATTENDEE's PARTSTAT and mailto (who is answering),
// DTSTART/DTEND as RFC3339 UTC (TZID and all-day forms resolved with the
// embedded tz data), and SEQUENCE. Empty when no such part.
func calendarJSON(env *enmime.Envelope) string {
	if env == nil || env.Root == nil {
		return ""
	}
	part := env.Root.BreadthMatchFirst(func(p *enmime.Part) bool {
		ct := strings.ToLower(p.ContentType)
		return ct == "text/calendar" || ct == "application/ics"
	})
	if part == nil {
		return ""
	}
	text := strings.ReplaceAll(string(part.Content), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\n ", "")
	text = strings.ReplaceAll(text, "\n\t", "")
	method := strings.ToUpper(strings.TrimSpace(part.ContentTypeParams["method"]))
	var uid, partstat, attendee, start, end string
	var sequence int64 = -1
	for _, line := range strings.Split(text, "\n") {
		upper := strings.ToUpper(line)
		switch {
		case method == "" && strings.HasPrefix(upper, "METHOD:"):
			method = strings.ToUpper(strings.TrimSpace(line[len("METHOD:"):]))
		case uid == "" && strings.HasPrefix(upper, "UID:"):
			uid = strings.TrimSpace(line[len("UID:"):])
		case attendee == "" && strings.HasPrefix(upper, "ATTENDEE"):
			if i := strings.Index(upper, "PARTSTAT="); i >= 0 {
				rest := upper[i+len("PARTSTAT="):]
				if j := strings.IndexAny(rest, ";:"); j >= 0 {
					rest = rest[:j]
				}
				partstat = strings.TrimSpace(rest)
			}
			if i := strings.LastIndex(upper, "MAILTO:"); i >= 0 {
				attendee = strings.ToLower(strings.TrimSpace(line[i+len("MAILTO:"):]))
			}
		case start == "" && (strings.HasPrefix(upper, "DTSTART:") || strings.HasPrefix(upper, "DTSTART;")):
			start = icalWhen(line)
		case end == "" && (strings.HasPrefix(upper, "DTEND:") || strings.HasPrefix(upper, "DTEND;")):
			end = icalWhen(line)
		case sequence < 0 && strings.HasPrefix(upper, "SEQUENCE:"):
			if n, err := strconv.ParseInt(strings.TrimSpace(line[len("SEQUENCE:"):]), 10, 64); err == nil {
				sequence = n
			}
		}
	}
	if method == "" && uid == "" {
		return ""
	}
	out := jsonx.NewObject()
	out.Set("method", method)
	out.Set("uid", uid)
	if partstat != "" {
		out.Set("partstat", partstat)
	}
	if attendee != "" {
		out.Set("attendee", attendee)
	}
	if start != "" {
		out.Set("start", start)
	}
	if end != "" {
		out.Set("end", end)
	}
	if sequence >= 0 {
		out.Set("sequence", sequence)
	}
	return out.String()
}

// icalWhen turns one DTSTART/DTEND property line into RFC3339 UTC, or "" when
// it cannot: `20260909T120000Z` (UTC), `;TZID=Europe/Paris:20260909T140000`
// (local, resolved with the embedded tz data), `;VALUE=DATE:20260909`
// (all-day, midnight UTC). A floating time with no TZID is read as UTC.
func icalWhen(line string) string {
	i := strings.Index(line, ":")
	if i < 0 {
		return ""
	}
	params, value := strings.ToUpper(line[:i]), strings.TrimSpace(line[i+1:])
	loc := time.UTC
	if j := strings.Index(params, "TZID="); j >= 0 {
		tz := params[j+len("TZID="):]
		if k := strings.IndexAny(tz, ";:"); k >= 0 {
			tz = tz[:k]
		}
		// The parameter was upper-cased for matching; take the zone from the
		// original line so its case survives (IANA names are case-sensitive).
		orig := line[:i]
		if jj := strings.Index(strings.ToUpper(orig), "TZID="); jj >= 0 {
			raw := orig[jj+len("TZID="):]
			if k := strings.IndexAny(raw, ";:"); k >= 0 {
				raw = raw[:k]
			}
			tz = strings.Trim(raw, "\"")
		}
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	switch {
	case len(value) == 16 && strings.HasSuffix(value, "Z"):
		if t, err := time.Parse("20060102T150405Z", value); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	case len(value) == 15:
		if t, err := time.ParseInLocation("20060102T150405", value, loc); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	case len(value) == 8:
		if t, err := time.ParseInLocation("20060102", value, loc); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return ""
}
