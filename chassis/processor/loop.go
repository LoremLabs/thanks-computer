// The LOOP clause: `LOOP [EVERY <d>] [SET …] UNTIL <predicate> [MAX <n>]`.
//
// A looping op re-executes its EXEC inside its own dispatch goroutine
// until the predicate holds against the accumulated view or a budget
// runs out, merging each pass's output locally, and hands the scope
// merge ONE payload. The scope therefore does not advance until the
// loop finishes — the property `@goto` loops cannot give you, because
// a goto re-enters the stage and every sibling op re-fires with it.
//
// Per iteration: dispatch with the current input, run the usual
// post-exec tail, merge the output into both the accumulator (the op's
// contribution) and the view (envelope + everything merged so far),
// then check, in this order: error, fuel, timeout/halt, UNTIL, MAX.
// Only if the loop continues: pause EVERY, apply the LOOP SET
// assignments (resolved against the view, written onto the frozen base
// input and the view), re-resolve WITH against the view, and go again.
// The first pass is exactly the single-shot call: SET never runs
// before it. The loop never fails the run on its own: a mid-loop
// error, a budget hit or a sibling's halt truncates and flags — the
// accumulated output is kept and the step carries the reason.
//
// Design: internal docs/todo-exec-loop.md.

package processor

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/txcl/runtime"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

const (
	loopDefaultMax   = int64(10)             // LOOP without MAX
	loopDefaultEvery = 50 * time.Millisecond // LOOP without EVERY
	loopMinEvery     = 2 * time.Millisecond  // busy-loop floor; anything lower is raised to it
)

// Stop reasons, as recorded on the trace step (StopReason) and the
// envelope bookkeeping (`_txc.runtime.loop.<op>.stop`).
const (
	loopStopDone    = "done"    // UNTIL held
	loopStopMax     = "max"     // MAX passes ran
	loopStopFuel    = "fuel"    // request fuel ceiling crossed
	loopStopTimeout = "timeout" // op ctx done (WITH timeout / --loop-timeout / request deadline)
	loopStopHalted  = "halted"  // a sibling op at this stage emitted _txc.halt
	loopStopError   = "error"   // a pass, a LOOP SET, or a WITH re-resolution failed
)

// loopTransportAdmitted reports whether the EXEC scheme may loop. The
// parser already refused the shapes that can never progress (no EXEC,
// noop, stage jumps); this is the set the runner has been proven on.
// compute:// and ai:// are held back deliberately: a compute error is
// fatal on the single-shot path while a loop truncates, and fuel does
// not meter model time — both need a decision before they loop.
// workspace:// is admitted: its failures are in-band data (never a
// fatal single-shot error) and its wall-clock is fuel-metered.
func loopTransportAdmitted(exec string) bool {
	for _, prefix := range []string{"txco://", "http://", "https://", "mcp+http://", "mcp+https://", "workspace://"} {
		if strings.HasPrefix(exec, prefix) {
			return true
		}
	}
	return false
}

// loopAdmit is the dispatch-time gate for a looping op. A rejected op
// is logged loudly and dropped from the merge for this request — the
// request proceeds without its contribution, mirroring the
// op-timeout-max rejection. The parser and `txco apply` refuse the
// same shapes statically, so this fires only on what they cannot see
// (a path-valued mode, an operator ceiling).
func (pu *Unit) loopAdmit(op operation.Operation) (max int64, ok bool) {
	reject := func(reason string, extra ...zap.Field) (int64, bool) {
		fields := append([]zap.Field{
			zap.String("stack", op.Stack),
			zap.Int("scope", op.Scope),
			zap.String("name", op.Name),
			zap.String("reason", reason),
		}, extra...)
		pu.Logger.Error("LOOP op rejected; dropping op", fields...)
		return 0, false
	}
	exec := op.Resonator.Exec
	if !loopTransportAdmitted(exec) {
		return reject("LOOP is not admitted for this EXEC scheme in this version", zap.String("exec", exec))
	}
	if mode := gjson.Get(op.Meta, "mode").String(); mode == "async" || mode == "continuable" {
		return reject("LOOP cannot combine with WITH mode = "+mode, zap.String("mode", mode))
	}
	max = op.Resonator.Loop.Max
	if max <= 0 {
		max = loopDefaultMax
	}
	if ceiling := int64(pu.Conf.OpLoopMax); ceiling > 0 && max > ceiling {
		return reject("LOOP MAX exceeds op-loop-max", zap.Int64("requested", max), zap.Int64("max", ceiling))
	}
	return max, true
}

