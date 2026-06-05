package processor

// Deferred-join processor logic (internal docs/todo-deferred-join.md): an async op
// dispatched at one scope that joins at a later same-stack scope. This file
// holds the run-side pieces (deadline derivation now; dispatch + per-boundary
// join check land with P4). The durable store records live in
// chassis/continuation/deferred.go.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// deadlineHorizon computes the reap deadline (expires_at) for a deferred
// join:
//
//		expires_at = now + async_budget + fixed_slack
//
//	  - async_budget = the op's runtime budget (opAsyncBudget): its WITH
//	    timeout, or async-runtime-default. The long pole.
//	  - fixed_slack  = the configured deferred-join-slack pad covering the
//	    downstream synchronous scopes the run still traverses.
//
// v1 uses a flat slack — cheap, deterministic, and fail-closed (an
// over-estimate only delays orphan reaping). Accurately summing each
// remaining scope's max op timeout would require resolving the downstream
// opstack at dispatch (envelope-dependent, double work); it's a documented
// future refinement, not needed for a safe reap horizon.
func (pu *Unit) deadlineHorizon(now time.Time, op operation.Operation) time.Time {
	slack, _ := time.ParseDuration(pu.Conf.DeferredJoinSlack) // validated at startup
	return now.Add(pu.opAsyncBudget(op) + slack)
}

// stageScope is the resolved (stack, scope) the run is actually at. Prefer
// the ops' own scope: OpsForStage returns the MIN scope ≥ the requested
// stage (floor lookup), so a sparse entry like "web/0" lands the run at the
// first real scope (e.g. 100). Falls back to parsing the stage string when
// the scope has no ops.
func (pu *Unit) stageScope(stage string, ops []operation.Operation) (string, int) {
	if len(ops) > 0 {
		return ops[0].Stack, ops[0].Scope
	}
	st, sc, err := pu.StageParse(stage)
	if err != nil {
		return stage, 0
	}
	return st, sc
}

// isDeferredJoinManifest reports whether a suspended stage's manifest is a
// deferred-join suspend (vs a same-scope barrier). Deferred entries carry an
// OpContinuationID (their terminal lives at the opc-keyed location, and the
// join scope's own ops have not run yet — see Resume's divergence).
func isDeferredJoinManifest(m []continuation.OpManifestEntry) bool {
	for _, e := range m {
		if e.OpContinuationID != "" {
			return true
		}
	}
	return false
}

