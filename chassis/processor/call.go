package processor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/opname"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/txcguard"
)

// `EXEC "call://<stack>[/<scope>]"` runs another stack IN PROCESS, inside
// the same request, and answers with one result object:
//
//	WITH input   = &object(…)     what the callee is given (default {})
//	     into    = "_orient"      where the result lands (default _call)
//	     timeout = 20000          the call's own limit (ms)
//	     stack   = <expr>         replaces the ref's stack, as WITH name does
//	                              for cap://
//	EXEC "call://app/_orient"
//
// The callee starts from a fresh envelope — its identity facts plus
// `@call.input` — runs its named stack and only it (never a parent's
// scopes), and when it ends the caller finds under `into`:
//
//	{ok, output, error, stack, version, ms}
//
// `ok` is the chassis's verdict; `output` is what the callee wrote at
// `_call.output`; `error` is its `_call.error` or the engine's own
// (txco_call_*). Nothing else of the callee's envelope comes back, and
// nothing outside `into` is written, so two calls in one scope cannot
// collide. A `@halt` in the callee ends only the callee.
//
// A call isolates DATA, not AUTHORITY: the callee runs with the request's
// tenant, principal, secrets and run grant. It is not a security boundary.
//
// The frame (frame.go) is what makes a call a call: it carries the boundary,
// the policies the version-0 engine refuses (streaming, suspension), the
// frame's own transition history, and the ops in flight the engine waits on
// when the call is cancelled. The same engine is meant to serve a capability
// answered on the chassis that declares it; see docs/todo-stack-call.md in
// the service repo.

const (
	callDefaultInto    = "_call"
	callDepthDefault   = 8
	callDefaultTimeout = 60 * time.Second
	callDefaultGrace   = 2 * time.Second
)

// callError is the `error` member of a result object: the callee's own
// `_call.error`, or the engine's verdict. Stopped and Running are set only
// on a cancelled call (§2.9): whether the callee's ops had all returned
// within the grace, and which were still running if not.
type callError struct {
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Stopped *bool    `json:"stopped,omitempty"`
	Running []string `json:"running,omitempty"`
}

// callResult is the one object a call writes under `into`.
type callResult struct {
	OK      bool            `json:"ok"`
	Output  json.RawMessage `json:"output"`
	Error   *callError      `json:"error"`
	Stack   string          `json:"stack"`
	Version int64           `json:"version"`
	MS      int64           `json:"ms"`
}

// callPayload places the result object at into, and nothing else.
func callPayload(into string, r callResult) event.Payload {
	if len(r.Output) == 0 {
		r.Output = json.RawMessage(`{}`)
	}
	b, _ := json.Marshal(r)
	raw, err := txcguard.BoundedSetRaw(`{}`, into, string(b))
	if err != nil {
		raw, _ = sjson.SetRaw(`{}`, "_call", string(b))
	}
	return event.Payload{Raw: raw, Type: event.JSON}
}

// callDepthLimit is how deep calls may nest: 8, or --call-depth-max when
// that is lower. The flag can only tighten it.
func (pu *Unit) callDepthLimit() int {
	limit := callDepthDefault
	if n := pu.Conf.CallDepthMax; n > 0 && n < limit {
		limit = n
	}
	return limit
}

// callStopGrace is how long a cancelled call waits for its callee's ops to
// return before answering with stopped: false.
func (pu *Unit) callStopGrace() time.Duration {
	if d, err := time.ParseDuration(pu.Conf.CallStopGrace); err == nil && d >= 0 {
		return d
	}
	return callDefaultGrace
}