// runLoop drives the loop for one admitted op and leaves the op's whole
// contribution on op.Output: every pass's output merged with the
// scope-merge semantics (scalars overwrite, objects deep-merge, arrays
// append), plus the bookkeeping object at
// `_txc.runtime.loop.<op name>` = {passes, stop, elapsed_ms, error?}.
// `_txc.runtime.*` is chassis-only (no author-writable allowlist admits
// it) and the live sync merge passes trusted txco output raw, so a later
// scope can gate on `@runtime.loop.<name>.stop`. Keyed by op name so two
// loops in one scope never clobber each other.
//
// halt, when non-nil, is closed by Run when a sibling op's response at
// this stage carries _txc.halt: the loop is cut mid-pass or mid-pause
// and stops with "halted". A sibling's goto does not cut it — that is a
// later part of the stage lifecycle, and the loop's output rides along.
//
// One trace step spans the loop (Passes, StopReason, Output = the
// accumulator); each pass additionally writes an `op.pass` timeline
// event, which only the file sink keeps.
func (pu *Unit) runLoop(ctx context.Context, op *operation.Operation, max int64, halt <-chan struct{}) {
	loop := op.Resonator.Loop
	stage := op.Stack + "/" + strconv.Itoa(op.Scope)
	every := loop.Every
	if every == 0 {
		every = loopDefaultEvery
	}
	if every < loopMinEvery {
		every = loopMinEvery
	}

	// lctx is the loop's own context: the op deadline from ctx, plus
	// the sibling-halt cut.
	lctx, lcancel := context.WithCancel(ctx)
	defer lcancel()
	if halt != nil {
		go func() {
			select {
			case <-halt:
				lcancel()
			case <-lctx.Done():
			}
		}()
	}
	// cut names why lctx ended: the sibling halt wins over the deadline.
	cut := func() string {
		select {
		case <-halt:
			return loopStopHalted
		default:
			return loopStopTimeout
		}
	}

	origFull, baseInput := op.FullInput, op.Input
	view := op.EnvelopeView()
	acc := "{}"
	start := time.Now()
	stop, errText := "", ""
	passes := 0
	var first execResult

passes:
	for pass := int64(1); ; pass++ {
		if pass > 1 {
			// Pause, then feed the next pass. The pause comes after the
			// previous iteration's checks, so a loop that is done never
			// pays it.
			select {
			case <-time.After(every):
			case <-lctx.Done():
				stop = cut()
				break passes
			}
			if len(loop.Set) > 0 {
				// Resolve every assignment against the view IN ORDER, each
				// one seeing the writes before it — so `._n = &add(._n, 1),
				// ._cell = &get(._cells, &concat("", ._n))` advances a
				// counter and then looks up by the NEW counter in one SET.
				// (OverlayResponseFor resolves all of a clause's values
				// against the pre-overlay view, which is right for EMIT and
				// wrong here.) Each accepted write lands on the view and on
				// the frozen base input for this pass; reserved _txc.*
				// paths are dropped, same as SET.
				in := baseInput
				for _, bv := range loop.Set {
					path := strings.TrimPrefix(bv.Path, ".")
					val, err := runtime.Resolve(bv.Value, runtime.JSONEnv(view))
					if err != nil {
						stop, errText = loopStopError, "LOOP SET "+bv.Path+": "+err.Error()
						break passes
					}
					if !authorMayWriteTxc(path) {
						pu.Logger.Debug("loop set: dropped reserved control write",
							zap.String("name", op.Name), zap.String("path", path))
						continue
					}
					if nv, serr := sjson.Set(view, path, val); serr == nil {
						view = nv
					}
					if ni, serr := sjson.Set(in, path, val); serr == nil {
						in = ni
					}
				}
				op.Input = in
			}
			// Re-resolve WITH against the view so a cursor written by
			// the previous pass (`after = ._p.next`) advances.
			meta, err := pu.resolveWith(op.Resonator, runtime.JSONEnv(view))
			if err != nil {
				stop, errText = loopStopError, err.Error()
				break passes
			}
			op.Meta = meta
		}

		r := pu.dispatch(lctx, *op)
		passes++
		if pass == 1 {
			first = r
		}
		if r.err != nil {
			// A pass cut short by the loop's own context is a timeout or
			// a halt, not an op failure: the accumulated output stands.
			if lctx.Err() != nil {
				stop = cut()
			} else {
				stop, errText = loopStopError, r.err.Error()
			}
			break passes
		}

		// Post-exec tail, with EMIT resolving against the view this
		// pass ran in (EnvelopeView reads FullInput).
		op.FullInput = view
		op.Output = ""
		pu.finishOutput(lctx, op, r.payload, r.transport)
		out := op.Output
		if out != "" && out != "{}" {
			if merged, merr := pu.MergeJSON(acc, out); merr != nil {
				pu.Logger.Warn("loop merge", zap.String("name", op.Name), zap.Error(merr))
			} else {
				acc = merged
			}
			if merged, merr := pu.MergeJSON(view, out); merr != nil {
				pu.Logger.Warn("loop view merge", zap.String("name", op.Name), zap.Error(merr))
			} else {
				view = merged
			}
		}
		trace.FromContext(ctx).Event(trace.TimelineEvent{
			Ts:    r.finishedAt,
			Event: "op.pass",
			Fields: map[string]any{
				"stack":        op.Stack,
				"scope":        op.Scope,
				"name":         op.Name,
				"pass":         passes,
				"duration_ms":  r.finishedAt.Sub(r.startedAt).Milliseconds(),
				"output_bytes": len(out),
			},
		})

		if ferr := fuelExceeded(ctx, stage); ferr != nil {
			stop = loopStopFuel
			break passes
		}
		if lctx.Err() != nil {
			stop = cut()
			break passes
		}
		if loop.Until.Matches(view) {
			stop = loopStopDone
			break passes
		}
		if pass >= max {
			stop = loopStopMax
			break passes
		}
	}
	op.Input, op.FullInput = baseInput, origFull
	finished := time.Now()

	book := map[string]any{
		"passes":     passes,
		"stop":       stop,
		"elapsed_ms": finished.Sub(start).Milliseconds(),
	}
	if errText != "" {
		book["error"] = errText
	}
	if withBook, err := sjson.Set(acc, "_txc.runtime.loop."+sjsonKey(op.Name), book); err == nil {
		acc = withBook
	} else {
		pu.Logger.Warn("loop bookkeeping", zap.String("name", op.Name), zap.Error(err))
	}

	status := "ok"
	if stop == loopStopError {
		status = "error"
	}
	pu.Logger.Debug("loop",
		zap.String("stack", op.Stack), zap.Int("scope", op.Scope), zap.String("name", op.Name),
		zap.Int("passes", passes), zap.String("stop", stop), zap.String("err", errText))
	pu.recordStep(ctx, *op, first, trace.StepInfo{
		Output:     []byte(acc),
		StartedAt:  start,
		FinishedAt: finished,
		Status:     status,
		Error:      errText,
		Passes:     passes,
		StopReason: stop,
	})
	op.Output = acc
}

