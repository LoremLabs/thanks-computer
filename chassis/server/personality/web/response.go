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
// envelope to w, returning the resolved status. It mirrors the inline
// head-application in the buffered handler (checkStatus + checkContentType
// + the _txc.web.res.headers fan-out) and is used by the streaming path,
// which must commit status + headers before the first body chunk. It does
// NOT call WriteHeader — the caller does, after this returns.
func applyResponseHead(w http.ResponseWriter, output string) int {
	output, status := checkStatus(output)
	output = checkContentType(output)
	// Iterate the header values off the already-held Result instead of
	// re-resolving "_txc.web.res.headers.<key>" against the full doc
	// per header (each of those Gets re-scanned the whole envelope).
	gjson.Get(output, "_txc.web.res.headers").ForEach(func(key, value gjson.Result) bool {
		value.ForEach(func(k, v gjson.Result) bool {
			w.Header().Set(key.String(), v.String())
			return true
		})
		return true
	})
	return status
}

// applyAdmission translates a transport-neutral admission-denial marker
// (_txc.admission.{denied,status,reason}, stamped by the shared gate) into
// the _txc.web.res.* fields this outlet renders. It fires only when the
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
