// Package capgw is the capability inlet: `POST /v1/cap/<name>` on the web
// head, where dispatched work — a run on a NODE, a chassis of its own that
// this chassis dispatched a run to — asks this chassis to do something on
// its behalf: call a model, note a card, finish the run. The request
// carries the run's grant as a token (Txco-Run-Grant) and the call's input
// as JSON; the grant gateway decides it (chassis/server/grantgw.Invoke) and,
// when it is allowed, the stack that DECLARES the capability runs it as an
// ordinary pipeline run (`@src == "cap"`, the call at `@cap.*`), entered at
// the stack's start — or at the scope its declaration names (CAPS/<name>.yaml,
// chassis/capdecl) — and answers at the top-level `_cap.output` — or
// `_cap.error {code, message}`.
// A name no active stack declares still runs the tenant's `_cap` stack from
// its first scope: the router a tenant wrote before declarations existed.
//
// The inlet owns the transport and the identity; the stacks own the
// policy (`_grant`) and the work (the declaring stack). The chassis does
// not know what a capability does.
//
// Answers, always JSON. The decision is answered with its status:
//
//	401 {"ok":false,"error":{"code":"unauthorized"}}   no token, or not a live grant's
//	403 {"ok":false,"error":{"code":"denied"}}         refused: allowlist, standing grant, budget, a rule
//	409 {"ok":false,"error":{"code":"held"}}           a rule held it for a person
//	404 {"ok":false,"error":{"code":"no_capability"}}  not a capability name
//	503 {"ok":false,"error":{"code":"unavailable"}}    the chassis could not decide
//
// An ALLOWED call answers 200 at once — headers sent and flushed before the
// capability's run starts — and the body when the run ends. A model call can take
// a minute; a proxy on the way (the fleet's edge allows 20 s for response
// headers) must see headers, and then bytes: until the body, the inlet
// writes one space every keepalive interval. So what the run came to is in
// the body, with the status it would otherwise have carried:
//
//	200 {"ok":true,"output":…}
//	200 {"ok":false,"error":{"code":"no_capability","status":404}}        no rule answered
//	200 {"ok":false,"error":{"code":…,"message":…,"status":422}}         the stack's own error
//	200 {"ok":false,"error":{"code":"timeout","status":504}}             the run passed its ceiling
//	200 {"ok":false,"error":{"code":"unavailable","status":503}}         the run failed, or the tenant could not be routed
//
// A refusal says only that it was refused: the trace of the `_grant` run
// has the reason. A client reads `error.status` as the status (the node's
// cap:// does); the leading whitespace is JSON's own.
package capgw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
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
	// keepalive is how often a space goes to the client while the
	// capability's run is still running; well under any proxy's idle limit.
	keepalive = 10 * time.Second
)

// Gateway is the inlet.
type Gateway struct {
	ctx     context.Context
	log     *zap.Logger
	maxWait time.Duration // the run's ceiling (OpTimeoutMax)
	maxBody int64

	// Seams for tests; New wires the real ones.
	invoke func(ctx context.Context, c grantgw.Call) grantgw.Verdict
	run    func(ctx context.Context, payload string) (string, error)
	// lookup finds the stack that declares a capability; nil routes every
	// call to the tenant's `_cap` stack.
	lookup func(ctx context.Context, tenant, name string) (string, *capdecl.Decl, error)
}

// Decls is where the inlet finds who answers a capability: the tenant's
// active stack that declares the name, and the declaration. It answers
// capdecl.ErrNotDeclared for a name no active stack declares.
type Decls interface {
	Lookup(ctx context.Context, tenant, name string) (stack string, d *capdecl.Decl, err error)
}

