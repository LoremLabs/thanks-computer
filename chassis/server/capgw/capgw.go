// Package capgw is the capability inlet: `POST /v1/cap/<name>` on the web
// head, where dispatched work — a run on a NODE, a chassis of its own that
// this chassis dispatched a run to — asks this chassis to do something on
// its behalf: call a model, note a card, finish the run. The request
// carries the run's grant as a token (Txco-Run-Grant) and the call's input
// as JSON; the grant gateway decides it (chassis/server/grantgw.Invoke) and,
// when it is allowed, the tenant's `_cap` stack runs it as an ordinary
// pipeline run (`@src == "cap"`, the call at `@cap.*`) and answers at the
// top-level `_cap.output` — or `_cap.error {code, message}`.
//
// The inlet owns the transport and the identity; the stacks own the
// policy (`_grant`) and the work (`_cap`). The chassis does not know what a
// capability does.
//
// Answers, always JSON:
//
//	200 {"ok":true,"output":…}
//	401 {"ok":false,"error":{"code":"unauthorized"}}   no token, or not a live grant's
//	403 {"ok":false,"error":{"code":"denied"}}         refused: allowlist, standing grant, budget, a rule
//	409 {"ok":false,"error":{"code":"held"}}           a rule held it for a person
//	404 {"ok":false,"error":{"code":"no_capability"}}  no `_cap` rule answered
//	422 {"ok":false,"error":{"code":…,"message":…}}    the stack's own error
//	503 {"ok":false,"error":{"code":"unavailable"}}    the chassis could not decide or run
//
// A refusal says only that it was refused: the trace of the `_grant` run
// has the reason.
package capgw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/server/grantgw"
)

const (
	srcName   = "cap"
	stackName = "_cap"
	// Header is where the token travels. The node's web head reads the same
	// header off the dispatching request and keeps it out of its envelope.
	Header = processor.RunGrantHeader
	// CallerRIDHeader names the run that is calling, on the node: stamped
	// as `@cap.caller.rid` so a run can be followed across the two chassis.
	CallerRIDHeader = "Txco-Caller-Rid"

	defaultMaxBody = 4 << 20
)

// Gateway is the inlet.
type Gateway struct {
	ctx     context.Context
	log     *zap.Logger
	maxWait time.Duration // the `_cap` run's ceiling (OpTimeoutMax)
	maxBody int64

	// Seams for tests; New wires the real ones.
	invoke func(ctx context.Context, c grantgw.Call) grantgw.Verdict
	run    func(ctx context.Context, payload string) (string, error)
}

// New builds the inlet over the grant gateway and the processor's bus.
func New(ctx context.Context, pu *processor.Unit, grants *grantgw.Gateway) *Gateway {
	maxWait, err := time.ParseDuration(pu.Conf.OpTimeoutMax)
	if err != nil || maxWait <= 0 {
		maxWait = 10 * time.Minute
	}
	maxBody := int64(pu.Conf.OpPayloadMax)
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	g := &Gateway{ctx: ctx, log: pu.Logger, maxWait: maxWait, maxBody: maxBody}
	if g.log == nil {
		g.log = zap.NewNop()
	}
	g.invoke = grants.Invoke
	life := ctx
	g.run = func(ctx context.Context, payload string) (string, error) {
		return grantgw.RunOnBus(ctx, life, pu.Bus, payload)
	}
	return g
}

type answer struct {
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  *answerError    `json:"error,omitempty"`
}

type answerError struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func write(w http.ResponseWriter, status int, a answer) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(a)
}

func fail(w http.ResponseWriter, status int, code, message string) {
	write(w, status, answer{Error: &answerError{Code: code, Message: message}})
}

