package processor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// A NODE is a chassis that another chassis — its PARENT — dispatched a run
// to. The run arrives as an ordinary web request carrying the run's grant
// as a token in one header; the web head moves it out of the envelope and
// into the run's context, where nothing a rule writes can reach it, and
// `EXEC "cap://<name>"` presents it to the parent with each call up.
//
// `cap://<name>` is the node's one way to ask its parent for something:
//
//	WITH input   = &object(…)     what the capability is asked, as JSON
//	     into    = "_answer"      where the parent's output lands (default _cap)
//	     timeout = 60000          the whole round trip
//	EXEC "cap://card.note"
//
// `WITH name = <expr>` replaces the ref's name, as `WITH url` does for an
// http op: a loop that picks the capability from data writes one rule.
//
// The parent decides the call against the run grant (its `_grant` stack)
// and runs it (its `_cap` stack). The result merges under `into`; every
// failure is DATA at `cap.error {code, message, status}` — no grant in this
// run (`txco_cap_denied`), no parent configured (`txco_cap_unconfigured`),
// the parent's refusal, a transport failure — so a rule can gate on it and
// the loop that called it can go on. Only a malformed name is an authoring
// error that drops the op.

// RunGrantHeader carries a run grant's token on a request to a node.
const RunGrantHeader = "Txco-Run-Grant"

// callerRIDHeader carries the node run's rid up, so the parent's `_cap` run
// can be joined to it.
const callerRIDHeader = "Txco-Caller-Rid"

const capDefaultInto = "_cap"

type ctxKeyRunGrant struct{}

// WithRunGrant puts a run grant's token in a run's context. Only the web
// head calls it, from the request header; the token is never in the
// envelope, so no rule and no trace can see it.
func WithRunGrant(ctx context.Context, token string) context.Context {
	if strings.TrimSpace(token) == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRunGrant{}, token)
}

// RunGrantFrom is the token the run was dispatched with, or "".
func RunGrantFrom(ctx context.Context) string {
	tok, _ := ctx.Value(ctxKeyRunGrant{}).(string)
	return tok
}

// capClient reaches the parent. It is not egress-guarded: the parent's
// address is the operator's (--parent-url), never a rule's, and in
// development it is a loopback address the guard would refuse.
var capClient = &http.Client{Transport: &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:        16,
	MaxIdleConnsPerHost: 16,
	IdleConnTimeout:     90 * time.Second,
	// The answer is JSON either way; a compressed one only costs a decode.
	DisableCompression: true,
}}

// capFailure is the in-band shape of a call that did not get an answer.
func capFailure(into, name, code, message string, status int, rid string) event.Payload {
	raw, _ := sjson.SetRaw("{}", into, "{}")
	raw, _ = sjson.Set(raw, "cap.name", name)
	raw, _ = sjson.Set(raw, "cap.error.code", code)
	raw, _ = sjson.Set(raw, "cap.error.message", message)
	if status > 0 {
		raw, _ = sjson.Set(raw, "cap.error.status", status)
	}
	if rid != "" {
		raw, _ = sjson.Set(raw, "cap.rid", rid)
	}
	return event.Payload{Raw: raw, Type: event.JSON}
}