// New builds the inlet over the grant gateway, the capability declarations
// and the processor's bus. A nil decls routes every call to `_cap`.
func New(ctx context.Context, pu *processor.Unit, grants *grantgw.Gateway, decls Decls) *Gateway {
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
	if decls != nil {
		g.lookup = decls.Lookup
	}
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
	// Status is the HTTP status this failure would have carried, when the
	// headers (200) went out before the run ended.
	Status int `json:"status,omitempty"`
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

// early is a 200 whose headers went out before the body was known: it
// keeps the client's connection fed with a space every keepalive interval
// until the body is written. finish writes the body exactly once and stops
// the feeding; a failure carries the status it would have had.
type early struct {
	w    http.ResponseWriter
	mu   sync.Mutex
	done chan struct{}
	ok   bool
}

func startEarly(w http.ResponseWriter) *early {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	e := &early{w: w, done: make(chan struct{})}
	e.flush()
	go e.feed()
	return e
}

// flush reaches the listener through any middleware wrapper (the web
// head's wraps the writer and exposes Unwrap for exactly this): a type
// assertion on the wrapper would find no Flusher and the headers would
// wait for the body.
func (e *early) flush() {
	_ = http.NewResponseController(e.w).Flush()
}

func (e *early) feed() {
	t := time.NewTicker(keepalive)
	defer t.Stop()
	for {
		select {
		case <-e.done:
			return
		case <-t.C:
			e.mu.Lock()
			if !e.ok {
				_, _ = io.WriteString(e.w, " ")
				e.flush()
			}
			e.mu.Unlock()
		}
	}
}

func (e *early) finish(a answer) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ok {
		return
	}
	e.ok = true
	close(e.done)
	_ = json.NewEncoder(e.w).Encode(a)
	e.flush()
}

func (e *early) fail(status int, code, message string) {
	e.finish(answer{Error: &answerError{Code: code, Message: message, Status: status}})
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

	// Who answers: the active stack that declares the name, entered at its
	// start or at the scope its declaration names. The route is the inlet's to stamp —
	// detect-tenant has no declarations to read — and a declared timeout can
	// only shorten the wait. A name nobody declares goes to `_cap`.
	wait := g.maxWait
	var implStack, implTo string
	if g.lookup != nil {
		switch stack, d, lerr := g.lookup(r.Context(), v.Tenant, name); {
		case lerr == nil:
			implStack, implTo = stack, d.Stage(stack)
			if t := d.TimeoutDuration(); t > 0 && t < wait {
				wait = t
			}
		case errors.Is(lerr, capdecl.ErrNotDeclared):
		default:
			g.log.Warn("cap: the capability's declaration could not be read",
				zap.String("tenant", v.Tenant), zap.String("capability", name), zap.Error(lerr))
			fail(w, http.StatusServiceUnavailable, "unavailable", "")
			return
		}
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
	if implTo != "" {
		pb.Set("_txc.cap.impl.stack", implStack)
		pb.Set("_txc.cap.impl.to", implTo)
	}
	pb.SetRaw("_txc.cap.input", string(input))
	pb.Set("_ts", start.UTC().Format(time.RFC3339))

	// Allowed: the headers go out now (200, flushed), and the body when the
	// run ends. From here every failure is in the body, with its status.
	e := startEarly(w)

	// The capability's run: a context of its own, off the inlet's, with the
	// caller's rid; it ends when the caller goes away and at the ceiling.
	rctx, cancel := context.WithTimeout(g.ctx, wait)
	rctx = context.WithValue(rctx, config.CtxKeyRid, rid)
	stop := context.AfterFunc(r.Context(), cancel)
	final, runErr := g.run(rctx, pb.String())
	timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded)
	stop()
	cancel()

	fields := []zap.Field{
		zap.String("rid", rid), zap.String("tenant", v.Tenant), zap.String("capability", name),
		zap.String("run", v.Grant.Run), zap.String("grant", v.Grant.ID),
		zap.String("impl", implTo), zap.Duration("took", time.Since(start)),
	}
	switch {
	case timedOut:
		g.log.Warn("cap: the run timed out", fields...)
		e.fail(http.StatusGatewayTimeout, "timeout", "")
		return
	case runErr != nil:
		g.log.Warn("cap: the run failed", append(fields, zap.Error(runErr))...)
		e.fail(http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	f := gjson.GetMany(final, "_txc.route.unavailable", "_txc.admission.denied", "_cap.error", "_cap.output")
	switch {
	case f[0].Bool():
		g.log.Warn("cap: the tenant could not be routed", fields...)
		e.fail(http.StatusServiceUnavailable, "unavailable", "")
	case f[1].Bool():
		g.log.Info("cap: the tenant was refused admission", fields...)
		e.fail(http.StatusServiceUnavailable, "unavailable", "")
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
		e.fail(http.StatusUnprocessableEntity, code, msg)
	case f[3].Exists():
		g.log.Info("cap: answered", fields...)
		e.finish(answer{OK: true, Output: json.RawMessage(f[3].Raw)})
	default:
		g.log.Info("cap: no rule answered", fields...)
		where := "the tenant's _cap stack"
		if implTo != "" {
			where = implTo + " (the declaring stack's entry)"
		}
		e.fail(http.StatusNotFound, "no_capability", "no rule in "+where+" answered "+name)
	}
}
