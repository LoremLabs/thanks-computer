package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// ExecHTTP Handles execution of http, https operations.
//
// When op.Meta declares `secrets.*` refs (per internal docs/todo-secret-store.md
// §4), this is the only place cleartext crosses from op.Secrets (the
// in-process bag) into the outbound wire — applied as request headers
// or as JSON body field overlays. op.Input is NOT mutated, so the
// trace event still records the request body the operator authored;
// the wire body is a separate buffer that lives only for the
// duration of the HTTP call.
func (pu *Unit) ExecHTTP(ctx context.Context, op operation.Operation) (event.Payload, error) {
	opName := op.Resonator.Exec
	in := []byte(op.Input)
	pu.Logger.Debug("ExecHttp", zap.String("opname", opName), zap.String("input", string(in)))

	// Resolve `secrets.*` overlays (no-op when none declared).
	// Cleartext lives only in `bodyForWire` and `headerOverlays`
	// from here to req.Body / req.Header below; both are released
	// after pu.HTTPClient.Do returns.
	bodyForWire := in
	headerOverlays := map[string]string{}
	if refs, perr := secrets.ParseRefs(op.Meta); perr != nil {
		return event.Payload{
			Raw:  `{}`,
			Type: event.Null,
			Meta: `{"error":["secrets-parse-err"]}`,
		}, perr
	} else if len(refs) > 0 {
		var aerr error
		bodyForWire, aerr = applySecretOverlays(refs, op.Secrets, bodyForWire, headerOverlays)
		if aerr != nil {
			pu.Logger.Warn("HttpExec secrets overlay err", zap.String("err", aerr.Error()))
			return event.Payload{
				Raw:  `{}`,
				Type: event.Null,
				Meta: `{"error":["secrets-overlay-err"]}`,
			}, aerr
		}
	}

	// HTTP method: `WITH method="GET"` (default POST). Body-less methods
	// (GET/HEAD/DELETE) send no request body and skip the JSON
	// Content-Type, so an op can call third-party GET APIs (e.g.
	// timeapi.io) whose query params live in the URL. POST/PUT/PATCH keep
	// the envelope-as-body behavior.
	method := strings.ToUpper(strings.TrimSpace(gjson.Get(op.Meta, "method").String()))
	if method == "" {
		method = "POST"
	}
	sendBody := method == "POST" || method == "PUT" || method == "PATCH"

	// Body content-type: JSON by default (the envelope is the body).
	// `WITH body_encoding="urlencoded"` instead form-encodes a JSON object
	// from the envelope into application/x-www-form-urlencoded — the shape
	// vendor REST APIs like Stripe require (they reject JSON). `WITH
	// body_path="_form_input"` selects which object to encode (defaults to
	// the whole input); nested objects/arrays use bracket notation
	// (line_items[0][price]=…). Header overlays still run last, so an
	// operator can override Content-Type.
	contentType := "application/json"
	if sendBody {
		switch strings.ToLower(strings.TrimSpace(gjson.Get(op.Meta, "body_encoding").String())) {
		case "urlencoded", "form":
			src := bodyForWire
			if bp := normalizeEnvelopePath(gjson.Get(op.Meta, "body_path").String()); bp != "" {
				src = []byte(gjson.GetBytes(bodyForWire, bp).Raw)
			}
			bodyForWire = []byte(formEncode(src))
			contentType = "application/x-www-form-urlencoded"
		}
	}

	var reqBody io.Reader
	if sendBody {
		reqBody = bytes.NewBuffer(bodyForWire)
	}

	req, err := http.NewRequest(method, opName, reqBody)
	if err != nil {
		pu.Logger.Warn("HttpExec err", zap.String("err", err.Error()))

		return event.Payload{
			Raw: `{}`,
			//		Raw:  string(opstack),
			Type: event.Null,
			Meta: `{"error":["dial-http-create-err"]}`,
		}, err
	}

	// get a new request based on original request but with the context
	req = req.WithContext(ctx)

	var ua string = "txco"
	if ctx.Value(config.CtxKeyVersion) != nil {
		ua = fmt.Sprintf("txco/%s", (ctx.Value(config.CtxKeyVersion)).(string))
	}

	req.Header.Set("User-Agent", ua)
	if sendBody {
		req.Header.Set("Content-Type", contentType)
	}
	// Apply secret-bound header overlays last so the operator's
	// declaration wins over chassis defaults (e.g. operator can
	// override Content-Type if a vendor needs `application/x-www-
	// form-urlencoded`, though that would also need a different
	// body shape).
	for k, v := range headerOverlays {
		req.Header.Set(k, v)
	}

	resp, err := pu.HTTPClient.Do(req)
	if err != nil {
		pu.Logger.Warn("HttpExec err2", zap.String("err", err.Error()))

		meta, _ := sjson.Set("", "error[0]", "dial-http-exec-err")
		meta, _ = sjson.Set(meta, "error[0]", "dial-http-exec-err")
		meta, _ = sjson.Set(meta, "errorMsg", err.Error())

		return event.Payload{
			Raw: `{}`,
			//		Raw:  string(opstack),
			Type: event.Null,
			Meta: meta,
		}, err
	}
	defer resp.Body.Close()

	// TODO: look at response status codes
	body, _ := io.ReadAll(resp.Body)
	pu.Logger.Debug("Http Resp", zap.String("opname", opName), zap.String("resp", string(body)))

	// var out []byte
	// in := []byte(op.Input)

	// `WITH into="<path>"` nests the whole response under that key, so two
	// ops can each call an API and the results merge cleanly
	// (e.g. {london:{…}, tokyo:{…}}) instead of overwriting each other at
	// the envelope's top level. Falls back to the raw body if the
	// response isn't valid JSON.
	out := string(body)
	if into := normalizeEnvelopePath(gjson.Get(op.Meta, "into").String()); into != "" {
		if wrapped, werr := sjson.SetRaw("{}", into, out); werr == nil {
			out = wrapped
		}
	}

	res := event.NewJSON(out)
	return res.CreateJSONPayload()
}

