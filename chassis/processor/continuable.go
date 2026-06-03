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

	// Fire upstream in a goroutine with its OWN ctx so we can detach
	// independently from the request ctx. Buffered 1 so the goroutine
	// never blocks if we've moved on (promotion path drains later).
	workCtx, workCancel := context.WithTimeout(context.Background(), timeout)
	done := make(chan continuableResult, 1)
	aStart := time.Now()
	go func() {
		out, authorControlled, eerr := pu.Exec(workCtx, op)
		// Untrusted producer output is sanitized here so BOTH the sync-merge
		// path (below) and any async-promotion reuse see only allowed _txc.*.
		if eerr == nil && authorControlled && out.Type == event.JSON {
			out.Raw = sanitizeAuthorOutput(out.Raw)
		}
		done <- continuableResult{payload: out, err: eerr}
	}()

	timer := time.NewTimer(continueAfter)
	defer timer.Stop()

	select {
	case r := <-done:
		// SYNC PATH — completed before continue_after. Cancel the work
		// ctx (no-op since the call already returned, but releases the
		// timeout goroutine) and integrate the response.
		workCancel()
		return pu.completeContinuableSync(ctx, raw, stage, meta, op, name, ops, nextOps, r, aStart, resCh)
	case <-timer.C:
		// PROMOTION PATH — suspend durably, emit 202, detach goroutine.
		return pu.promoteContinuable(ctx, raw, stage, op, name, done, workCtx, workCancel, aStart, resCh)
	case <-ctx.Done():
		// Client disconnected before promotion would have fired. Kill the
		// upstream too — the response was speculative-sync, no continuation
		// exists yet, no one's listening.
		workCancel()
		return ctx.Err()
	}
}

// continuableResult is the inner channel payload — keeps the select
// readable.
type continuableResult struct {
	payload event.Payload
	err     error
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
		out, oerr := pu.OverlayResponse(op.Input, payload, op.Resonator.Emit.Overrides)
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
	aStart time.Time,
	resCh chan event.Payload,
) error {
	stack := op.Stack
	tenant, _ := ctx.Value(ctxKeyTenant).(string)
	cstage := stage

	// 1. Mint a run + rcid; freeze the opstack snapshot so a later
	//    `txco apply` can't change what this in-flight run resolves
	//    against (same protocol as suspendBarrierScope).
	var snapData []byte
	var snapHash string
	var snapN int
	if d, h, n, serr := pu.snapshotOpstack(ctx, tenant); serr != nil {
		pu.Logger.Warn("continuable: opstack snapshot failed; run will resume against live opstack",
			zap.String("tenant", tenant), zap.String("stack", stack), zap.Error(serr))
	} else if n > 0 {
		snapData, snapHash, snapN = d, h, n
	}
	originRID, _ := ctx.Value(config.CtxKeyRid).(string)
	runID, rcid, err := pu.Runs.CreateRun(ctx, tenant, stack, snapHash, cstage, originRID, time.Time{})
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
	go pu.finishContinuableDetached(workCtx, workCancel, done, runID, cstage, name, op)

	// 5. Emit the 202 (or 303 for browser Accept) to the client. From
	//    here the lifecycle is identical to mode=async: client polls
	//    /?_txc.continuation=<rcid>, gets the wait page if HTML, gets
	//    JSON status otherwise, eventually gets the resumed result.
	pu.emitContinuation202(ctx, raw, rcid, resCh)
	return nil
}

// finishContinuableDetached runs in the detached goroutine after a
// promotion: drains the in-flight EXEC result, records its terminal, and
// claims+Resume's the suspended stage. Spawns a resume trace with
// origin_rid linkage (admin-ui cross-navigation) — exact same shape as
// dispatchLocalAsync.
func (pu *Unit) finishContinuableDetached(
	workCtx context.Context,
	workCancel context.CancelFunc,
	done chan continuableResult,
	runID, stage, name string,
	op operation.Operation,
) {
	defer workCancel()

	var r continuableResult
	select {
	case r = <-done:
	case <-workCtx.Done():
		r = continuableResult{err: workCtx.Err()}
	}

	var tracer trace.RequestTracer
	var runTenant string // the run's tenant slug, for resume-trace attribution
	if pu.Sink != nil {
		tracer = pu.Sink.Begin(trace.RequestInfo{
			RID:       continuation.ResumeTraceRID(runID, stage),
			Src:       "continuation",
			Stack:     stage,
			StartedAt: time.Now(),
		})
		if rc, rcErr := pu.Runs.ReadRunCreated(workCtx, runID); rcErr == nil {
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
		workCtx = trace.WithContext(workCtx, tracer)
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
			out, oerr := pu.OverlayResponse(op.Input, payload, op.Resonator.Emit.Overrides)
			if oerr != nil {
				status = "failed"
				payload = string(failPayload(oerr.Error()))
			} else {
				payload = out
			}
		}
	}

	if _, terr := pu.Runs.RecordTerminal(workCtx, runID, stage, 0, name, status, []byte(payload)); terr != nil {
		pu.Logger.Error("continuable: RecordTerminal failed",
			zap.String("run", runID), zap.String("stage", stage),
			zap.String("op", name), zap.Error(terr))
		if tracer != nil {
			tracer.End("error", nil)
		}
		return
	}

	ss, sserr := pu.Runs.ReadStageSuspended(workCtx, runID, stage)
	if sserr != nil {
		if tracer != nil {
			tracer.End("error", nil)
		}
		return
	}
	state, _ := pu.Runs.StageState(workCtx, runID, stage, ss.Manifest)
	if state != continuation.StateResumable {
		// Solo-scope means this terminal SHOULD always make the stage
		// resumable; if it doesn't, the run is in an unexpected state.
		// Log so it's investigable but don't try to recover.
		pu.Logger.Warn("continuable: post-terminal stage not resumable",
			zap.String("run", runID), zap.String("stage", stage), zap.String("state", string(state)))
		if tracer != nil {
			tracer.End("ok", nil)
		}
		return
	}
	won, _ := pu.Runs.ClaimResume(workCtx, runID, stage)
	if !won {
		if tracer != nil {
			tracer.End("ok", nil)
		}
		return
	}
	rerr := pu.Resume(workCtx, runID, stage)
	if rerr != nil {
		pu.Logger.Error("continuable: Resume failed",
			zap.String("run", runID), zap.String("stage", stage), zap.Error(rerr))
	}
	if tracer != nil {
		rStatus := "ok"
		var final []byte
		if rerr != nil {
			rStatus = "error"
		} else if res, ok, _ := pu.Runs.ReadResult(workCtx, runID); ok {
			final = res
		}
		// Attribute the resume trace to the run's stored tenant slug (what
		// admin scoping filters on); fuel/bytes are best-effort from the
		// stored result envelope (may be empty on this path).
		trace.EmitUsage(tracer, FuelUsedFromEnvelope(string(final)), len(final), runTenant)
		tracer.End(rStatus, final)
	}
}
