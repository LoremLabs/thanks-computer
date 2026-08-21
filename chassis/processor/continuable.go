package processor

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// runScopeContinuable is the entry point for a solo-scope op authored with
// `WITH mode = "continuable"`. The chassis fires the upstream call inline
// and races its completion against a `continue_after` timer:
//
//   - upstream wins → merge the response and advance the scope normally,
//     no 202, no continuation token, no durable suspend (the speculative
//     "sync" branch costs nothing if it pays off).
//   - timer wins → mint a run + rcid, durably suspend the stage, emit
//     `202 Accepted` + continuation token to the client, and detach the
//     still-running upstream goroutine. When the upstream eventually
//     answers (or the timeout fires), the detached goroutine records the
//     terminal and drives Resume — same plumbing as a worker callback
//     would, just sourced from the chassis's own waiting goroutine.
//
// Solo-scope only in v1: a mixed scope (continuable + sync/async/other
// continuable in the same scope) needs lazy "promote all on first timer
// fire" semantics that aren't needed for the demo or any
// imminent use case. The caller (Run) rejects mixed scopes with a clear
// error. v2 can relax this when a real case appears.
func (pu *Unit) runScopeContinuable(
	ctx context.Context,
	raw, stage, meta string,
	ops []operation.Operation,
	nextOps []operation.Operation,
	resCh chan event.Payload,
) error {
	op := ops[0]
	name := opIdentity(op)
	in := op.Input
	if in == "" {
		in = "{}"
	}
	op.Input = in

	// The deferred-join peel only applies to mode="async" (deferred
	// dispatch); a join floor on a continuable op would be silently
	// ignored — reject loudly instead, before the timing checks (mode
	// compatibility is the more fundamental authoring error).
	if j, ok := opJoinAtScope(op); ok {
		return pu.failContinuableInline(ctx, resCh,
			fmt.Errorf("join_at_scope (= %d) requires mode = \"async\"; it is not honored with mode = \"continuable\"", j))
	}

	continueAfter := pu.opContinueAfter(op)
	timeout := pu.opContinuableTimeout(op)
	if continueAfter <= 0 {
		return pu.failContinuableInline(ctx, resCh,
			fmt.Errorf("continue_after must be > 0 (got %s)", continueAfter))
	}
	if continueAfter >= timeout {
		return pu.failContinuableInline(ctx, resCh,
			fmt.Errorf("continue_after (%s) must be < timeout (%s) — promotion would never fire",
				continueAfter, timeout))
	}

	// Materialize WITH `secrets.*` BEFORE spawning the op goroutine, against
	// the request ctx (tenant pin + request secret cache + request budget —
	// this is synchronous pre-promotion work, so its fuel charges the
	// request like any sync op's would). The bag's lifetime then belongs to
	// whoever sees the op actually finish: the sync-win branch, the detached
	// goroutine, or the client-disconnect drain below. Never a defer in THIS
	// frame — the promotion path returns while the op is still in flight,
	// and a mid-flight wipe corrupts per-retry credential reads.
	if secrets.HasRefs(op.Meta) {
		if merr := pu.materializeOpSecrets(ctx, &op); merr != nil {
			pu.Logger.Error("continuable: secret materialization failed",
				zap.String("stack", op.Stack), zap.Int("scope", op.Scope),
				zap.String("op_name", op.Name), zap.Error(merr))
			return pu.failContinuableInline(ctx, resCh, merr)
		}
	}

	// Fire upstream in a goroutine with its OWN ctx so we can detach
	// independently from the request ctx — detached from the request's
	// CANCELLATION, but still carrying the request VALUES the op
	// legitimately needs (tenant/source pins, rid, a live fuel budget).
	// Buffered 1 so the goroutine never blocks if we've moved on
	// (promotion path drains later).
	workCtx, fuelStart, workCancel := pu.detachedOpContext(ctx, raw, timeout)
	done := make(chan continuableResult, 1)
	aStart := time.Now()
	go func() {
		out, transport, eerr := pu.Exec(workCtx, op)
		// Untrusted producer output is sanitized here so BOTH the sync-merge
		// path (below) and any async-promotion reuse see only allowed _txc.*.
		// Trusted transports (ai://, txco://) pass through — their reserved
		// stamps (_txc.chat.*, _txc.computed.*) are the point.
		if eerr == nil && transportAuthorControlled(transport) && out.Type == event.JSON {
			out.Raw = sanitizeAuthorOutput(out.Raw)
		}
		done <- continuableResult{payload: out, transport: transport, err: eerr}
	}()

	timer := time.NewTimer(continueAfter)
	defer timer.Stop()

	select {
	case r := <-done:
		// SYNC PATH — completed before continue_after. Cancel the work
		// ctx (no-op since the call already returned, but releases the
		// timeout goroutine), wipe the bag (Exec has returned; nothing
		// reads secrets past this point), transfer the fuel the op drew
		// on its detached budget onto the request's meter, and integrate
		// the response.
		workCancel()
		op.Secrets.Zero()
		if drawn := fuelUsedFromCtx(workCtx) - fuelStart; drawn > 0 {
			// Err ignored: next Run-entry catches overshoot (same pattern
			// as Exec's charge site).
			_ = addFuel(ctx, drawn, stage)
		}
		return pu.completeContinuableSync(ctx, raw, stage, meta, op, name, ops, nextOps, r, aStart, resCh)
	case <-timer.C:
		// PROMOTION PATH — suspend durably, emit 202, detach goroutine.
		return pu.promoteContinuable(ctx, raw, stage, op, name, done, workCtx, workCancel, fuelStart, aStart, resCh)
	case <-ctx.Done():
		// Client disconnected before promotion would have fired. Kill the
		// upstream too — the response was speculative-sync, no continuation
		// exists yet, no one's listening. The bag is wiped only after the
		// op goroutine actually returns (a cancel is a signal, not a join).
		workCancel()
		go func() {
			<-done
			op.Secrets.Zero()
		}()
		return ctx.Err()
	}
}