// Handle serves POST /v1/cap/{name}. Registered ahead of the web catch-all:
// no BasicAuth (the token is the whole authorization), no opstack render
// path.
func (g *Gateway) Handle(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	name := strings.TrimSpace(mux.Vars(r)["name"])
	if !sandbox.ValidCapability(name) {
		fail(w, http.StatusNotFound, "no_capability", "not a capability name")
		return
	}
	token := strings.TrimSpace(r.Header.Get(Header))
	if token == "" {
		fail(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, g.maxBody+1))
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "reading the body")
		return
	}
	if int64(len(body)) > g.maxBody {
		fail(w, http.StatusRequestEntityTooLarge, "too_large", "")
		return
	}
	input := json.RawMessage(`{}`)
	if len(strings.TrimSpace(string(body))) > 0 {
		if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
			fail(w, http.StatusBadRequest, "bad_request", "the body must be a JSON object: {\"input\": …}")
			return
		}
		if in := gjson.GetBytes(body, "input"); in.Exists() {
			input = json.RawMessage(in.Raw)
		}
	}

	v := g.invoke(r.Context(), grantgw.Call{Token: token, Name: name, Input: input})
	switch {
	case v.Unavailable:
		fail(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	case v.Held:
		fail(w, http.StatusConflict, "held", "")
		return
	case !v.Allowed && !v.Identified():
		fail(w, http.StatusUnauthorized, "unauthorized", "")
		return
	case !v.Allowed:
		fail(w, http.StatusForbidden, "denied", "")
		return
	}

	rid := hxid.NewTimeSort().String()
	w.Header().Set("X-Request-ID", rid)
	pb := jsonx.New()
	pb.Set("_txc.src", srcName)
	pb.Set("_txc.rid", rid)
	pb.Set("_txc.cap.tenant", v.Tenant)
	pb.Set("_txc.cap.name", name)
	pb.Set("_txc.cap.run", v.Grant.Run)
	pb.Set("_txc.cap.grant", v.Grant.ID)
	pb.Set("_txc.cap.generation", v.Grant.Generation)
	pb.Set("_txc.cap.principal.id", v.Grant.Principal.ID)
	pb.Set("_txc.cap.principal.kind", string(v.Grant.Principal.Kind))
	pb.Set("_txc.cap.stack", v.Grant.Stack)
	if v.Grant.Workspace != "" {
		pb.Set("_txc.cap.workspace", v.Grant.Workspace)
	}
	if v.Grant.TraceID != "" {
		pb.Set("_txc.cap.trace", v.Grant.TraceID)
	}
	if caller := strings.TrimSpace(r.Header.Get(CallerRIDHeader)); caller != "" && len(caller) <= 64 {
		pb.Set("_txc.cap.caller.rid", caller)
	}
	pb.SetRaw("_txc.cap.input", string(input))
	pb.Set("_ts", start.UTC().Format(time.RFC3339))

	// The `_cap` run: a context of its own, off the inlet's, with the
	// caller's rid; it ends when the caller goes away and at the ceiling.
	rctx, cancel := context.WithTimeout(g.ctx, g.maxWait)
	rctx = context.WithValue(rctx, config.CtxKeyRid, rid)
	stop := context.AfterFunc(r.Context(), cancel)
	final, runErr := g.run(rctx, pb.String())
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
	stop()
	cancel()

	fields := []zap.Field{
		zap.String("rid", rid), zap.String("tenant", v.Tenant), zap.String("capability", name),
		zap.String("run", v.Grant.Run), zap.String("grant", v.Grant.ID),
		zap.Duration("took", time.Since(start)),
	}
	switch {
	case timedOut:
		g.log.Warn("cap: the _cap run timed out", fields...)
		fail(w, http.StatusGatewayTimeout, "timeout", "")
		return
	case runErr != nil:
		g.log.Warn("cap: the _cap run failed", append(fields, zap.Error(runErr))...)
		fail(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	f := gjson.GetMany(final, "_txc.route.unavailable", "_txc.admission.denied", "_cap.error", "_cap.output")
	switch {
	case f[0].Bool():
		g.log.Warn("cap: the tenant could not be routed", fields...)
		fail(w, http.StatusServiceUnavailable, "unavailable", "")
	case f[1].Bool():
		g.log.Info("cap: the tenant was refused admission", fields...)
		fail(w, http.StatusServiceUnavailable, "unavailable", "")
	case f[2].Exists() && f[2].Type != gjson.Null:
		code := strings.TrimSpace(f[2].Get("code").String())
		if code == "" {
			code = "error"
		}
		msg := f[2].Get("message").String()
		if len(msg) > 1000 {
			msg = msg[:1000]
		}
		g.log.Info("cap: the stack answered an error", append(fields, zap.String("code", code))...)
		fail(w, http.StatusUnprocessableEntity, code, msg)
	case f[3].Exists():
		g.log.Info("cap: answered", fields...)
		write(w, http.StatusOK, answer{OK: true, Output: json.RawMessage(f[3].Raw)})
	default:
		g.log.Info("cap: no _cap rule answered", fields...)
		fail(w, http.StatusNotFound, "no_capability", "no rule in the tenant's _cap stack answered "+name)
	}
}
