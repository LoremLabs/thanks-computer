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
// The engine below (runFrame) is what makes a call a call: a frame
// (frame.go) carrying the boundary, the policies the version-0 engine
// refuses (streaming, suspension), the frame's own transition history, and
// the ops in flight the engine waits on when the call is cancelled. The same
// engine serves a capability answered on the chassis that declares it
// (cap.go, execCapLocal): the two ways in differ only in what they stamp
// and how they shape the answer. See docs/todo-stack-call.md in the service
// repo.

const (
	callDefaultInto    = "_call"
	callDepthDefault   = 8
	callDefaultTimeout = 60 * time.Second
	callDefaultGrace   = 2 * time.Second
)

// The engine's verdicts on a frame, short; each way in composes its own
// code from them (txco_call_<verdict>, txco_cap_<verdict>).
const (
	verdictNoStack     = "no_stack"    // no stack of that name in the tenant
	verdictBoundary    = "boundary"    // the callee jumped out of its stack
	verdictUnsupported = "unsupported" // the callee tried to suspend or stream
	verdictTimeout     = "timeout"     // the deadline
	verdictCancelled   = "cancelled"   // the request ended the call
	verdictDepth       = "depth"       // calls nested past the limit
	verdictFailed      = "failed"      // the callee's run failed without an answer
	verdictBudget      = "budget"      // the request's budget ran out inside the callee
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

	out := pu.runFrame(ctx, op, frameSpec{
		stack: stack, scope: scope, source: "call", channel: callDefaultInto,
		input: metaInput(op.Meta), what: stack,
	})
	var ce *callError
	switch {
	case out.verdict == verdictBudget:
		ce = &callError{Code: out.budget, Message: out.message}
	case out.verdict != "":
		ce = &callError{Code: "txco_call_" + out.verdict, Message: out.message}
	case out.own != nil:
		ce = out.own
		if ce.Code == "" {
			ce.Code = "txco_call_error" // the callee said so but gave no code
		}
	}
	if ce != nil {
		ce.Stopped, ce.Running = out.stopped, out.running
	}
	return callPayload(into, callResult{
		OK: ce == nil, Output: json.RawMessage(out.output), Error: ce,
		Stack: stack, Version: out.version, MS: out.ms,
	}), nil
}

// metaInput is the rule's resolved `WITH input`, or {} so the callee's rules
// see a shape, not a hole.
func metaInput(meta string) string {
	if v := gjson.Get(meta, "input"); v.Exists() && v.Type != gjson.Null {
		return v.Raw
	}
	return `{}`
}

// frameSpec is what a way in asks of the engine: which stack to run and from
// where, under which source, with what input and which further facts, and
// the top-level key the callee answers at.
type frameSpec struct {
	stack   string
	scope   int
	source  string         // "call" or "cap": `_txc.src`, and the facts key `_txc.<source>`
	channel string         // the callee's answer key: "_call" or "_cap"
	input   string         // JSON
	facts   map[string]any // more facts under `_txc.<source>.*`, beside input, caller and depth
	timeout time.Duration  // > 0: bounds the frame further than the op's deadline does
	what    string         // the thing called, for messages: "app/_orient", "mail.send"
}

// frameOutcome is what the engine answers: its own verdict ("" when it has
// none), the callee's own error when it wrote one and the engine had no
// verdict, and the callee's output.
type frameOutcome struct {
	verdict  string
	budget   string // the budget's own code, when verdict is verdictBudget
	message  string
	stopped  *bool
	running  []string
	own      *callError // the callee's `<channel>.error`
	output   string     // the callee's `<channel>.output`, or {}
	answered bool       // the callee wrote an output or an error
	version  int64
	ms       int64
}