// detachedOpContext builds the context a continuable op's goroutine runs
// under: a fresh Background-rooted ctx (so the request ending cannot cancel
// the detached work) carrying explicit copies of the request values the op
// legitimately needs —
//
//   - the tenant pin (per-tenant secret resolution in ai:// and friends),
//   - the source pin (privileged ops gate on the originating inlet),
//   - the rid (op logging attribution),
//   - a FRESH fuel/TTL budget hydrated from the envelope, so detached work
//     meters exactly like sync work (returned fuelStart is the hydrated
//     baseline; drawn fuel = fuelUsedFromCtx(workCtx) − fuelStart).
//
// The request tracer is deliberately NOT carried: the promotion records its
// own pending step on the origin trace, and the detached completion lands
// on the resume trace (finishContinuableDetached attaches it). Copying
// values explicitly — rather than context.WithoutCancel — keeps the origin
// trace and the request-scoped secret cache from leaking past the request.
func (pu *Unit) detachedOpContext(ctx context.Context, raw string, timeout time.Duration) (context.Context, int64, context.CancelFunc) {
	dctx := context.Background()
	if t := tenantScope(ctx); t != "" {
		dctx = WithTenant(dctx, t)
	}
	if s := sourceScope(ctx); s != "" {
		dctx = WithSource(dctx, s)
	}
	if rid, ok := ctx.Value(config.CtxKeyRid).(string); ok && rid != "" {
		dctx = context.WithValue(dctx, config.CtxKeyRid, rid)
	}
	dctx, fuelStart, _ := loadBudget(dctx, raw, pu.Conf)
	dctx, cancel := context.WithTimeout(dctx, timeout)
	return dctx, fuelStart, cancel
}

// continuableResult is the inner channel payload — keeps the select
// readable. transport is what Exec's dispatch switch actually took; it is
// persisted on the terminal so the resume merge can key its trust decision
// on it.
type continuableResult struct {
	payload   event.Payload
	transport string
	err       error
}

// failContinuableInline emits an error payload to the client without
// promoting. Used for the bad-config branches (continue_after <= 0 or
// >= timeout) that should never have made it past validate, but we
// surface them clearly if they do.
func (pu *Unit) failContinuableInline(ctx context.Context, resCh chan event.Payload, err error) error {
	select {
	case resCh <- event.Payload{Raw: string(failPayload(err.Error())), Type: event.ErrorStr}:
	case <-ctx.Done():
	}
	return err
}

