package processor

// Scope fast-forward: when a stage's entry evaluation matches NOTHING,
// the pre-fast-forward path spawned one full recursive Run frame per
// populated scope — span/goroutine/WaitGroup scaffolding, a getnext
// probe, and advanceAfterScope's envelope scans — just to discover the
// next scope was dead too. An unmatched GET on a ~96-scope stack paid
// that ~96 times before reaching its fallback. fastForward replaces the
// chain of dead frames with a tight loop over the (index-backed)
// ladder: evaluate each scope's pre-parsed WHENs, hop on failure, and
// re-enter the full stage machinery only at the first scope where
// something resonates.
//
// Soundness: while no op fires, nothing merges, so the only envelope
// fields that change between frames are the budget counters — and the
// loop replays exactly the per-frame budget work (TTL hop, scope-enter
// fuel, transition record, syncBudgetToEnvelope) that each replaced
// recursion performed, in the same order with the same stage labels.
// The final envelope — including `_txc.fuel_used` — is byte-identical
// to the recursive path. Deliberate mirror quirk: the pre-fast-forward
// path evaluates the entry floor scope TWICE (once in the entry frame,
// once in its successor frame, which charges for the re-visit); the
// loop's first iteration reproduces that re-evaluation so the charge
// sequence matches.
//
// Callers gate on (see Run):
//   - no deferred run in flight (the join-floor check runs per frame)
//   - `_txc.flag_breakpoint` unset (break=N may halt at an empty
//     intermediate scope; skipping would overshoot — same precedent as
//     "breakpoints pre-empt streaming", and it keeps full per-scope
//     traces in `txco dev`)
//   - the ops index covers this request's snapshot (hops are
//     OpsForStage calls; only the index path makes them ~free)
//
// The `_sys` → tenant handoff and admission checks cannot occur inside
// a fast-forward: the tenant pin only changes via a goto from a FIRED
// op, and the loop stops at the first firing scope.

import (
	"context"
	"strconv"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// fastForward walks forward from `stage` until a scope resonates or the
// ladder is exhausted. Returns the resonated (WHEN-matched, SET-PRE/WITH
// decorated) ops at the landing scope, the landing stage, and the
// budget-synced envelope for the caller to continue with. On ladder
// exhaustion ops is empty — Run's existing empty-dispatch flow then
// emits the final response exactly as the last frame does on the
// recursive path. stopped=true means a budget-exhaustion response was
// already emitted on resCh (mirrors Run's entry handling); the caller
// returns nil.
func (pu *Unit) fastForward(ctx context.Context, raw, stage string, resCh chan event.Payload) (ops []operation.Operation, landedStage, newRaw string, stopped bool, err error) {
	curStack, curScope, perr := pu.StageParse(stage)
	if perr != nil {
		// The same stage just parsed inside OpsForStage; treat a failure
		// here as nothing-to-walk and let the normal flow answer.
		return nil, stage, raw, false, nil
	}

	// First hop = the oracle's own successor floor (entryScope+1, what
	// nextStageFor computes). Two cases, both mirrored exactly: an entry
	// BELOW its floor set (www/0 → ops at 1000) recurses INTO that same
	// scope and re-evaluates it with fresh frame charges; an entry AT an
	// exact populated scope moves straight past it. Index path ⇒ a map
	// hit either way.
	cur, ferr := pu.OpsForStage(ctx, curStack+"/"+strconv.Itoa(curScope+1))
	if ferr != nil {
		return nil, stage, raw, false, ferr
	}

	prev := stage
	hops := 0
	evaluated := 0
	for len(cur) > 0 {
		hopStage := curStack + "/" + strconv.Itoa(cur[0].Scope)

		// Mirror of the advance-side sync (advanceAfterScope runs
		// syncBudgetToEnvelope BEFORE recursing): the envelope carries
		// the budget as of the PREVIOUS frame, so a halt payload never
		// includes the landing frame's own charges — byte-identical to
		// the recursive path.
		raw = syncBudgetToEnvelope(ctx, raw)

		// Frame-entry charges — exactly what the recursive Run this hop
		// replaces would have paid (Run's TTL/fuel/transition block).
		// Forward-only hops can never repeat a transition, so the
		// penalty sleep is unreachable; it is honored anyway so behavior
		// stays identical if that invariant ever shifts.
		if berr := decrementTTL(ctx, hopStage); berr != nil {
			if emitBudgetExhausted(berr, resCh) {
				return nil, hopStage, raw, true, nil
			}
			return nil, hopStage, raw, false, berr
		}
		if berr := addFuel(ctx, fuelCostScopeEnter, hopStage); berr != nil {
			if emitBudgetExhausted(berr, resCh) {
				return nil, hopStage, raw, true, nil
			}
			return nil, hopStage, raw, false, berr
		}
		penalty, berr := chargeTransition(ctx, prev, hopStage)
		if berr != nil {
			if emitBudgetExhausted(berr, resCh) {
				return nil, hopStage, raw, true, nil
			}
			return nil, hopStage, raw, false, berr
		}
		if penalty > 0 {
			select {
			case <-time.After(penalty):
			case <-ctx.Done():
				return nil, hopStage, raw, false, ctx.Err()
			}
		}

		// A floor lookup returns a single scope's ops, but tolerate a
		// mixed set by advancing past the highest member.
		nextFloor := cur[0].Scope + 1
		for i := range cur {
			if cur[i].Scope+1 > nextFloor {
				nextFloor = cur[i].Scope + 1
			}
		}
		n := len(cur)
		resonated, rerr := pu.ResonatingOps(raw, cur, "")
		if rerr != nil {
			return nil, hopStage, raw, false, rerr
		}
		evaluated += n
		if len(resonated) > 0 {
			pu.emitFastForward(ctx, stage, hopStage, hops, evaluated)
			return resonated, hopStage, raw, false, nil
		}

		hops++
		prev = hopStage
		cur, ferr = pu.OpsForStage(ctx, curStack+"/"+strconv.Itoa(nextFloor))
		if ferr != nil {
			return nil, hopStage, raw, false, ferr
		}
	}

	// Ladder exhausted: nothing at-or-above the entry can fire.
	pu.emitFastForward(ctx, stage, prev, hops, evaluated)
	return nil, prev, raw, false, nil
}

// emitFastForward records the whole skipped walk as ONE timeline event
// (replacing the per-frame stage.enter chain the skipped recursions
// would have written). Both trace backends carry TimelineEvent fields
// generically, so no sink changes are needed.
func (pu *Unit) emitFastForward(ctx context.Context, from, to string, scopes, opsEvaluated int) {
	fields := map[string]any{
		"from":          from,
		"to":            to,
		"scopes":        scopes,
		"ops_evaluated": opsEvaluated,
	}
	if b := budgetFromCtx(ctx); b != nil {
		fields["fuel_used"] = b.fuel.Load()
		fields["ttl"] = b.ttl.Load()
	}
	trace.FromContext(ctx).Event(trace.TimelineEvent{
		Ts:     time.Now(),
		Event:  "stage.fastforward",
		Fields: fields,
	})
}
