package lmtp

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/jhillyerd/enmime"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// bounceDetected reports whether an inbound message is a bounce / auto-generated
// DSN, so a stack can halt before replying and avoid mail loops. Surfaced as
// `_txc.lmtp.is_bounce` (read it as `@lmtp.is_bounce`). Two signals:
//   - a null reverse-path (empty SMTP MAIL FROM) — the RFC 5321 marker every
//     compliant sender stamps on bounces and auto-responses; the primary, most
//     reliable check (and what the OOO guard's `@lmtp.mail.from != ""` keys on).
//   - a formal delivery-status report (Content-Type: multipart/report;
//     report-type=delivery-status) — catches a DSN sent with a non-null sender.
func bounceDetected(mailFrom, msgJSON string) bool {
	if strings.TrimSpace(mailFrom) == "" {
		return true
	}
	ct := strings.ToLower(gjson.Get(msgJSON, "headers.content-type").String())
	return strings.Contains(ct, "multipart/report") && strings.Contains(ct, "delivery-status")
}

// parseMessage takes RFC 5322 bytes and returns a JSON object suitable
// for `sjson.SetRaw` under `_txc.lmtp.msg`. Populates:
//
//	id           string                     — Message-ID header
//	date         string (RFC3339, optional) — parsed Date header
//	subject      string                     — RFC 2047 decoded
//	from / to / cc  [{name, addr}, ...]
//	text         string                     — text/plain body
//	html         string                     — text/html body
//	headers      { name: [values...] }      — multi-value-safe
//	attachments  [{name, type, size, sha256, content}, ...]
//
// Caller is responsible for `_txc.lmtp.msg.raw` (the b64-encoded
// original bytes) — kept separately as the always-safe escape hatch
// for rules that want to re-deliver, archive, or re-parse.
//
// enmime's parse is forgiving: missing Subject, bad base64, non-UTF-8
// charsets, malformed multipart all produce best-effort output plus
// non-fatal entries in `env.Errors`. We surface those errors to the
// caller for logging but do NOT fail — a partly-parsed envelope is
// more useful to rules than nothing.
func parseMessage(raw []byte) (jsonOut string, err error) {
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}

	out := "{}"

	if id := env.GetHeader("Message-ID"); id != "" {
		out, _ = sjson.Set(out, "id", id)
	}
	if d, derr := env.Date(); derr == nil && !d.IsZero() {
		out, _ = sjson.Set(out, "date", d.UTC().Format(time.RFC3339))
	}
	if s := env.GetHeader("Subject"); s != "" {
		out, _ = sjson.Set(out, "subject", s)
	}

	if addrs := addressList(env, "From"); len(addrs) > 0 {
		out, _ = sjson.SetRaw(out, "from", addrsJSON(addrs))
	}
	if addrs := addressList(env, "To"); len(addrs) > 0 {
		out, _ = sjson.SetRaw(out, "to", addrsJSON(addrs))
	}
	if addrs := addressList(env, "Cc"); len(addrs) > 0 {
		out, _ = sjson.SetRaw(out, "cc", addrsJSON(addrs))
	}

	if env.Text != "" {
		out, _ = sjson.Set(out, "text", env.Text)
	}
	if env.HTML != "" {
		out, _ = sjson.Set(out, "html", env.HTML)
	}

	out, _ = sjson.SetRaw(out, "headers", headersJSON(env))

	if atts := attachmentsJSON(env); atts != "" {
		out, _ = sjson.SetRaw(out, "attachments", atts)
	}

	return out, nil
}

// addressList wraps env.AddressList with a nil-safe fallback. enmime
// returns an error when the header is missing or non-address; we treat
// that as "no addresses" rather than propagating.
func addressList(env *enmime.Envelope, key string) []*mail.Address {
	addrs, err := env.AddressList(key)
	if err != nil {
		return nil
	}
	return addrs
}

// addrsJSON serializes []*mail.Address into a JSON array of
// `{name, addr}` objects. Names already RFC 2047 decoded by enmime.
func addrsJSON(addrs []*mail.Address) string {
	if len(addrs) == 0 {
		return "[]"
	}
	out := "[]"
	for i, a := range addrs {
		out, _ = sjson.Set(out, "-1", map[string]string{})
		out, _ = sjson.Set(out, jsonIdx(i)+".name", a.Name)
		out, _ = sjson.Set(out, jsonIdx(i)+".addr", a.Address)
	}
	return out
}

// jsonIdx renders an integer index for sjson paths.
func jsonIdx(i int) string {
	return itoa(i)
}

// itoa is a tiny strconv.Itoa replacement that avoids the import for
// such a narrow use. Performance not relevant here; mail volumes are
// orders of magnitude lower than HTTP request volumes.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// headersJSON serializes the message headers as a string-keyed map
// where each value is a JSON array of strings. Multi-valued headers
// (Received, DKIM-Signature, Authentication-Results) preserve order.
//
// Keys are normalized to lowercase to make rule selectors stable —
// `_txc.lmtp.msg.headers.received` works regardless of whether the
// sender wrote `Received:` or `RECEIVED:`. Matches how the web inlet
// is consumed in practice (rules SELECT on canonicalized header
// names).
func headersJSON(env *enmime.Envelope) string {
	keys := env.GetHeaderKeys()
	if len(keys) == 0 {
		return "{}"
	}
	// Sort keys for byte-deterministic output. Go map iteration is
	// randomized, which would otherwise make the produced JSON
	// reshuffle on every parse — fine for rule evaluation but bad
	// for trace storage, signing, and any downstream consumer that
	// hashes the envelope.
	sort.Strings(keys)
	out := "{}"
	for _, k := range keys {
		lk := strings.ToLower(k)
		vals := env.GetHeaderValues(k)
		if len(vals) == 0 {
			continue
		}
		out, _ = sjson.Set(out, escapeKey(lk), vals)
	}
	return out
}

// escapeKey wraps a header key so sjson treats it as a single literal
// path segment. Some headers contain `-` (Content-Type) or other
// chars that sjson handles fine, but headers with `.` (rare but
// possible in vendor X-headers like `X-Example.Foo`) would otherwise
// be interpreted as nested paths.
func escapeKey(k string) string {
	return strings.ReplaceAll(k, ".", `\.`)
}

// attachmentsJSON serializes Attachments + Inlines as a JSON array.
// Each entry: {name, type, size, sha256, content (b64)}.
// Phase 1 always inlines; the LMTPInlineMaxBytes threshold for
// large-attachment offload is Phase 5 polish.
//
// Inlines (cid: references in HTML) and Attachments (Content-
// Disposition: attachment) are both surfaced — rules that care about
// the distinction can read `_txc.lmtp.msg.headers.content-disposition`
// of the original message, or we can split into two arrays in a
// future revision if real usage demands it.
func attachmentsJSON(env *enmime.Envelope) string {
	all := make([]*enmime.Part, 0, len(env.Attachments)+len(env.Inlines))
	all = append(all, env.Attachments...)
	all = append(all, env.Inlines...)
	if len(all) == 0 {
		return ""
	}
	out := "[]"
	for i, p := range all {
		sum := sha256.Sum256(p.Content)
		entry := map[string]interface{}{
			"name":    p.FileName,
			"type":    p.ContentType,
			"size":    len(p.Content),
			"sha256":  hex.EncodeToString(sum[:]),
			"content": base64.StdEncoding.EncodeToString(p.Content),
		}
		out, _ = sjson.Set(out, "-1", entry)
		_ = i
	}
	return out
}