// dispatchDeferred peels async ops whose join floor is a LATER scope out of
// the scope's op set, dispatches them WITHOUT suspending the run, and returns
// the remaining ops (which fall through to the normal sync / same-scope
// barrier path). It threads the run identity onto ctx so every later scope
// boundary runs the floor check (resolveDeferredJoins) and a later same-scope
// barrier reuses this run. No deferred op ⇒ ctx and ops pass through
// untouched (zero overhead for ordinary runs).
func (pu *Unit) dispatchDeferred(ctx context.Context, raw, stage string, ops []operation.Operation) (context.Context, []operation.Operation, error) {
	curStack, curScope := pu.stageScope(stage, ops)

	var deferred []operation.Operation
	remaining := make([]operation.Operation, 0, len(ops))
	for _, op := range ops {
		if j, ok := opJoinAtScope(op); ok && isAsyncOp(op) && j > curScope {
			deferred = append(deferred, op)
		} else {
			remaining = append(remaining, op)
		}
	}
	if len(deferred) == 0 {
		return ctx, ops, nil
	}

	cstage := curStack + "/" + strconv.Itoa(curScope)
	tenant, _ := ctx.Value(ctxKeyTenant).(string)

	// Deterministic ordinal among the deferred ops (identity sort), matching
	// suspendBarrierScope's ordering so a join scope with several deferred ops
	// merges in a stable order.
	sort.SliceStable(deferred, func(a, b int) bool {
		return opIdentity(deferred[a]) < opIdentity(deferred[b])
	})

	// Run identity: reuse an existing one (a resume, or an earlier deferred
	// dispatch on this same request) else create up front — the worker
	// callback needs a durable target. Freeze the opstack at creation so
	// resume is deterministic (mirrors suspendBarrierScope).
	var runID, rcid string
	if di, ok := deferredRunFrom(ctx); ok {
		runID, rcid = di.runID, di.rcid
	} else if ri, ok := resumeRunFrom(ctx); ok {
		runID, rcid = ri.runID, ri.rcid
	}
	if runID == "" {
		now := time.Now().UTC()
		var expiresAt time.Time // run horizon = latest possible completion
		for _, op := range deferred {
			if h := pu.deadlineHorizon(now, op); h.After(expiresAt) {
				expiresAt = h
			}
		}
		var snapData []byte
		var snapHash string
		var snapN int
		if d, h, n, serr := pu.snapshotOpstack(ctx, tenant); serr != nil {
			pu.Logger.Warn("opstack snapshot failed; deferred run will resume against live opstack",
				zap.String("tenant", tenant), zap.Error(serr))
		} else if n > 0 {
			snapData, snapHash, snapN = d, h, n
		}
		originRID, _ := ctx.Value(config.CtxKeyRid).(string)
		var cerr error
		runID, rcid, cerr = pu.Runs.CreateRun(ctx, tenant, curStack, snapHash, cstage, originRID, expiresAt)
		if cerr != nil {
			return ctx, ops, cerr
		}
		if snapN > 0 {
			if werr := pu.Runs.WriteOpstackSnapshot(ctx, runID, snapData); werr != nil {
				pu.Logger.Warn("deferred opstack snapshot write failed",
					zap.String("run", runID), zap.Error(werr))
			}
		}
		_ = pu.Runs.AppendEvent(ctx, runID, "run.created", map[string]any{
			"stack": curStack, "stage": cstage, "tenant": tenant, "deferred": true,
		})
	}
	ctx = withDeferredRun(ctx, runID, rcid)

	// Records first (so an instant callback resolves against an existing
	// doc), then dispatch. Remote-async ops POST within the low ack timeout
	// in parallel; local-async ops detach.
	var wg sync.WaitGroup
	for ordinal, op := range deferred {
		name := opIdentity(op)
		opc, oerr := continuation.NewOpContinuationID()
		if oerr != nil {
			return ctx, ops, oerr
		}
		token, hash, terr := continuation.MintToken()
		if terr != nil {
			return ctx, ops, terr
		}
		join, _ := opJoinAtScope(op)
		in := op.Input
		if in == "" {
			in = "{}"
		}
		expiresAt := pu.deadlineHorizon(time.Now().UTC(), op)
		sp := continuation.DeferredOpSpec{
			OpContinuationID: opc, Op: name, Ordinal: ordinal,
			JoinAtScope: join, DispatchStage: cstage, DispatchStack: curStack,
			TokenHash: hash, Input: []byte(in), ExpiresAt: expiresAt,
		}
		if cerr := pu.Runs.CreateDeferredOp(ctx, runID, sp); cerr != nil {
			return ctx, ops, cerr
		}

		if isLocalAsyncOp(op) {
			pu.dispatchLocalAsyncDeferred(ctx, op, runID, opc, name)
			continue
		}

		wg.Add(1)
		go func(op operation.Operation, opc, token, name string, expiresAt time.Time) {
			defer wg.Done()
			actx, cancel := context.WithTimeout(ctx, pu.opAckTimeout())
			defer cancel()
			env := AsyncEnvelope{
				OpContinuationID:  opc,
				CallbackURL:       pu.callbackURLFor(opc),
				RunID:             runID,
				RunContinuationID: rcid,
				Stack:             curStack,
				Stage:             cstage,
				Op:                name,
				ExpiresAt:         expiresAt.Format(time.RFC3339),
			}
			aStart := time.Now()
			jobID, aerr := pu.ExecHTTPAsync(actx, op, env, token)
			if aerr != nil {
				pu.Logger.Warn("deferred async dispatch failed",
					zap.String("stage", cstage), zap.String("op", name), zap.Error(aerr))
				trace.FromContext(ctx).Step(trace.StepInfo{
					Stack: op.Stack, Scope: op.Scope, Name: name,
					Operation: op.Resonator.Exec, Transport: "async",
					Input:     []byte(op.Input),
					StartedAt: aStart, FinishedAt: time.Now(),
					Status: "error", Error: aerr.Error(),
				})
				_, _ = pu.Runs.RecordDeferredTerminal(ctx, runID, opc, "failed", failPayload(aerr.Error()))
				return
			}
			ack, _ := json.Marshal(map[string]string{"status": "accepted", "job_id": jobID})
			trace.FromContext(ctx).Step(trace.StepInfo{
				Stack: op.Stack, Scope: op.Scope, Name: name,
				Operation: op.Resonator.Exec, Transport: "async",
				Input: []byte(op.Input), Output: ack,
				StartedAt: aStart, FinishedAt: time.Now(), Status: "pending",
			})
		}(op, opc, token, name, expiresAt)
	}
	wg.Wait()

	return ctx, remaining, nil
}