// completeContinuableSync handles the "upstream beat the timer" branch:
// merge the response into the running envelope and call advanceAfterScope
// to drive the rest of the pipeline. Trace step shape mirrors a regular
// sync op (transport: "continuable", status: "ok"), so admin-ui shows it
// the way the author thinks of it.
func (pu *Unit) completeContinuableSync(
	ctx context.Context,
	raw, stage, meta string,
	op operation.Operation,
	name string,
	ops, nextOps []operation.Operation,
	r continuableResult,
	aStart time.Time,
	resCh chan event.Payload,
) error {
	finish := time.Now()
	if r.err != nil {
		trace.FromContext(ctx).Step(trace.StepInfo{
			Stack: op.Stack, Scope: op.Scope, Name: name,
			Operation: op.Resonator.Exec, Transport: "continuable",
			Input:     []byte(op.Input),
			StartedAt: aStart, FinishedAt: finish,
			Status: "error", Error: r.err.Error(),
		})
		select {
		case resCh <- event.Payload{Raw: string(failPayload(r.err.Error())), Type: event.ErrorStr}:
		case <-ctx.Done():
		}
		return r.err
	}
	payload := r.payload.Raw
	if r.payload.Type == event.Null || payload == "" {
		payload = "{}"
	}
	if op.Resonator != nil && op.Resonator.Emit != nil {
		out, oerr := pu.OverlayResponseFor(ctx, op.EnvelopeView(), payload, op.Resonator.Emit.Overrides)
		if oerr != nil {
			trace.FromContext(ctx).Step(trace.StepInfo{
				Stack: op.Stack, Scope: op.Scope, Name: name,
				Operation: op.Resonator.Exec, Transport: "continuable",
				Input:     []byte(op.Input),
				StartedAt: aStart, FinishedAt: finish,
				Status: "error", Error: oerr.Error(),
			})
			select {
			case resCh <- event.Payload{Raw: string(failPayload(oerr.Error())), Type: event.ErrorStr}:
			case <-ctx.Done():
			}
			return oerr
		}
		payload = out
	}
	trace.FromContext(ctx).Step(trace.StepInfo{
		Stack: op.Stack, Scope: op.Scope, Name: name,
		Operation: op.Resonator.Exec, Transport: "continuable",
		Input:     []byte(op.Input),
		Output:    []byte(payload),
		StartedAt: aStart, FinishedAt: finish,
		Status: "ok",
	})

	resp, merr := pu.MergeJSON(raw, payload)
	if merr != nil {
		select {
		case resCh <- event.Payload{Raw: string(failPayload(merr.Error())), Type: event.ErrorStr}:
		case <-ctx.Done():
		}
		return merr
	}
	opsDone := false
	stop, derr := pu.advanceAfterScope(ctx, stage, resp, ops, meta, nextOps, &opsDone, resCh, func() {})
	if !stop && derr == nil {
		return nil
	}
	return derr
}

