package web

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// applyResponseHead applies the status and headers from a response
// envelope to w, returning the resolved status and writeResHeaders'
// conflicts. It mirrors the inline
// head-application in the buffered handler (checkStatus + checkContentType
// + the _txc.web.res.headers fan-out) and is used by the streaming path,
// which must commit status + headers before the first body chunk. It does
// NOT call WriteHeader — the caller does, after this returns.
func applyResponseHead(w http.ResponseWriter, output string) (int, []string) {
	output, status := checkStatus(output)
	output = checkContentType(output)
	output = applyNegotiation(output)
	conflicts := writeResHeaders(w.Header(), output)
	return status, conflicts
}

// singleValued are the response fields HTTP defines as one value. Two
// different values for one of them are the app's bug (a stack set it from
// two ops), and browsers reject some outright (Chrome fails a response with
// two different Location, Content-Disposition or Content-Length values).
// The writer still sends what the app said; it reports the conflict so the
// bug can be found.
var singleValued = map[string]bool{
	"Content-Type": true, "Content-Length": true, "Content-Disposition": true,
	"Content-Location": true, "Content-Range": true, "Location": true,
	"Etag": true, "Last-Modified": true, "Expires": true, "Retry-After": true,
	"Access-Control-Allow-Origin": true, "Access-Control-Allow-Credentials": true,
	"Strict-Transport-Security": true, "X-Frame-Options": true,
	"X-Content-Type-Options": true, "Referrer-Policy": true,
}

// writeResHeaders renders _txc.web.res.headers onto h, for both the
// buffered and the streaming writer, and returns the single-value fields
// that got more than one distinct value.
//
// A header's value is an array (@web.res.headers.<name>.0, .1, …), and the
// chassis sends exactly what the app wrote: every distinct value is its own
// header line, as HTTP allows (repeated lines of a list-valued field mean
// the same as one comma-joined line; Set-Cookie must repeat). Ops' outputs
// merge by appending (processor.MergeJSON), so two ops setting the same
// header both reach the client — the chassis doesn't arbitrate between
// them. An exact repeat is written once, the empty Set-Cookie is dropped,
// and the envelope's values replace any already on h.
//
// It iterates the held Result rather than re-resolving
// "_txc.web.res.headers.<key>" per header, which re-scanned the whole
// envelope each time.
func writeResHeaders(h http.Header, output string) []string {
	seen := map[string]map[string]bool{}
	var order []string
	gjson.Get(output, "_txc.web.res.headers").ForEach(func(key, value gjson.Result) bool {
		name := http.CanonicalHeaderKey(key.String())
		vals := seen[name]
		if vals == nil {
			vals = map[string]bool{}
			seen[name] = vals
			order = append(order, name)
			h.Del(name)
		}
		value.ForEach(func(_, v gjson.Result) bool {
			s := v.String()
			if vals[s] || (s == "" && name == "Set-Cookie") {
				return true
			}
			vals[s] = true
			h.Add(name, s)
			return true
		})
		return true
	})
	var conflicts []string
	for _, name := range order {
		if singleValued[name] && len(seen[name]) > 1 {
			conflicts = append(conflicts, name)
		}
	}
	return conflicts
}