// resolveDeferredJoins is the per-boundary floor check. For each outstanding
// deferred join whose floor we've reached in the dispatch stack (or any join,
// when we're crossing into a different stack), it merges a ready terminal or
// suspends to wait. Returns the (possibly merged) envelope and stop=true when
// the run should return without running this scope — either a 202 was emitted
// (suspended) or a deferred op failed (error surfaced to the client).
func (pu *Unit) resolveDeferredJoins(ctx context.Context, di deferredIdent, curStack string, curScope int, raw string, resCh chan event.Payload) (string, bool, error) {
	pjs, err := pu.Runs.ListPendingJoins(ctx, di.runID)
	if err != nil {
		return raw, false, err
	}
	if len(pjs) == 0 {
		return raw, false, nil
	}

	// Select joins to resolve here: floor reached in the same stack, OR a
	// cross-stack transition (single-stack constraint — force-resolve every
	// pending join before control leaves the dispatch stack).
	var toResolve []continuation.PendingJoin
	for _, pj := range pjs {
		sameStack := pj.DispatchStack == curStack
		reached := sameStack && curScope >= pj.JoinAtScope
		crossing := !sameStack
		if !(reached || crossing) {
			continue
		}
		joined, jerr := pu.Runs.IsJoined(ctx, di.runID, pj.OpContinuationID)
		if jerr != nil {
			return raw, false, jerr
		}
		if joined {
			continue // fire-once: already merged (e.g. by a racing resume)
		}
		toResolve = append(toResolve, pj)
	}
	if len(toResolve) == 0 {
		return raw, false, nil
	}
	sort.SliceStable(toResolve, func(i, j int) bool {
		return toResolve[i].Ordinal < toResolve[j].Ordinal
	})

	merged := raw
	var waiting []continuation.PendingJoin
	for _, pj := range toResolve {
		term, terr := pu.Runs.ReadDeferredTerminal(ctx, di.runID, pj.OpContinuationID)
		if terr == continuation.ErrNotFound {
			waiting = append(waiting, pj)
			continue
		}
		if terr != nil {
			return raw, false, terr
		}
		if term.Status == "failed" {
			// A deferred op failed. Surface it and stop the request. The run
			// record lingers and is reaped on expiry (v1: no in-request run
			// teardown). Mark joined so a later boundary won't retry it.
			_, _ = pu.Runs.MarkJoined(ctx, di.runID, pj.OpContinuationID)
			eb := []byte(nil)
			if term.ErrorKey != "" {
				eb, _ = pu.Runs.Get(ctx, term.ErrorKey)
			}
			if len(eb) == 0 {
				eb = failPayload("deferred op failed: " + pj.Op)
			}
			select {
			case resCh <- event.Payload{Raw: string(eb), Type: event.ErrorStr}:
			case <-ctx.Done():
			}
			return raw, true, nil
		}
		// Ready → merge exactly once.
		won, jerr := pu.Runs.MarkJoined(ctx, di.runID, pj.OpContinuationID)
		if jerr != nil {
			return raw, false, jerr
		}
		if !won {
			continue // a racing resume already merged it
		}
		var ob []byte
		if term.OutputKey != "" {
			ob, _ = pu.Runs.Get(ctx, term.OutputKey)
		}
		if s := string(ob); s != "" && s != "{}" {
			// Deferred op output is untrusted: strip reserved _txc.* before merge.
			s = sanitizeAuthorOutput(s)
			if m, merr := pu.MergeJSON(merged, s); merr == nil {
				merged = m
			} else {
				pu.Logger.Warn("deferred join merge error",
					zap.String("run", di.runID), zap.String("op", pj.Op), zap.Error(merr))
			}
		}
	}

	if len(waiting) == 0 {
		return merged, false, nil // all merged; run this scope normally
	}

	joinStage := curStack + "/" + strconv.Itoa(curScope)
	if serr := pu.suspendForDeferredJoins(ctx, di, joinStage, merged, waiting, resCh); serr != nil {
		return raw, false, serr
	}
	return merged, true, nil
}