// ExecCall runs a `call://` op: one stack called from another, in process.
func (pu *Unit) ExecCall(ctx context.Context, op operation.Operation) (event.Payload, error) {
	start := time.Now()
	stack, scope, err := opname.CallTarget(op.Resonator.Exec)
	if err != nil {
		// An authoring error: the parser refuses it at apply, and this is
		// the same check at dispatch. The op is dropped, like a bad goto.
		return event.Payload{Raw: `{}`, Type: event.JSON}, err
	}
	into := boundedInto(gjson.Get(op.Meta, "into").String())
	if into == "" {
		into = callDefaultInto
	}
	fail := func(code, message string) (event.Payload, error) {
		return callPayload(into, callResult{
			Error: &callError{Code: code, Message: message},
			Stack: stack, MS: time.Since(start).Milliseconds(),
		}), nil
	}

	// `WITH stack` names the callee from data; the ref's stack is the
	// static default. A value that is not a stack name is data too, not an
	// authoring error: the rule computed it.
	if v := gjson.Get(op.Meta, "stack"); v.Exists() {
		stack = strings.TrimSpace(v.String())
		if verr := opname.ValidStack(stack); verr != nil {
			return fail("txco_call_target", "WITH stack is not a stack name: "+verr.Error())
		}
	}
	input := `{}`
	if v := gjson.Get(op.Meta, "input"); v.Exists() && v.Type != gjson.Null {
		input = v.Raw
	}

	parent := frameFrom(ctx)
	if parent.callDepth()+1 > pu.callDepthLimit() {
		return fail("txco_call_depth", fmt.Sprintf("calls nest %d deep; the limit is %d", parent.callDepth()+1, pu.callDepthLimit()))
	}
	fr := newCallFrame(parent, stack, input, frameCaller{
		Stack: op.Stack, Stage: op.Stack + "/" + strconv.Itoa(op.Scope), Op: op.Name,
	})

	// The callee's context: the caller's — same deadline, tenant, budget,
	// tracer, secrets — with the frame's own facts in place of the caller's.
	// Each of these keys would otherwise be inherited by the nested Run,
	// which skips its first-Run block because the snapshot is already
	// pinned (design §2.6).
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fr.ctx = cctx
	cctx = withFrame(cctx, fr)
	cctx = WithSource(cctx, "call")
	cctx = context.WithValue(cctx, ctxKeyParentStage, "")
	cctx = withStreamSink(cctx, nil)
	cctx = withResumeRun(cctx, resumeIdent{})
	cctx = context.WithValue(cctx, ctxKeyDeferredRun, deferredIdent{})

	// A missing callee is a result error, not an authoring one: the callee
	// deploys separately. Looked up under the frame, so a parent's scopes
	// cannot stand in for it.
	probe, perr := pu.OpsForStage(cctx, stack+"/0")
	if perr != nil {
		return fail("txco_call_failed", "looking up "+stack+": "+perr.Error())
	}
	if len(probe) == 0 {
		return fail("txco_call_no_stack", "no stack "+stack+" in this tenant")
	}
	tenant := tenantScope(ctx)
	version := pu.stackVersion(cctx, tenant, stack)
	entry := stack + "/" + strconv.Itoa(scope)

	tr := trace.FromContext(ctx)
	tr.Event(trace.TimelineEvent{
		Ts: time.Now(), Event: "call.enter",
		Fields: map[string]any{
			"stack": stack, "stage": entry, "version": version, "depth": fr.depth,
			"caller": fr.caller.Stage + "/" + fr.caller.Op, "input_bytes": len(input),
		},
	})

	// Capture. Run's sends block until read, and it may send more than one
	// payload or none, so the frame's channel is drained concurrently and
	// the first payload is the answer.
	resCh := make(chan event.Payload)
	var captured []event.Payload
	stop := make(chan struct{})
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			select {
			case p := <-resCh:
				captured = append(captured, p)
			case <-stop:
				return
			}
		}
	}()
	runErr := pu.Run(cctx, pu.callEnvelope(cctx, op, fr), entry, resCh)
	close(stop)
	<-drained

	// What the call came to, in order of cause: a boundary or policy
	// refusal the frame recorded; the deadline or a cancellation, after
	// waiting for the callee's ops to stop; the budget; then what the
	// callee answered.
	ce := fr.failure()
	if cctx.Err() != nil {
		cancel()
		stopped, running := fr.waitOps(pu.callStopGrace())
		if ce == nil {
			code, msg := "txco_call_cancelled", stack+" was cancelled"
			if errors.Is(cctx.Err(), context.DeadlineExceeded) {
				code, msg = "txco_call_timeout", stack+" ran past its deadline"
			}
			ce = &callError{Code: code, Message: msg}
		}
		ce.Stopped = &stopped
		ce.Running = running
	}
	if ce == nil {
		ce = budgetExhaustedInCall(ctx, entry)
	}
	final := ""
	if ce == nil {
		switch {
		case len(captured) == 0 && runErr != nil:
			ce = &callError{Code: "txco_call_failed", Message: runErr.Error()}
		case len(captured) == 0:
			// Nothing emitted and no error: an empty answer.
		case captured[0].Type == event.JSON:
			final = captured[0].Raw
		case captured[0].Type == event.ErrorStr:
			msg := gjson.Get(captured[0].Raw, "error.message").String()
			if msg == "" {
				msg = captured[0].Raw
			}
			ce = &callError{Code: "txco_call_failed", Message: msg}
		default:
			ce = &callError{Code: "txco_call_unsupported", Message: stack + " streamed its answer; a call cannot stream"}
		}
	}
	output := `{}`
	if final != "" {
		if o := gjson.Get(final, "_call.output"); o.Exists() && o.Type != gjson.Null {
			output = o.Raw
		}
		if e := gjson.Get(final, "_call.error"); ce == nil && e.Exists() && e.Type != gjson.Null {
			ce = &callError{Code: e.Get("code").String(), Message: e.Get("message").String()}
			if e.Type == gjson.String {
				ce.Message = e.String()
			}
			if ce.Code == "" {
				ce.Code = "txco_call_error"
			}
		}
	}
	ms := time.Since(start).Milliseconds()
	fields := map[string]any{
		"stack": stack, "version": version, "depth": fr.depth, "ms": ms, "ok": ce == nil,
	}
	if ce != nil {
		fields["code"] = ce.Code
		if ce.Stopped != nil {
			fields["stopped"] = *ce.Stopped
		}
		pu.Logger.Debug("call: returned with an error",
			zap.String("stack", stack), zap.String("code", ce.Code), zap.String("message", ce.Message))
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "call.return", Fields: fields})
	return callPayload(into, callResult{
		OK: ce == nil, Output: json.RawMessage(output), Error: ce,
		Stack: stack, Version: version, MS: ms,
	}), nil
}