// applyAdmission translates a transport-neutral admission-denial marker
// (_txc.admission.{denied,status,reason}, stamped by the shared gate) into
// the _txc.web.res.* fields this response writer renders. It fires only when the
// gate denied the request AND the pipeline didn't already shape an
// explicit web status — so a stack that emits its own 4xx still wins. A
// 503 (drain) additionally gets Retry-After + Connection: close so proxies
// don't pin a draining node. The body is a minimal "<code> <text>" line;
// no internal state leaks because getOutput writes the explicit body and
// strips _-prefixed keys.
func applyAdmission(output string) string {
	if !gjson.Get(output, "_txc.admission.denied").Bool() {
		return output
	}
	if gjson.Get(output, "_txc.web.res.status").Exists() {
		return output // a stack shaped its own response; leave it alone
	}
	// Denied path (rare): one scan for the remaining admission fields
	// instead of three.
	fields := gjson.GetMany(output,
		"_txc.admission.status", "_txc.admission.reason", "_txc.admission.retry_after")
	status := int(fields[0].Int())
	if status < 100 || status > 599 {
		status = http.StatusForbidden
	}
	output, _ = sjson.Set(output, "_txc.web.res.status", status)
	output, _ = sjson.Set(output, "_txc.web.res.headers.content-type.0", "text/plain; charset=utf-8")
	if reason := fields[1].String(); reason != "" {
		output, _ = sjson.Set(output, "_txc.web.res.headers.x-txc-deny-reason.0", reason)
	}
	// Retry-After from the gate's suggestion (rate-limit carries the bucket
	// delay); 429/503 are transient. Drain (503) also closes the connection
	// so proxies don't pin a draining node, and defaults Retry-After to 0.
	if ra := fields[2]; ra.Exists() {
		output, _ = sjson.Set(output, "_txc.web.res.headers.retry-after.0", strconv.Itoa(int(ra.Int())))
	}
	if status == http.StatusServiceUnavailable {
		if !fields[2].Exists() {
			output, _ = sjson.Set(output, "_txc.web.res.headers.retry-after.0", "0")
		}
		output, _ = sjson.Set(output, "_txc.web.res.headers.connection.0", "close")
	}
	body := strconv.Itoa(status) + " " + http.StatusText(status) + "\n"
	output, _ = sjson.Set(output, "_txc.web.res.body", base64.StdEncoding.EncodeToString([]byte(body)))
	return output
}

// failureResponse renders a run that failed (the processor's ErrorStr
// answer: a halting op error such as a compute that threw or ran out of
// wall-clock, a failed continuation or deferred op, a cancel) as the
// failure it is: 500 unless the payload carries its own status (an
// abort's 503), never cached, and the body the error alone
// ({"error": {...}}), never the envelope, even where SHOW_PRIVATE_VARS
// (dev) shows envelopes. Without it a failed compute answered 200 with the
// writer's own _txc bookkeeping as its body.
func failureResponse(output string) string {
	if !gjson.Get(output, "_txc.web.res.status").Exists() {
		output, _ = sjson.Set(output, "_txc.web.res.status", http.StatusInternalServerError)
	}
	if !gjson.Get(output, "_txc.web.res.headers.cache-control").Exists() {
		output, _ = sjson.Set(output, "_txc.web.res.headers.cache-control.0", "no-store")
	}
	if gjson.Get(output, "_txc.web.res.body").String() == "" {
		body, ok := stripTopLevelUnderscoreFast(output)
		if !ok {
			body = stripTopLevelUnderscoreSlow(output)
		}
		output, _ = sjson.Set(output, "_txc.web.res.body", base64.StdEncoding.EncodeToString([]byte(body)))
	}
	return output
}

// getOutput convert a body from base64, or return json
func getOutput(output string, hidePrivate bool) ([]byte, error) {

	b64BodyString := gjson.Get(output, "_txc.web.res.body").String()
	if b64BodyString == "" {
		// no body = return raw output

		// Per-event override: if _txc.flag_private is true, keep
		// underscore-prefixed fields even when chassis config would
		// strip them. Lets a rule (or a chassis stamping it in dev/
		// debug mode) ask for the full envelope without changing
		// chassis-wide config.
		flagPrivate := gjson.Get(output, "_txc.flag_private").Bool()

		// but first check if we should strip out private vars
		if hidePrivate && !flagPrivate {
			// hide vars unless we're told to show them by the config
			if stripped, ok := stripTopLevelUnderscoreFast(output); ok {
				output = stripped
			} else {
				output = stripTopLevelUnderscoreSlow(output)
			}
		}

		return []byte(output), nil
	}

	decoded, err := base64.StdEncoding.DecodeString(b64BodyString)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

// stripTopLevelUnderscoreSlow is the original per-key sjson.Delete loop
// (each Delete re-copies the whole doc). Kept as the semantic reference
// and the fallback for docs the fast path can't prove compact/plain.
func stripTopLevelUnderscoreSlow(output string) string {
	gjson.Parse(output).ForEach(func(key, value gjson.Result) bool {
		if strings.HasPrefix(key.String(), "_") {
			output, _ = sjson.Delete(output, key.String())
		}
		return true
	})
	return output
}

// plainStripKey mirrors the constraint under which sjson.Delete(key)
// deletes exactly that top-level key: no '.' (path separator), no ':'
// (force prefix), no wildcard/escape bytes. Keys outside this set make
// the slow loop misbehave in its own historical ways (dotted keys
// survive, wildcards poison the doc) — those docs bail to it.
func plainStripKey(k string) bool {
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '-' || c == '/' || c == '$' || c == ' ':
		default:
			return false
		}
	}
	return true
}

