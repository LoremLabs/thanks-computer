// In-op repetition: `WITH repeat_until = <predicate>, repeat_max = N`.
//
// A repeating op re-executes its EXEC inside its own dispatch goroutine
// until the predicate holds against the accumulated view or a budget
// runs out, merging each pass's output locally, and hands the scope
// merge ONE payload. The scope therefore does not advance until the
// loop finishes — the property `@goto` loops cannot give you, because
// a goto re-enters the stage and every sibling op re-fires with it.
//
// Per pass: WITH is re-resolved against the view (envelope + everything
// merged so far), the `repeat_*` family is stripped, the op is
// dispatched with its input frozen, the usual post-exec tail runs, and
// the output is merged into both the accumulator (the op's
// contribution) and the view (next pass's environment). Stop reasons,
// checked in this order after every pass: error, fuel, timeout, done,
// max. The loop never fails the run on its own: a mid-loop error
// truncates and flags (accumulated output is kept, the step carries
// the error) — the same rule as a budget hit.
//
// v1 admits `txco://` EXECs only (see repeatAdmit); the runner itself
// is transport-agnostic. Design: internal docs/todo-exec-loop.md.

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

// The chassis-owned WITH keys of the repeat family. repeat_until never
// reaches Meta (the parser lands it on Resonator.RepeatUntil); the other
// two ride Meta like any WITH key and are consumed and stripped here.
const (
	withRepeatMax    = "repeat_max"
	withRepeatBudget = "repeat_budget" // reserved: per-loop wall clock (not built)
)

// Stop reasons, as recorded on the trace step (StopReason) and the
// envelope bookkeeping (`_txc.runtime.repeat.<op>.stop`).
const (
	repeatStopDone    = "done"    // predicate held
	repeatStopMax     = "max"     // repeat_max passes ran
	repeatStopFuel    = "fuel"    // request fuel ceiling crossed
	repeatStopTimeout = "timeout" // op ctx done (WITH timeout / request deadline)
	repeatStopError   = "error"   // a pass or a WITH re-resolution failed
)

// repeatDirectives strips the repeat_* family from an op's Meta and
// returns repeat_max as an integer (a JSON number, or a numeric string —
// the same tolerance `timeout` has). hasMax is false when the key is
// absent or not a number; hasBudget reports a reserved repeat_budget so
// the caller can say it is not built yet. Meta without either key comes
// back unchanged.
func repeatDirectives(meta string) (stripped string, max int64, hasMax bool, hasBudget bool) {
	if meta == "" || !strings.Contains(meta, "repeat_") {
		return meta, 0, false, false
	}
	stripped = meta
	if v := gjson.Get(meta, withRepeatMax); v.Exists() {
		switch v.Type {
		case gjson.Number:
			max, hasMax = v.Int(), true
		case gjson.String:
			if n, err := strconv.ParseInt(strings.TrimSpace(v.String()), 10, 64); err == nil {
				max, hasMax = n, true
			}
		}
		stripped, _ = sjson.Delete(stripped, withRepeatMax)
	}
	if gjson.Get(meta, withRepeatBudget).Exists() {
		hasBudget = true
		stripped, _ = sjson.Delete(stripped, withRepeatBudget)
	}
	return stripped, max, hasMax, hasBudget
}

// repeatAdmit is the dispatch-time gate for a repeating op. A rejected
// op is logged loudly and dropped from the merge for this request —
// the request proceeds without its contribution, mirroring the
// op-timeout-max rejection. `txco apply` lints the same conditions so
// authors normally see them before deploy.
func (pu *Unit) repeatAdmit(op operation.Operation, max int64, hasMax, hasBudget bool) bool {
	reject := func(reason string, extra ...zap.Field) bool {
		fields := append([]zap.Field{
			zap.String("stack", op.Stack),
			zap.Int("scope", op.Scope),
			zap.String("name", op.Name),
			zap.String("reason", reason),
		}, extra...)
		pu.Logger.Error("repeat_until op rejected; dropping op", fields...)
		return false
	}
	if hasBudget {
		pu.Logger.Debug("WITH repeat_budget is reserved and not yet supported; ignoring",
			zap.String("stack", op.Stack), zap.Int("scope", op.Scope), zap.String("name", op.Name))
	}
	if !strings.HasPrefix(op.Resonator.Exec, "txco://") {
		return reject("repeat_until requires a txco:// EXEC in this version",
			zap.String("exec", op.Resonator.Exec))
	}
	if !hasMax || max <= 0 {
		return reject("repeat_until requires repeat_max, a positive integer pass ceiling")
	}
	if ceiling := int64(pu.Conf.OpRepeatMax); ceiling > 0 && max > ceiling {
		return reject("repeat_max exceeds op-repeat-max",
			zap.Int64("requested", max), zap.Int64("max", ceiling))
	}
	return true
}