// callEnvelope is the fresh document a callee starts from: the identity
// facts the chassis stamps, and the input. Rules cannot write `_txc`, so a
// callee cannot forge its own input or caller; anything else it needs
// arrives in `input`. The tenant and rid come from the request's pins, with
// the caller's envelope as the fallback for a context that has none (tests,
// untenanted paths).
func (pu *Unit) callEnvelope(ctx context.Context, op operation.Operation, fr *frame) string {
	call := map[string]any{"input": json.RawMessage(fr.input), "caller": fr.caller, "depth": fr.depth}
	txc := map[string]any{"src": fr.source, "stack": fr.stack, "call": call}
	if t := tenantScope(ctx); t != "" {
		txc["tenant"] = t
	} else if t := gjson.Get(op.Input, "_txc.tenant").String(); t != "" {
		txc["tenant"] = t
	}
	if rid, _ := ctx.Value(config.CtxKeyRid).(string); rid != "" {
		txc["rid"] = rid
	} else if rid := gjson.Get(op.Input, "_txc.rid").String(); rid != "" {
		txc["rid"] = rid
	}
	raw, _ := sjson.Set(`{}`, "_txc", txc)
	if ts := gjson.Get(op.Input, "_ts"); ts.Exists() {
		raw, _ = sjson.SetRaw(raw, "_ts", ts.Raw)
	}
	return stampPrincipal(ctx, raw)
}

// budgetExhaustedInCall reports the request's budget state after a callee
// ran, as a call error: the callee that spent the last of the fuel or the
// last hop ended on the exhaustion, and the caller learns it here before
// its own next charge refuses it too.
func budgetExhaustedInCall(ctx context.Context, stage string) *callError {
	s := budgetFromCtx(ctx)
	if s == nil {
		return nil
	}
	switch e := s.overBudget(s.fuel.Load(), stage).(type) {
	case nil:
	case *FuelExhaustedError:
		return &callError{Code: "txco_fuel_exhausted", Message: e.Error()}
	case *AllowanceExhaustedError:
		return &callError{Code: "txco_allowance_exhausted", Message: e.Error()}
	default:
		return &callError{Code: "txco_fuel_exhausted", Message: e.Error()}
	}
	if s.maxTTL > 0 && s.ttl.Load() <= 0 {
		return &callError{Code: "txcl_scope_ttl_exhausted",
			Message: fmt.Sprintf("the request's %d stage hops were used up inside the call", s.maxTTL)}
	}
	return nil
}

// stackVersion is the callee stack's active version number in the request's
// pinned snapshot: what the trace and the result record so they say which
// code answered. 0 when it cannot be told (no tenant pinned, no such stack,
// no active version, a snapshot without the control tables). Remembered per
// (tenant, stack) on the snapshot's ops index when there is one.
func (pu *Unit) stackVersion(ctx context.Context, tenant, stack string) int64 {
	if tenant == "" {
		return 0
	}
	db := pu.opstackDB(ctx)
	idx := pu.currentOpsIndex(db)
	key := tenant + "\x00" + stack
	if idx != nil {
		if v, ok := idx.versions.Load(key); ok {
			return v.(int64)
		}
	}
	var n sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT v.version_number
		  FROM stacks s
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		  JOIN stack_versions v ON v.version_id = s.active_version
		 WHERE t.slug = ? AND s.name = ?`, tenant, stack).Scan(&n)
	if err != nil {
		return 0
	}
	if idx != nil {
		idx.versions.Store(key, n.Int64)
	}
	return n.Int64
}