// stripTopLevelUnderscoreFast removes top-level "_"-prefixed keys in a
// single pass. It proves the doc is fully compact first: every byte
// must be accounted for by key spans, value spans, and the structural
// {":,} bytes — length equality is only possible with zero whitespace,
// because whitespace is the only other thing JSON allows between
// tokens. On a compact doc the sequential-Delete result is exactly the
// surviving pairs re-joined, which is what this emits.
func stripTopLevelUnderscoreFast(output string) (string, bool) {
	if len(output) < 2 || output[0] != '{' || !gjson.Valid(output) {
		return "", false
	}
	parsed := gjson.Parse(output)
	if !parsed.IsObject() {
		return "", false
	}
	total := 2
	pairs := 0
	stripped := false
	ok := true
	var b strings.Builder
	b.Grow(len(output))
	b.WriteByte('{')
	first := true
	parsed.ForEach(func(key, value gjson.Result) bool {
		if key.Raw == "" || !plainStripKey(key.String()) {
			ok = false
			return false
		}
		if pairs > 0 {
			total++ // comma
		}
		total += len(key.Raw) + 1 + len(value.Raw)
		pairs++
		if strings.HasPrefix(key.String(), "_") {
			stripped = true
			return true
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(key.Raw)
		b.WriteByte(':')
		b.WriteString(value.Raw)
		return true
	})
	if !ok || total != len(output) {
		return "", false
	}
	if !stripped {
		return output, true
	}
	b.WriteByte('}')
	return b.String(), true
}

// sjsonTargetRaw walks path segments the way sjson.Set resolves its
// replace target (per-segment gjson.Get recursion — which can differ
// from a one-shot full-path Get when a doc carries duplicate keys) and
// returns the raw bytes sjson would splice over. ok=false when any
// segment is missing or positionless: sjson would take the insert path
// there, which is never a no-op.
func sjsonTargetRaw(doc string, segs ...string) (string, bool) {
	cur := doc
	for _, s := range segs {
		r := gjson.Get(cur, s)
		if !r.Exists() || r.Index <= 0 {
			return "", false
		}
		cur = r.Raw
	}
	return cur, true
}

// checkStatus Make sure the response object has a valid status set (100-599)
func checkStatus(output string) (string, int) {
	var status int
	st, err := strconv.ParseInt(gjson.Get(output, "_txc.web.res.status").String(), 10, 64)
	if (err != nil) || (st < 100) || (st > 599) {
		st = 200
	}
	status = int(st)
	// Skip the rewrite when sjson's replace target already holds the
	// exact bytes it would write — replacing a span with identical
	// bytes is a full-doc copy for nothing (and it ran on EVERY
	// response).
	if raw, ok := sjsonTargetRaw(output, "_txc", "web", "res", "status"); ok && raw == strconv.Itoa(status) {
		return output, status
	}
	output, _ = sjson.Set(output, "_txc.web.res.status", status)

	return output, status
}

// checkContentType Make sure the response object has a valid content type, defaulting if needed
func checkContentType(output string) string {
	// add a default content-type if we don't have one already
	ct := gjson.Get(output, "_txc.web.res.headers.content-type.0").String()
	if ct == "" {
		ct = "application/json"
	} else if raw, ok := sjsonTargetRaw(output, "_txc", "web", "res", "headers", "content-type", "0"); ok && raw == string(jsonx.AppendStringify(nil, ct)) {
		// already stored in canonical encoding — skip the full-doc copy
		return output
	}
	output, _ = sjson.Set(output, "_txc.web.res.headers.content-type.0", ct)
	return output
}