// suspendForDeferredJoins records the join-scope suspend (manifest of the
// not-yet-complete deferred ops, partial-merged envelope), notes the dynamic
// join stage per opc so the worker callback can find it, emits the client 202
// (silent under resume — no client is attached), then race-guards against a
// terminal that landed during the suspend write.
func (pu *Unit) suspendForDeferredJoins(ctx context.Context, di deferredIdent, joinStage, merged string, waiting []continuation.PendingJoin, resCh chan event.Payload) error {
	manifest := make([]continuation.OpManifestEntry, 0, len(waiting))
	for _, pj := range waiting {
		manifest = append(manifest, continuation.OpManifestEntry{
			Ordinal: pj.Ordinal, Op: pj.Op, Async: true,
			OpContinuationID: pj.OpContinuationID,
		})
	}
	sort.SliceStable(manifest, func(i, j int) bool {
		return manifest[i].Ordinal < manifest[j].Ordinal
	})

	// StackVersion "" — the opstack snapshot was frozen at run creation
	// (dispatch); resume loads it by runID, not from this doc.
	if err := pu.Runs.SuspendStage(ctx, di.runID, joinStage, merged, "", manifest); err != nil {
		return err
	}
	for _, pj := range waiting {
		if err := pu.Runs.SetDeferredSuspendedAt(ctx, di.runID, pj.OpContinuationID, joinStage); err != nil {
			return err
		}
	}
	_ = pu.Runs.AppendEvent(ctx, di.runID, "stage.suspended.deferred", map[string]any{
		"stage": joinStage, "ops": len(manifest),
	})
	trace.FromContext(ctx).Event(trace.TimelineEvent{
		Ts: time.Now(), Event: "continuation.suspend",
		Fields: map[string]any{
			"run_id": di.runID, "run_continuation_id": di.rcid, "stage": joinStage,
		},
	})

	// Under resume there is no client waiting (it got its 202 at the original
	// join suspend); emitting here would be misread by the resume capture
	// channel as a final result. Mirror suspendBarrierScope's silence.
	if _, resuming := resumeRunFrom(ctx); !resuming {
		pu.emitContinuation202(ctx, merged, di.rcid, resCh)
	}

	// Race guard: a worker callback may have recorded a terminal between the
	// caller's ReadDeferredTerminal and SuspendStage above. If the stage is
	// already resumable, drive resume in the background — the request ctx dies
	// once the 202 is consumed. ClaimResume dedupes against the callback's own
	// claim, so exactly one resume runs.
	if state, serr := pu.Runs.StageState(ctx, di.runID, joinStage, manifest); serr == nil && state == continuation.StateResumable {
		runID, stage := di.runID, joinStage
		go pu.DriveDeferredResume(runID, stage)
	}
	return nil
}

// DriveDeferredResume claims and resumes a deferred-join suspend on a fresh
// background context (the request ctx is gone). Self-contained and safe for
// both the suspend-time race guard and the worker-completion path: it
// re-checks resumability and lets ClaimResume pick the single winner.
func (pu *Unit) DriveDeferredResume(runID, stage string) {
	bg := context.Background()
	ss, err := pu.Runs.ReadStageSuspended(bg, runID, stage)
	if err != nil {
		return
	}
	state, serr := pu.Runs.StageState(bg, runID, stage, ss.Manifest)
	if serr != nil || state != continuation.StateResumable {
		return // sibling deferred ops still pending
	}
	won, cerr := pu.Runs.ClaimResume(bg, runID, stage)
	if cerr != nil || !won {
		return
	}

	var tracer trace.RequestTracer
	var runTenant string // the run's tenant slug, for resume-trace attribution
	if pu.Sink != nil {
		tracer = pu.Sink.Begin(trace.RequestInfo{
			RID: continuation.ResumeTraceRID(runID, stage), Src: "continuation",
			Stack: stage, StartedAt: time.Now(),
		})
		if rc, rcErr := pu.Runs.ReadRunCreated(bg, runID); rcErr == nil {
			runTenant = rc.TenantID
			tracer.Event(trace.TimelineEvent{
				Ts: time.Now(), Event: "continuation.resume",
				Fields: map[string]any{
					"run_id": runID, "run_continuation_id": rc.RunContinuationID,
					"origin_rid": rc.OriginRID, "stage": stage,
					"stack_version_id": rc.StackVersionID,
				},
			})
		}
		bg = trace.WithContext(bg, tracer)
	}
	rerr := pu.Resume(bg, runID, stage)
	if tracer != nil {
		rStatus := "ok"
		rReason := ""
		var final []byte
		if rerr != nil {
			rStatus = "error"
			rReason = "deferred resume failed: " + rerr.Error()
		} else if res, ok, _ := pu.Runs.ReadResult(bg, runID); ok {
			final = res
		}
		// Attribute the resume trace to the run's stored tenant slug (what
		// admin scoping filters on); fuel/bytes are best-effort from the
		// stored result envelope.
		trace.EmitUsage(tracer, FuelUsedFromEnvelope(string(final)), len(final), runTenant)
		tracer.End(rStatus, rReason, final)
	}
	if rerr != nil {
		pu.Logger.Error("deferred resume failed",
			zap.String("run", runID), zap.String("stage", stage), zap.Error(rerr))
	}
}