// promoteContinuable handles the "timer beat upstream" branch: mint a
// continuation, durably suspend the stage so the polling URL can resolve
// it, emit 202 to the client, and detach the still-running goroutine.
// When the upstream eventually returns (or the work ctx times out), the
// detached goroutine records the terminal + ClaimResume + Resume — same
// path as dispatchLocalAsync's post-completion block, deliberately so.
func (pu *Unit) promoteContinuable(
	ctx context.Context,
	raw, stage string,
	op operation.Operation,
	name string,
	done chan continuableResult,
	workCtx context.Context,
	workCancel context.CancelFunc,
	fuelStart int64,
	aStart time.Time,
	resCh chan event.Payload,
) error {
	stack := op.Stack
	tenant, _ := ctx.Value(ctxKeyTenant).(string)
	cstage := stage

	// 1. Resolve the run identity. A continuable promoting inside a RESUMED
	//    pipeline (or after a deferred dispatch on this request) reuses the
	//    existing run — same protocol as suspendBarrierScope — rather than
	//    minting a second run; only a fresh request creates one and freezes
	//    the opstack snapshot (so a later `txco apply` can't change what
	//    this in-flight run resolves against).
	var runID, rcid string
	resuming := false
	// Snapshot hash recorded on the run/stage docs (debug/trace only —
	// resume loads the snapshot doc by runID, not by hash). Set on first
	// suspend; "" when reusing an existing run's identity.
	snapHash := ""
	if ri, ok := resumeRunFrom(ctx); ok {
		runID, rcid, resuming = ri.runID, ri.rcid, true
	} else if di, ok := deferredRunFrom(ctx); ok {
		// Run + snapshot already exist from the deferred dispatch. NOT
		// resuming: the client is still attached and this is its first 202.
		runID, rcid = di.runID, di.rcid
	} else {
		var snapData []byte
		var snapN int
		if d, h, n, serr := pu.snapshotOpstack(ctx, tenant); serr != nil {
			pu.Logger.Warn("continuable: opstack snapshot failed; run will resume against live opstack",
				zap.String("tenant", tenant), zap.String("stack", stack), zap.Error(serr))
		} else if n > 0 {
			snapData, snapHash, snapN = d, h, n
		}
		originRID, _ := ctx.Value(config.CtxKeyRid).(string)
		var err error
		runID, rcid, err = pu.Runs.CreateRun(ctx, tenant, stack, snapHash, cstage, originRID, time.Time{})
		if err != nil {
			workCancel()
			return err
		}
		if snapN > 0 {
			if werr := pu.Runs.WriteOpstackSnapshot(ctx, runID, snapData); werr != nil {
				pu.Logger.Warn("continuable: opstack snapshot write failed",
					zap.String("run", runID), zap.Error(werr))
			}
		}
		_ = pu.Runs.AppendEvent(ctx, runID, "run.created", map[string]any{
			"stack": stack, "stage": cstage, "tenant": tenant, "promoted_from": "continuable",
		})
	}

	// 2. Suspend the stage with a one-op manifest (solo scope; ordinal 0).
	//    `Async: true` so StageState treats it like an async op pending a
	//    terminal — the detached goroutine will record one shortly.
	manifest := []continuation.OpManifestEntry{{Ordinal: 0, Op: name, Async: true}}
	in := op.Input
	if in == "" {
		in = "{}"
	}
	specs := []continuation.OpRecordSpec{{
		Ordinal: 0, Op: name, Async: true, Input: []byte(in),
	}}
	if err := pu.Runs.SuspendStage(ctx, runID, cstage, raw, snapHash, manifest); err != nil {
		workCancel()
		return err
	}
	if err := pu.Runs.CreateOpRecords(ctx, runID, cstage, specs); err != nil {
		workCancel()
		return err
	}
	_ = pu.Runs.AppendEvent(ctx, runID, "stage.suspended", map[string]any{
		"stage": cstage, "ops": 1, "promoted": true,
	})

	// 3. Trace the promotion on the suspending request's trace so admin-ui
	//    can navigate origin → resume. Pending step shape matches
	//    dispatchLocalAsync's so the timeline reads consistently.
	trace.FromContext(ctx).Event(trace.TimelineEvent{
		Ts:    time.Now(),
		Event: "stage.promote-to-continuation",
		Fields: map[string]any{
			"run_id":              runID,
			"run_continuation_id": rcid,
			"stage":               cstage,
		},
	})
	ack, _ := json.Marshal(map[string]string{"status": "promoted", "transport": "continuable"})
	trace.FromContext(ctx).Step(trace.StepInfo{
		Stack: op.Stack, Scope: op.Scope, Name: name,
		Operation: op.Resonator.Exec, Transport: "continuable",
		Input:     []byte(op.Input),
		Output:    ack,
		StartedAt: aStart, FinishedAt: time.Now(),
		Status: "pending",
	})

	// 4. Detach the upstream goroutine. When it returns (or workCtx
	//    times out), record the terminal and drive Resume — symmetric
	//    with dispatchLocalAsync's tail.
	go pu.finishContinuableDetached(workCtx, workCancel, done, runID, cstage, name, op, fuelStart)

	// 5. Emit the 202 (or 303 for browser Accept) to the client. From
	//    here the lifecycle is identical to mode=async: client polls
	//    /?_txc.continuation=<rcid>, gets the wait page if HTML, gets
	//    JSON status otherwise, eventually gets the resumed result.
	//    Under resume there is no client waiting (it got its 202 on the
	//    original request); emitting here would be misread by Resume's
	//    capture channel as a final result — mirror suspendBarrierScope's
	//    silence, but bill the resume segment this suspend just ended.
	if resuming {
		pu.emitResumeSegmentUsage(ctx, raw, runID)
	} else {
		pu.emitContinuation202(ctx, raw, rcid, resCh)
	}
	return nil
}