// normalizeEnvelopePath turns a txcl-authored envelope path (e.g. a
// `WITH into=…` value or a `_txc.delete` entry) into an sjson path: a
// leading `@` (txcl sugar for `._txc.`) expands, and a leading `.` is
// dropped. "" stays "" (no path).
func normalizeEnvelopePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "@") {
		p = "_txc." + strings.TrimPrefix(p[1:], ".")
	}
	return strings.TrimPrefix(p, ".")
}

// formEncode serializes a JSON value into an
// application/x-www-form-urlencoded body using bracket notation for
// nesting — the convention Stripe / Rails / PHP expect:
//
//	{"a":"hi","b":" x"}                       -> a=hi&b=%20x
//	{"metadata":{"email":"a@b"}}              -> metadata[email]=a%40b
//	{"items":[{"price":"p1"},{"price":"p2"}]} -> items[0][price]=p1&items[1][price]=p2
//
// Object keys and all values are percent-encoded RFC-3986 style (space ->
// %20), which Stripe accepts; the bracket delimiters in keys stay literal
// so the vendor parser reconstructs the structure. Pairs are emitted in
// deterministic (sorted) order — wire order is irrelevant to form parsers,
// and determinism keeps the output testable. Empty/whitespace input yields
// "".
func formEncode(raw []byte) string {
	var pairs []string
	var walk func(prefix string, v gjson.Result)
	walk = func(prefix string, v gjson.Result) {
		switch {
		case v.IsObject():
			v.ForEach(func(k, val gjson.Result) bool {
				seg := formEscape(k.String())
				if prefix == "" {
					walk(seg, val)
				} else {
					walk(prefix+"["+seg+"]", val)
				}
				return true
			})
		case v.IsArray():
			i := 0
			v.ForEach(func(_, val gjson.Result) bool {
				walk(prefix+"["+strconv.Itoa(i)+"]", val)
				i++
				return true
			})
		default:
			// Scalar leaf. A top-level scalar (no key) has nowhere to go in
			// a form body, so it's dropped; nested scalars emit key=value.
			if prefix != "" {
				pairs = append(pairs, prefix+"="+formEscape(v.String()))
			}
		}
	}
	walk("", gjson.ParseBytes(raw))
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// formEscape percent-encodes a urlencoded key segment or value RFC-3986
// style: like url.QueryEscape but emitting space as %20 (not +), which is
// unambiguous and accepted by Stripe and other form parsers.
func formEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// AsyncEnvelope is the `_txc` block handed to an async worker so it can
// post one op result back. The single-use bearer token is delivered
// out-of-band of the body (request header), not in the envelope.
type AsyncEnvelope struct {
	OpContinuationID  string `json:"op_continuation_id"`
	CallbackURL       string `json:"callback_url"`
	RunID             string `json:"run_id"`
	RunContinuationID string `json:"run_continuation_id"`
	Stack             string `json:"stack"`
	Stage             string `json:"stage"`
	Op                string `json:"op"`
	ExpiresAt         string `json:"expires_at,omitempty"`
}

// ExecHTTPAsync invokes an async worker. Unlike ExecHTTP it wraps the op
// input as `{ "input": <op.Input>, "_txc": {…} }` (async-only — ExecHTTP
// is untouched) and carries the single-use callback token in the
// X-Txco-Continuation-Token request header. The worker is expected to
// answer 202 Accepted (optionally `{"job_id":"…"}`); the real result
// arrives later via the callback endpoint. Returns the worker-supplied
// job id (optional, may be "").
func (pu *Unit) ExecHTTPAsync(ctx context.Context, op operation.Operation, env AsyncEnvelope, token string) (jobID string, err error) {
	opName := op.Resonator.Exec

	input := op.Input
	if input == "" {
		input = "{}"
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	body := `{}`
	body, _ = sjson.SetRaw(body, "input", input)
	body, _ = sjson.SetRaw(body, "_txc", string(envJSON))

	req, err := http.NewRequestWithContext(ctx, "POST", opName, bytes.NewBufferString(body))
	if err != nil {
		return "", err
	}

	ua := "txco"
	if ctx.Value(config.CtxKeyVersion) != nil {
		ua = fmt.Sprintf("txco/%s", ctx.Value(config.CtxKeyVersion).(string))
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/json")
	// Out-of-band single-use callback credential. The worker echoes it
	// back as `Authorization: Bearer <token>` on the completion POST.
	req.Header.Set("X-Txco-Continuation-Token", token)

	resp, err := pu.HTTPClient.Do(req)
	if err != nil {
		pu.Logger.Warn("ExecHTTPAsync dial err", zap.String("op", opName), zap.String("err", err.Error()))
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("async worker %s: expected 202, got %d", opName, resp.StatusCode)
	}
	if jid := gjson.GetBytes(rb, "job_id"); jid.Exists() {
		jobID = jid.String()
	}
	return jobID, nil
}

// applySecretOverlays walks the parsed secret refs and applies each
// to the outbound request:
//
//   - `headers.X` → writes the (formatted) cleartext into the
//     headerOverlays map, which the caller then req.Header.Set's.
//   - `body.X.y.z` → sjson.SetBytes the (formatted) cleartext into
//     the request body buffer at that JSON path.
//
// Returns the (possibly modified) body bytes. op.Input is never
// touched; the only places cleartext lives are in `bodyForWire`
// (for body refs) and `headerOverlays` (for header refs). Both are
// released once the HTTP call returns.
//
// Errors when a ref's secret isn't present in the bag (the splice
// should have materialized everything, so a miss here is a chassis
// bug) or when a `format` template fails substitution. The error
// path returns an explicit event.Payload upstream — cleartext does
// not appear in the error meta.
func applySecretOverlays(
	refs []secrets.Ref,
	bag secrets.SecretBag,
	body []byte,
	headerOverlays map[string]string,
) ([]byte, error) {
	for _, ref := range refs {
		cleartext, ok := bag.Get(ref.Secret)
		if !ok {
			return body, fmt.Errorf("secret %q not materialized into op.Secrets (processor splice bug?)", ref.Secret)
		}
		var value string
		if ref.Format == "" {
			value = string(cleartext)
		} else {
			v, err := secrets.Substitute(ref.Format, cleartext)
			if err != nil {
				return body, fmt.Errorf("secret %q format: %w", ref.Secret, err)
			}
			value = v
		}

		switch {
		case strings.HasPrefix(ref.Path, "headers."):
			name := strings.TrimPrefix(ref.Path, "headers.")
			if name == "" {
				return body, fmt.Errorf("empty header name in path %q", ref.Path)
			}
			headerOverlays[name] = value
		case strings.HasPrefix(ref.Path, "body."):
			jsonPath := strings.TrimPrefix(ref.Path, "body.")
			if jsonPath == "" {
				return body, fmt.Errorf("empty body path in %q", ref.Path)
			}
			// If the body is empty, seed with an object so sjson has
			// somewhere to write.
			if len(body) == 0 || string(body) == "null" {
				body = []byte("{}")
			}
			var err error
			body, err = sjson.SetBytes(body, jsonPath, value)
			if err != nil {
				return body, fmt.Errorf("body overlay at %q: %w", ref.Path, err)
			}
		default:
			return body, fmt.Errorf("unsupported secret path %q (must start with headers. or body.)", ref.Path)
		}
	}
	return body, nil
}

// asyncTimeoutCeil reads a per-op timeout from op.Meta (WITH clause),
// mirroring the sync path's logic, and reports whether it exceeds the
// configured ceiling.
func (pu *Unit) opMetaTimeout(op operation.Operation) (timeout time.Duration, overCeiling bool) {
	timeout, _ = time.ParseDuration(pu.Conf.OpTimeout)
	if val := gjson.Get(op.Meta, "timeout"); val.Exists() {
		switch val.Type {
		case gjson.Number:
			timeout = time.Duration(val.Int()) * time.Millisecond
		case gjson.String:
			if parsed, perr := time.ParseDuration(val.String()); perr == nil {
				timeout = parsed
			}
		}
	}
	if maxDur, merr := time.ParseDuration(pu.Conf.OpTimeoutMax); merr == nil && timeout > maxDur {
		return timeout, true
	}
	return timeout, false
}

// opAsyncBudget is the unified-timeout *runtime budget* for an async op:
// how long the out-of-band worker may run before the run is considered
// expired (it seeds the continuation's expires_at). Reads the same `WITH
// timeout` knob authors use for sync ops ("this op finishes within
// $timeout"), falling back to async-runtime-default when omitted.
//
// Deliberately NOT capped by op-timeout-max — async is the long-running
// path (internal docs/todo-deferred-join.md "Unified timeout model"). Distinct from
// opMetaTimeout, which bounds a *synchronous* call and IS capped.
func (pu *Unit) opAsyncBudget(op operation.Operation) time.Duration {
	budget, _ := time.ParseDuration(pu.Conf.AsyncRuntimeDefault) // validated at startup
	if val := gjson.Get(op.Meta, "timeout"); val.Exists() {
		switch val.Type {
		case gjson.Number:
			budget = time.Duration(val.Int()) * time.Millisecond
		case gjson.String:
			if parsed, perr := time.ParseDuration(val.String()); perr == nil {
				budget = parsed
			}
		}
	}
	return budget
}

// opAckTimeout bounds the async *handoff* — how long the chassis waits for
// a worker's 202 ack, NOT the worker's runtime. Low by default
// (async-ack-timeout); invisible plumbing, not part of the author's
// `timeout` mental model.
func (pu *Unit) opAckTimeout() time.Duration {
	ack, _ := time.ParseDuration(pu.Conf.AsyncAckTimeout) // validated at startup
	return ack
}

// opContinueAfter returns the promotion deadline for a `WITH mode = "continuable"`
// op: the upper bound on how long the chassis runs the call sync before
// emitting a 202 + continuation token to the client and detaching the
// in-flight goroutine. Reads `WITH continue_after`, falls back to
// continue-after-default (validated >0 at startup). Caller is expected to
// have already vetted mode=continuable.
func (pu *Unit) opContinueAfter(op operation.Operation) time.Duration {
	d, _ := time.ParseDuration(pu.Conf.ContinueAfterDefault) // validated at startup
	if val := gjson.Get(op.Meta, "continue_after"); val.Exists() {
		switch val.Type {
		case gjson.Number:
			d = time.Duration(val.Int()) * time.Millisecond
		case gjson.String:
			if parsed, perr := time.ParseDuration(val.String()); perr == nil {
				d = parsed
			}
		}
	}
	return d
}

// opContinuableTimeout is the runtime budget for a `WITH mode = "continuable"`
// op — bounds the upstream work both PRE- and POST-promotion. Same author-
// facing `WITH timeout` knob as sync / async; default comes from
// continuable-timeout-default (independent from async-runtime-default so the
// two paths can be tuned separately). Deliberately NOT capped by
// op-timeout-max — continuable IS a long-running path once promoted.
func (pu *Unit) opContinuableTimeout(op operation.Operation) time.Duration {
	budget, _ := time.ParseDuration(pu.Conf.ContinuableTimeoutDefault) // validated at startup
	if val := gjson.Get(op.Meta, "timeout"); val.Exists() {
		switch val.Type {
		case gjson.Number:
			budget = time.Duration(val.Int()) * time.Millisecond
		case gjson.String:
			if parsed, perr := time.ParseDuration(val.String()); perr == nil {
				budget = parsed
			}
		}
	}
	return budget
}

// opJoinAtScope reads the `WITH join_at_scope = <int>` deferred-join floor.
// Returns (scope, true) when set; (0, false) when absent (the op is a
// same-scope async op, today's immediate barrier).
func opJoinAtScope(op operation.Operation) (int, bool) {
	val := gjson.Get(op.Meta, "join_at_scope")
	if !val.Exists() {
		return 0, false
	}
	return int(val.Int()), true
}