// dropLoopsOutsideRequest removes, with a loud log and an error trace
// step, any looping op whose resolved WITH mode would route it through
// the async or continuable paths — neither ever reaches the loop
// runner, so admitting it there would silently run one pass and leak
// the loop's intent. The parser refuses literal modes at apply time;
// this catches a path-valued one.
func (pu *Unit) dropLoopsOutsideRequest(ctx context.Context, ops []operation.Operation) []operation.Operation {
	kept := ops[:0]
	for _, op := range ops {
		if op.Resonator == nil || op.Resonator.Loop == nil || (!isAsyncOp(op) && !isContinuableOp(op)) {
			kept = append(kept, op)
			continue
		}
		mode := gjson.Get(op.Meta, "mode").String()
		reason := "LOOP cannot combine with WITH mode = " + mode
		pu.Logger.Error("LOOP op rejected; dropping op",
			zap.String("stack", op.Stack), zap.Int("scope", op.Scope),
			zap.String("name", op.Name), zap.String("reason", reason))
		now := time.Now()
		trace.FromContext(ctx).Step(trace.StepInfo{
			Stack: op.Stack, Scope: op.Scope, Name: op.Name, Operation: op.Resonator.Exec,
			Txcl: op.Txcl, Input: []byte(op.Input), StartedAt: now, FinishedAt: now,
			Status: "error", Error: reason,
		})
	}
	return kept
}

// sjsonKey escapes one path component so an op name containing path
// metacharacters (`.`, `*`, `?`, `|`, `#`, `@`, `:`) addresses a single
// key rather than a nested path or a wildcard.
func sjsonKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '\\', '.', '*', '?', '|', '#', '@', ':':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