// runRepeat drives the loop for one admitted op and leaves the op's
// whole contribution on op.Output: every pass's output merged with the
// scope-merge semantics (scalars overwrite, objects deep-merge, arrays
// append), plus the bookkeeping object at
// `_txc.runtime.repeat.<op name>` = {passes, stop, elapsed_ms, error?}.
// `_txc.runtime.*` is chassis-only (no author-writable allowlist admits
// it) and the live sync merge passes trusted txco output raw, so a later
// scope can gate on `@runtime.repeat.<name>.stop`. Keyed by op name so
// two loops in one scope never clobber each other.
//
// One trace step spans the loop (Passes, StopReason, Output = the
// accumulator); each pass additionally writes an `op.pass` timeline
// event, which only the file sink keeps.
func (pu *Unit) runRepeat(ctx context.Context, op *operation.Operation, max int64) {
	stage := op.Stack + "/" + strconv.Itoa(op.Scope)
	origFull := op.FullInput
	view := op.EnvelopeView()
	acc := "{}"
	start := time.Now()
	stop, errText := "", ""
	passes := 0
	var first execResult

	for pass := int64(1); ; pass++ {
		if pass > 1 {
			// Re-resolve WITH against the accumulated view so a cursor
			// (`after = ._p.next`) advances; the repeat_* family is
			// stripped again because resolution rebuilds Meta.
			meta, err := pu.resolveWith(op.Resonator, runtime.JSONEnv(view))
			if err != nil {
				stop, errText = repeatStopError, err.Error()
				break
			}
			op.Meta, _, _, _ = repeatDirectives(meta)
		}

		// op.Input stays frozen: parameters travel by WITH, and a
		// deterministic input is what makes a pass reproducible.
		r := pu.dispatch(ctx, *op)
		passes++
		if pass == 1 {
			first = r
		}
		if r.err != nil {
			// A pass cut short by the op's own deadline is a timeout,
			// not an op failure: the accumulated output is still good.
			if ctx.Err() != nil {
				stop = repeatStopTimeout
			} else {
				stop, errText = repeatStopError, r.err.Error()
			}
			break
		}

		// Post-exec tail, with EMIT resolving against the view this
		// pass ran in (EnvelopeView reads FullInput).
		op.FullInput = view
		op.Output = ""
		pu.finishOutput(ctx, op, r.payload, r.transport)
		out := op.Output
		if out != "" && out != "{}" {
			if merged, merr := pu.MergeJSON(acc, out); merr != nil {
				pu.Logger.Warn("repeat merge", zap.String("name", op.Name), zap.Error(merr))
			} else {
				acc = merged
			}
			if merged, merr := pu.MergeJSON(view, out); merr != nil {
				pu.Logger.Warn("repeat view merge", zap.String("name", op.Name), zap.Error(merr))
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
			stop = repeatStopFuel
			break
		}
		if ctx.Err() != nil {
			stop = repeatStopTimeout
			break
		}
		if op.Resonator.RepeatUntil.Matches(view) {
			stop = repeatStopDone
			break
		}
		if pass >= max {
			stop = repeatStopMax
			break
		}
	}
	op.FullInput = origFull
	finished := time.Now()

	book := map[string]any{
		"passes":     passes,
		"stop":       stop,
		"elapsed_ms": finished.Sub(start).Milliseconds(),
	}
	if errText != "" {
		book["error"] = errText
	}
	if withBook, err := sjson.Set(acc, "_txc.runtime.repeat."+sjsonKey(op.Name), book); err == nil {
		acc = withBook
	} else {
		pu.Logger.Warn("repeat bookkeeping", zap.String("name", op.Name), zap.Error(err))
	}

	status := "ok"
	if stop == repeatStopError {
		status = "error"
	}
	pu.Logger.Debug("repeat",
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