// ExecCap runs a `cap://<name>` op: one call up to the parent.
func (pu *Unit) ExecCap(ctx context.Context, op operation.Operation) (event.Payload, error) {
	empty := event.Payload{Raw: `{}`, Type: event.JSON}
	name := strings.TrimPrefix(op.Resonator.Exec, "cap://")
	if !sandbox.ValidCapability(name) {
		return empty, fmt.Errorf("malformed capability ref %q; want cap://<name>, lowercase words joined by dots", op.Resonator.Exec)
	}
	into := boundedInto(gjson.Get(op.Meta, "into").String())
	if into == "" {
		into = capDefaultInto
	}
	// `WITH name` names the capability from data; the ref's name is the
	// static default. A name that is not one is data too, not an authoring
	// error: the rule wrote it from the model's choice.
	if v := gjson.Get(op.Meta, "name"); v.Exists() {
		name = strings.TrimSpace(v.String())
		if !sandbox.ValidCapability(name) {
			return capFailure(into, name, "txco_cap_unknown", "WITH name is not a capability name (lowercase words joined by dots)", 0, ""), nil
		}
	}
	parent := strings.TrimRight(strings.TrimSpace(pu.Conf.ParentURL), "/")
	if parent == "" {
		return capFailure(into, name, "txco_cap_unconfigured",
			"this chassis has no parent (--parent-url): nothing answers cap:// here", 0, ""), nil
	}
	if !strings.HasPrefix(parent, "http://") && !strings.HasPrefix(parent, "https://") {
		return capFailure(into, name, "txco_cap_unconfigured",
			"--parent-url must be http(s)://…", 0, ""), nil
	}
	token := RunGrantFrom(ctx)
	if token == "" {
		return capFailure(into, name, "txco_cap_denied",
			"this run was dispatched with no run grant: nothing to present to the parent", 0, ""), nil
	}

	// The input is the rule's `WITH input`, already resolved. Absent: an
	// empty object, so the parent's rules see a shape, not a hole.
	input := "{}"
	if v := gjson.Get(op.Meta, "input"); v.Exists() && v.Type != gjson.Null {
		input = v.Raw
	}
	body, _ := sjson.SetRaw("{}", "input", input)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parent+"/v1/cap/"+name, bytes.NewBufferString(body))
	if err != nil {
		return capFailure(into, name, "txco_cap_request", err.Error(), 0, ""), nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(RunGrantHeader, token)
	ua := "txco"
	if v, ok := ctx.Value(config.CtxKeyVersion).(string); ok && v != "" {
		ua = "txco/" + v
	}
	req.Header.Set("User-Agent", ua)
	if rid, _ := ctx.Value(config.CtxKeyRid).(string); rid != "" {
		req.Header.Set(callerRIDHeader, rid)
	}

	start := time.Now()
	resp, err := capClient.Do(req)
	if err != nil {
		code := "txco_cap_transport"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			code = "txco_cap_timeout"
		}
		pu.Logger.Warn("cap: the call to the parent failed",
			zap.String("capability", name), zap.String("code", code), zap.Error(err))
		return capFailure(into, name, code, scrubToken(err.Error(), token), 0, ""), nil
	}
	defer resp.Body.Close()
	limit := int64(pu.Conf.OpPayloadMax)
	if limit <= 0 {
		limit = 4 << 20
	}
	answer, rerr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	rid := resp.Header.Get("X-Request-ID")
	if rerr != nil {
		return capFailure(into, name, "txco_cap_transport", "reading the parent's answer: "+rerr.Error(), resp.StatusCode, rid), nil
	}
	if int64(len(answer)) > limit {
		return capFailure(into, name, "txco_cap_too_large", "the parent's answer is over --op-payload-max", resp.StatusCode, rid), nil
	}
	ms := time.Since(start).Milliseconds()

	ok := resp.StatusCode == http.StatusOK && gjson.ValidBytes(answer) && gjson.GetBytes(answer, "ok").Bool()
	if !ok {
		code := "txco_cap_" + errorClass(resp.StatusCode)
		message := ""
		if gjson.ValidBytes(answer) {
			if c := strings.TrimSpace(gjson.GetBytes(answer, "error.code").String()); c != "" {
				code = codeFor(resp.StatusCode, c)
			}
			message = gjson.GetBytes(answer, "error.message").String()
		}
		if len(message) > 1000 {
			message = message[:1000]
		}
		pu.Logger.Info("cap: refused or failed",
			zap.String("capability", name), zap.Int("status", resp.StatusCode), zap.String("code", code), zap.Int64("ms", ms))
		return capFailure(into, name, code, scrubToken(message, token), resp.StatusCode, rid), nil
	}
	output := gjson.GetBytes(answer, "output")
	out := "{}"
	if output.Exists() {
		out = output.Raw
	}
	raw, werr := sjson.SetRaw("{}", into, out)
	if werr != nil {
		return capFailure(into, name, "txco_cap_answer", "the parent's output could not be placed at into", resp.StatusCode, rid), nil
	}
	raw, _ = sjson.Set(raw, "cap.name", name)
	raw, _ = sjson.Set(raw, "cap.status", resp.StatusCode)
	raw, _ = sjson.Set(raw, "cap.ms", ms)
	if rid != "" {
		raw, _ = sjson.Set(raw, "cap.rid", rid)
	}
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

// errorClass names a status the parent answered with, when its body did not.
func errorClass(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "denied"
	case http.StatusForbidden:
		return "denied"
	case http.StatusConflict:
		return "held"
	case http.StatusNotFound:
		return "unknown"
	case http.StatusGatewayTimeout:
		return "timeout"
	case http.StatusServiceUnavailable:
		return "unavailable"
	}
	return "http_" + fmt.Sprint(status)
}

// codeFor maps the parent's own code to the node's vocabulary: the inlet's
// classes become txco_cap_* so a rule matches one grammar; a `_cap` stack's
// own error code (a 422) is carried as it is.
func codeFor(status int, code string) string {
	switch code {
	case "unauthorized", "denied":
		return "txco_cap_denied"
	case "held":
		return "txco_cap_held"
	case "no_capability":
		return "txco_cap_unknown"
	case "unavailable", "timeout", "too_large", "bad_request":
		return "txco_cap_" + code
	}
	if status == http.StatusUnprocessableEntity {
		return code
	}
	return "txco_cap_" + code
}

// scrubToken keeps the token out of anything that reaches the envelope,
// should a transport error ever quote a header.
func scrubToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[run grant]")
}

// capInput is a test seam: the body a cap:// op would send.
func capInput(meta string) json.RawMessage {
	if v := gjson.Get(meta, "input"); v.Exists() && v.Type != gjson.Null {
		return json.RawMessage(v.Raw)
	}
	return json.RawMessage(`{}`)
}