// finishContinuableDetached runs in the detached goroutine after a
// promotion: drains the in-flight EXEC result, wipes the op's secret bag
// (this goroutine owns its lifetime — the suspending request frame returned
// long ago), records its terminal with the producing transport + detached
// fuel, and claims+Resume's the suspended stage. Spawns a resume trace with
// origin_rid linkage (admin-ui cross-navigation) — exact same shape as
// dispatchLocalAsync.
func (pu *Unit) finishContinuableDetached(
	workCtx context.Context,
	workCancel context.CancelFunc,
	done chan continuableResult,
	runID, stage, name string,
	op operation.Operation,
	fuelStart int64,
) {
	defer workCancel()

	var r continuableResult
	select {
	case r = <-done:
		// Exec has returned — safe to wipe the bag now.
		op.Secrets.Zero()
	case <-workCtx.Done():
		r = continuableResult{err: workCtx.Err()}
		// The op goroutine may still be unwinding from the cancel; wipe
		// only after it actually returns (a cancel is a signal, not a join).
		go func() {
			<-done
			op.Secrets.Zero()
		}()
	}

	// Fuel the op drew on its detached budget — persisted on the terminal
	// so Resume folds it into the envelope meter (billing parity with sync).
	drawnFuel := fuelUsedFromCtx(workCtx) - fuelStart
	if drawnFuel < 0 {
		drawnFuel = 0
	}

	// The record/resume sequence runs on its OWN context: workCtx bounds the
	// upstream op and may already be exhausted (the timeout branch above) or
	// nearly so — a slow upstream must not starve the resume pipeline, which
	// runs every remaining scope of the run. Background like the worker-
	// callback and deferred resume paths; the resumed ops' own timeouts
	// bound the work.
	finCtx := context.Background()

	var tracer trace.RequestTracer
	var runTenant string // the run's tenant slug, for resume-trace attribution
	if pu.Sink != nil {
		tracer = pu.Sink.Begin(trace.RequestInfo{
			RID:       continuation.ResumeTraceRID(runID, stage),
			Src:       "continuation",
			Stack:     stage,
			StartedAt: time.Now(),
		})
		if rc, rcErr := pu.Runs.ReadRunCreated(finCtx, runID); rcErr == nil {
			runTenant = rc.TenantID
			tracer.Event(trace.TimelineEvent{
				Ts:    time.Now(),
				Event: "continuation.resume",
				Fields: map[string]any{
					"run_id":              runID,
					"run_continuation_id": rc.RunContinuationID,
					"origin_rid":          rc.OriginRID,
					"stage":               stage,
					"stack_version_id":    rc.StackVersionID,
				},
			})
		}
		finCtx = trace.WithContext(finCtx, tracer)
	}

	status := "completed"
	var payload string
	if r.err != nil {
		status = "failed"
		payload = string(failPayload(r.err.Error()))
	} else {
		payload = r.payload.Raw
		if r.payload.Type == event.Null || payload == "" {
			payload = "{}"
		}
		if op.Resonator != nil && op.Resonator.Emit != nil {
			// workCtx, not finCtx: the detached work ctx carries the tenant
			// pin OverlayResponseFor keys system provenance on.
			out, oerr := pu.OverlayResponseFor(workCtx, op.EnvelopeView(), payload, op.Resonator.Emit.Overrides)
			if oerr != nil {
				status = "failed"
				payload = string(failPayload(oerr.Error()))
			} else {
				payload = out
			}
		}
	}

	if _, terr := pu.Runs.RecordTerminal(finCtx, runID, stage, 0, name, status,
		continuation.TerminalMeta{Transport: r.transport, FuelUsed: drawnFuel}, []byte(payload)); terr != nil {
		pu.Logger.Error("continuable: RecordTerminal failed",
			zap.String("run", runID), zap.String("stage", stage),
			zap.String("op", name), zap.Error(terr))
		if tracer != nil {
			tracer.End("error", "continuable: RecordTerminal failed: "+terr.Error(), nil)
		}
		return
	}

	ss, sserr := pu.Runs.ReadStageSuspended(finCtx, runID, stage)
	if sserr != nil {
		if tracer != nil {
			tracer.End("error", "continuable: ReadStageSuspended failed: "+sserr.Error(), nil)
		}
		return
	}
	state, _ := pu.Runs.StageState(finCtx, runID, stage, ss.Manifest)
	if state != continuation.StateResumable {
		// Solo-scope means this terminal SHOULD always make the stage
		// resumable; if it doesn't, the run is in an unexpected state.
		// Log so it's investigable but don't try to recover.
		pu.Logger.Warn("continuable: post-terminal stage not resumable",
			zap.String("run", runID), zap.String("stage", stage), zap.String("state", string(state)))
		if tracer != nil {
			tracer.End("ok", "", nil)
		}
		return
	}
	won, _ := pu.Runs.ClaimResume(finCtx, runID, stage)
	if !won {
		if tracer != nil {
			tracer.End("ok", "", nil)
		}
		return
	}
	rerr := pu.Resume(finCtx, runID, stage)
	if rerr != nil {
		pu.Logger.Error("continuable: Resume failed",
			zap.String("run", runID), zap.String("stage", stage), zap.Error(rerr))
	}
	if tracer != nil {
		rStatus := "ok"
		rReason := ""
		var final []byte
		if rerr != nil {
			rStatus = "error"
			rReason = "continuable: Resume failed: " + rerr.Error()
		} else if res, ok, _ := pu.Runs.ReadResult(finCtx, runID); ok {
			final = res
		}
		// Attribute the resume trace to the run's stored tenant slug (what
		// admin scoping filters on); fuel/bytes are best-effort from the
		// stored result envelope (may be empty on this path).
		trace.EmitUsage(tracer, FuelUsedFromEnvelope(string(final)), len(final), runTenant)
		tracer.End(rStatus, rReason, final)
	}
}