// dispatchLocalAsyncDeferred runs a local-async (mcp+http) deferred op
// fire-and-forget: records a 'pending' step on the dispatching trace, then a
// detached goroutine runs the op, records the deferred terminal, and — if the
// run has by then suspended at the join waiting for this op — drives resume.
// Mirrors dispatchLocalAsync but writes the terminal at the opc-keyed
// deferred location and resolves the join stage dynamically.
func (pu *Unit) dispatchLocalAsyncDeferred(reqCtx context.Context, op operation.Operation, runID, opc, name string) {
	timeout, over := pu.opMetaTimeout(op)
	if over {
		_, _ = pu.Runs.RecordDeferredTerminal(reqCtx, runID, opc, "failed",
			failPayload("op timeout exceeds op-timeout-max"))
		return
	}

	aStart := time.Now()
	ack, _ := json.Marshal(map[string]string{"status": "accepted", "transport": "async-local"})
	trace.FromContext(reqCtx).Step(trace.StepInfo{
		Stack: op.Stack, Scope: op.Scope, Name: name,
		Operation: op.Resonator.Exec, Transport: "async",
		Input: []byte(op.Input), Output: ack,
		StartedAt: aStart, FinishedAt: time.Now(), Status: "pending",
	})

	go func() {
		workCtx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Trust bit discarded: stored then merged at the sanitizing
		// resume/deferred sites (async/worker output is always untrusted).
		out, _, eerr := pu.Exec(workCtx, op)
		status := "completed"
		var payload string
		if eerr != nil {
			status = "failed"
			payload = string(failPayload(eerr.Error()))
		} else {
			payload = out.Raw
			if out.Type == event.Null || payload == "" {
				payload = "{}"
			}
			if op.Resonator != nil && op.Resonator.Emit != nil {
				o, oerr := pu.OverlayResponse(op.Input, payload, op.Resonator.Emit.Overrides)
				if oerr != nil {
					status = "failed"
					payload = string(failPayload(oerr.Error()))
				} else {
					payload = o
				}
			}
		}
		if _, terr := pu.Runs.RecordDeferredTerminal(workCtx, runID, opc, status, []byte(payload)); terr != nil {
			pu.Logger.Error("local-async deferred: RecordDeferredTerminal failed",
				zap.String("run", runID), zap.String("opc", opc), zap.Error(terr))
			return
		}

		// Drive resume only if the run has suspended at the join for this op;
		// otherwise it's still in-request and the in-request join reads the
		// terminal directly.
		stage, exists, serr := pu.Runs.ReadDeferredSuspendedAt(workCtx, runID, opc)
		if serr != nil || !exists {
			return
		}
		pu.DriveDeferredResume(runID, stage)
	}()
}