// runFrame runs spec.stack on a frame of its own inside this request and
// reads back what it answered. op is the rule that asked, named to the
// callee as its caller.
func (pu *Unit) runFrame(ctx context.Context, op operation.Operation, spec frameSpec) frameOutcome {
	start := time.Now()
	elapsed := func() int64 { return time.Since(start).Milliseconds() }

	parent := frameFrom(ctx)
	if parent.callDepth()+1 > pu.callDepthLimit() {
		return frameOutcome{verdict: verdictDepth, output: `{}`, ms: elapsed(),
			message: fmt.Sprintf("calls nest %d deep; the limit is %d", parent.callDepth()+1, pu.callDepthLimit())}
	}
	fr := newCallFrame(parent, spec.stack, spec.source, spec.input, frameCaller{
		Stack: op.Stack, Stage: op.Stack + "/" + strconv.Itoa(op.Scope), Op: op.Name,
	})

	// The callee's context: the caller's — same deadline, tenant, budget,
	// tracer, secrets — with the frame's own facts in place of the caller's.
	// Each of these keys would otherwise be inherited by the nested Run,
	// which skips its first-Run block because the snapshot is already
	// pinned (design §2.6). A way in may bound the frame further.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if spec.timeout > 0 {
		var tcancel context.CancelFunc
		cctx, tcancel = context.WithTimeout(cctx, spec.timeout)
		defer tcancel()
	}
	fr.ctx = cctx
	cctx = withFrame(cctx, fr)
	cctx = WithSource(cctx, spec.source)
	cctx = context.WithValue(cctx, ctxKeyParentStage, "")
	cctx = withStreamSink(cctx, nil)
	cctx = withResumeRun(cctx, resumeIdent{})
	cctx = context.WithValue(cctx, ctxKeyDeferredRun, deferredIdent{})

	// A missing callee is a result error, not an authoring one: the callee
	// deploys separately. Looked up under the frame, so a parent's scopes
	// cannot stand in for it.
	probe, perr := pu.OpsForStage(cctx, spec.stack+"/0")
	if perr != nil {
		return frameOutcome{verdict: verdictFailed, output: `{}`, ms: elapsed(), message: "looking up " + spec.stack + ": " + perr.Error()}
	}
	if len(probe) == 0 {
		return frameOutcome{verdict: verdictNoStack, output: `{}`, ms: elapsed(), message: "no stack " + spec.stack + " in this tenant"}
	}
	tenant := tenantScope(ctx)
	version := pu.stackVersion(cctx, tenant, spec.stack)
	entry := spec.stack + "/" + strconv.Itoa(spec.scope)

	tr := trace.FromContext(ctx)
	tr.Event(trace.TimelineEvent{
		Ts: time.Now(), Event: "call.enter",
		Fields: map[string]any{
			"via": spec.source, "what": spec.what, "stack": spec.stack, "stage": entry, "version": version,
			"depth": fr.depth, "caller": fr.caller.Stage + "/" + fr.caller.Op, "input_bytes": len(spec.input),
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
	runErr := pu.Run(cctx, pu.frameEnvelope(cctx, op, fr, spec), entry, resCh)
	close(stop)
	<-drained

	// What the frame came to, in order of cause: a boundary or policy
	// refusal the frame recorded; the deadline or a cancellation, after
	// waiting for the callee's ops to stop; the budget; then what the
	// callee answered.
	out := frameOutcome{output: `{}`, version: version}
	if f := fr.failure(); f != nil {
		out.verdict, out.message = f.Code, f.Message
	}
	if cctx.Err() != nil {
		cancel()
		stopped, running := fr.waitOps(pu.callStopGrace())
		if out.verdict == "" {
			out.verdict, out.message = verdictCancelled, spec.what+" was cancelled"
			if errors.Is(cctx.Err(), context.DeadlineExceeded) {
				out.verdict, out.message = verdictTimeout, spec.what+" ran past its deadline"
			}
		}
		out.stopped, out.running = &stopped, running
	}
	if out.verdict == "" {
		if code, msg := budgetExhaustedInCall(ctx, entry); code != "" {
			out.verdict, out.budget, out.message = verdictBudget, code, msg
		}
	}
	final := ""
	if out.verdict == "" {
		switch {
		case len(captured) == 0 && runErr != nil:
			out.verdict, out.message = verdictFailed, runErr.Error()
		case len(captured) == 0:
			// Nothing emitted and no error: an empty answer.
		case captured[0].Type == event.JSON:
			final = captured[0].Raw
		case captured[0].Type == event.ErrorStr:
			msg := gjson.Get(captured[0].Raw, "error.message").String()
			if msg == "" {
				msg = captured[0].Raw
			}
			out.verdict, out.message = verdictFailed, msg
		default:
			out.verdict, out.message = verdictUnsupported, spec.what+" streamed its answer; a call cannot stream"
		}
	}
	if final != "" {
		if o := gjson.Get(final, spec.channel+".output"); o.Exists() && o.Type != gjson.Null {
			out.output, out.answered = o.Raw, true
		}
		if e := gjson.Get(final, spec.channel+".error"); e.Exists() && e.Type != gjson.Null {
			out.answered = true
			if out.verdict == "" {
				out.own = &callError{Code: e.Get("code").String(), Message: e.Get("message").String()}
				if e.Type == gjson.String {
					out.own.Message = e.String()
				}
			}
		}
	}
	out.ms = elapsed()
	fields := map[string]any{
		"via": spec.source, "what": spec.what, "stack": spec.stack, "version": version,
		"depth": fr.depth, "ms": out.ms, "ok": out.verdict == "" && out.own == nil,
	}
	if out.verdict != "" {
		fields["verdict"] = out.verdict
		if out.stopped != nil {
			fields["stopped"] = *out.stopped
		}
		pu.Logger.Debug("call: returned with the engine's verdict",
			zap.String("via", spec.source), zap.String("stack", spec.stack),
			zap.String("verdict", out.verdict), zap.String("message", out.message))
	} else if out.own != nil {
		fields["code"] = out.own.Code
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "call.return", Fields: fields})
	return out
}

// frameEnvelope is the fresh document a callee starts from: the identity
// facts the chassis stamps, and the input. Rules cannot write `_txc`, so a
// callee cannot forge its own input or caller; anything else it needs
// arrives in `input`. The tenant and rid come from the request's pins, with
// the caller's envelope as the fallback for a context that has none (tests,
// untenanted paths). The way in's own facts go beside input, caller and
// depth under `_txc.<source>`.
func (pu *Unit) frameEnvelope(ctx context.Context, op operation.Operation, fr *frame, spec frameSpec) string {
	facts := map[string]any{"input": json.RawMessage(fr.input), "caller": fr.caller, "depth": fr.depth}
	for k, v := range spec.facts {
		facts[k] = v
	}
	txc := map[string]any{"src": fr.source, "stack": fr.stack, fr.source: facts}
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
// ran, as the budget's own code and message: the callee that spent the last
// of the fuel or the last hop ended on the exhaustion, and the caller learns
// it here before its own next charge refuses it too.
func budgetExhaustedInCall(ctx context.Context, stage string) (code, message string) {
	s := budgetFromCtx(ctx)
	if s == nil {
		return "", ""
	}
	switch e := s.overBudget(s.fuel.Load(), stage).(type) {
	case nil:
	case *FuelExhaustedError:
		return "txco_fuel_exhausted", e.Error()
	case *AllowanceExhaustedError:
		return "txco_allowance_exhausted", e.Error()
	default:
		return "txco_fuel_exhausted", e.Error()
	}
	if s.maxTTL > 0 && s.ttl.Load() <= 0 {
		return "txcl_scope_ttl_exhausted",
			fmt.Sprintf("the request's %d stage hops were used up inside the call", s.maxTTL)
	}
	return "", ""
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