// resumeDeferredJoin resumes a deferred-join suspend. Unlike the same-scope
// barrier (whose ops already ran), the join scope's OWN ops have not executed
// — the deferred op ran at an earlier scope. So this merges the deferred
// terminals onto the suspend envelope, marks each joined (so the re-run's
// floor check doesn't re-merge), then RE-RUNS the join scope via Run with a
// capture channel. A re-suspend during the re-run is silent (the resume
// identity is carried), and any OTHER still-pending joins resolve via the
// re-run's own floor check (the deferred identity is carried too).
func (pu *Unit) resumeDeferredJoin(ctx context.Context, runID, stage string, ss continuation.StageSuspended) error {
	start := time.Now() // resume-segment wall-clock, for the billing usage line
	merged := ss.ScopeEnvelope
	if merged == "" {
		merged = "{}"
	}
	rStack, rScope, _ := pu.StageParse(stage)

	entries := append([]continuation.OpManifestEntry(nil), ss.Manifest...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Ordinal < entries[j].Ordinal })
	for _, e := range entries {
		term, terr := pu.Runs.ReadDeferredTerminal(ctx, runID, e.OpContinuationID)
		if terr != nil {
			return terr
		}
		if term.Status == "failed" {
			var eb []byte
			if term.ErrorKey != "" {
				eb, _ = pu.Runs.Get(ctx, term.ErrorKey)
			}
			trace.FromContext(ctx).Step(trace.StepInfo{
				Stack: rStack, Scope: rScope, Name: e.Op, Transport: "async",
				Output: eb, StartedAt: ss.SuspendedAt, FinishedAt: term.RecordedAt,
				Status: "error", Error: "deferred op failed",
			})
			_ = pu.Runs.FailStage(ctx, runID, stage, "deferred-op-failed:"+e.Op)
			_ = pu.Runs.AppendEvent(ctx, runID, "stage.failed",
				map[string]any{"stage": stage, "op": e.Op})
			return nil
		}
		var ob []byte
		if term.OutputKey != "" {
			var gerr error
			ob, gerr = pu.Runs.Get(ctx, term.OutputKey)
			if gerr != nil {
				return gerr
			}
		}
		trace.FromContext(ctx).Step(trace.StepInfo{
			Stack: rStack, Scope: rScope, Name: e.Op, Transport: "async",
			Output: ob, StartedAt: ss.SuspendedAt, FinishedAt: term.RecordedAt,
			Status: "completed",
		})
		if s := string(ob); s != "" && s != "{}" {
			// Deferred resume output is untrusted: strip reserved _txc.* before merge.
			s = sanitizeAuthorOutput(s)
			if m, merr := pu.MergeJSON(merged, s); merr == nil {
				merged = m
			} else {
				pu.Logger.Warn("deferred resume merge error",
					zap.String("run", runID), zap.String("op", e.Op), zap.Error(merr))
			}
		}
		_, _ = pu.Runs.MarkJoined(ctx, runID, e.OpContinuationID)
	}

	// rcid for a re-suspend's (silent) 202 plumbing; SECURITY: tenant comes
	// from the chassis-stamped scope envelope, never merged worker output.
	rcid := ""
	if rc, rcErr := pu.Runs.ReadRunCreated(ctx, runID); rcErr == nil {
		rcid = rc.RunContinuationID
	}
	rctx := withResumeRun(ctx, runID, rcid)
	rctx = withDeferredRun(rctx, runID, rcid)
	rctx = WithTenant(rctx, gjson.Get(ss.ScopeEnvelope, "_txc.tenant").String())

	if snapData, serr := pu.Runs.ReadOpstackSnapshot(ctx, runID); serr == nil && len(snapData) > 0 {
		if snapDB, berr := buildSnapshotDB(snapData); berr != nil {
			pu.Logger.Warn("opstack snapshot rebuild failed; resuming against live opstack",
				zap.String("run", runID), zap.Error(berr))
		} else {
			defer snapDB.Close()
			rctx = context.WithValue(rctx, ctxKeyOpstackSnap, snapDB)
		}
	}

	// Re-run the join scope. Run's prelude re-runs the floor check (already-
	// joined ops are skipped) and then executes the join scope's own ops.
	capCh := make(chan event.Payload, 1)
	if rerr := pu.Run(rctx, merged, stage, capCh); rerr != nil {
		return rerr
	}
	select {
	case p := <-capCh:
		if werr := pu.Runs.WriteResult(ctx, runID, []byte(p.Raw)); werr != nil {
			return werr
		}
		_ = pu.Runs.AppendEvent(ctx, runID, "run.completed", map[string]any{"stage": stage})
		pu.emitResumeUsage(ss, []byte(p.Raw), runID, stage, time.Since(start))
	default:
		// Re-suspended at a later barrier/join; its callbacks drive the run.
	}
	return nil
}
