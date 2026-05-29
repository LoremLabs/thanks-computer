package continuation

// Deferred-join support: durable records for an async op that is
// *dispatched now* and *joins at a later scope* (internal docs/todo-deferred-join.md).
//
// Unlike a same-scope barrier op (whose records live under a fixed
// stage), a deferred op's join scope is resolved dynamically at run time
// (the first scope >= join_at_scope the run reaches). Its records are
// therefore keyed by the op-continuation id (opc), not a stage:
//
//	runs/{run_id}/deferred/{opc}/deferred-created.json
//	runs/{run_id}/deferred/{opc}/input.json
//	runs/{run_id}/deferred/{opc}/terminal.json   (+ output.json / error.json)
//	runs/{run_id}/pending-joins/{opc}.json        (one per outstanding join)
//
// The op-continuation lookup (op-continuations/{opc}.json) is shared with
// the same-scope path but marked Deferred so the callback handler records
// the terminal here instead of at a stage key. Everything is create-if-
// absent and immutable, exactly like the rest of the store.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---- doc types ------------------------------------------------------------

// DeferredOpCreated is the immutable record written when a deferred op is
// dispatched (no suspend). It captures everything the join needs later.
type DeferredOpCreated struct {
	OpContinuationID string    `json:"op_continuation_id"`
	Op               string    `json:"op"`
	Ordinal          int       `json:"ordinal"`
	JoinAtScope      int       `json:"join_at_scope"`
	Dest             string    `json:"dest,omitempty"`
	DispatchStage    string    `json:"dispatch_stage"`
	InputKey         string    `json:"input_key"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// PendingJoin is the in-flight-obligation record: one per outstanding
// deferred op on a run. Listed by the join check (and the sweeper) to
// find joins that must resolve at a scope >= JoinAtScope. Carries only
// what the join needs; the full record is DeferredOpCreated.
//
// DispatchStack is the stack the op was dispatched in. JoinAtScope is
// interpreted purely in that stack's scope numbering (scope numbers are
// not comparable across stacks), so the floor check only fires when the
// run is still in DispatchStack; a cross-stack transition force-resolves
// instead (internal docs/todo-deferred-join.md, single-stack constraint).
type PendingJoin struct {
	OpContinuationID string `json:"op_continuation_id"`
	Op               string `json:"op"`
	Ordinal          int    `json:"ordinal"`
	JoinAtScope      int    `json:"join_at_scope"`
	Dest             string `json:"dest,omitempty"`
	DispatchStack    string `json:"dispatch_stack,omitempty"`
}

// DeferredOpSpec is the input to CreateDeferredOp.
type DeferredOpSpec struct {
	OpContinuationID string
	Op               string
	Ordinal          int
	JoinAtScope      int
	Dest             string
	DispatchStage    string
	DispatchStack    string
	TokenHash        string
	Input            []byte
	ExpiresAt        time.Time
}

// ---- key builders ---------------------------------------------------------

func deferredDir(runID, opc string) string { return runDir(runID) + "/deferred/" + opc }
func deferredCreatedKey(runID, opc string) string {
	return deferredDir(runID, opc) + "/deferred-created.json"
}
func deferredInputKey(runID, opc string) string    { return deferredDir(runID, opc) + "/input.json" }
func deferredTerminalKey(runID, opc string) string { return deferredDir(runID, opc) + "/terminal.json" }
func deferredOutputKey(runID, opc string) string   { return deferredDir(runID, opc) + "/output.json" }
func deferredErrorKey(runID, opc string) string    { return deferredDir(runID, opc) + "/error.json" }

func pendingJoinsDir(runID string) string     { return runDir(runID) + "/pending-joins/" }
func pendingJoinKey(runID, opc string) string { return pendingJoinsDir(runID) + opc + ".json" }

func deferredJoinedKey(runID, opc string) string {
	return deferredDir(runID, opc) + "/joined.json"
}
func deferredSuspendedAtKey(runID, opc string) string {
	return deferredDir(runID, opc) + "/suspended-at.json"
}

// ---- lifecycle ------------------------------------------------------------

// CreateDeferredOp writes, create-if-absent: the input blob, the
// deferred-created doc, the op-continuation lookup (marked Deferred so the
// callback records here), and the pending-join record. Idempotent — a
// re-entered dispatch of the same opc is a no-op.
func (r *Runs) CreateDeferredOp(ctx context.Context, runID string, sp DeferredOpSpec) error {
	in := sp.Input
	if len(in) == 0 {
		in = []byte("{}")
	}
	ik := deferredInputKey(runID, sp.OpContinuationID)
	if _, err := r.s.Create(ctx, ik, bytes.NewReader(in), Meta{ContentType: "application/json"}); ignoreExists(err) != nil {
		return err
	}

	created := DeferredOpCreated{
		OpContinuationID: sp.OpContinuationID,
		Op:               sp.Op,
		Ordinal:          sp.Ordinal,
		JoinAtScope:      sp.JoinAtScope,
		Dest:             sp.Dest,
		DispatchStage:    sp.DispatchStage,
		InputKey:         ik,
		ExpiresAt:        sp.ExpiresAt,
		CreatedAt:        time.Now().UTC(),
	}
	if err := ignoreExists(r.createJSON(ctx, deferredCreatedKey(runID, sp.OpContinuationID), created)); err != nil {
		return err
	}

	lk := OpContinuationLookup{
		RunID: runID, Op: sp.Op, Ordinal: sp.Ordinal,
		TokenHash: sp.TokenHash, ExpiresAt: sp.ExpiresAt, Deferred: true,
	}
	if err := ignoreExists(r.createJSON(ctx, opContinuationKey(sp.OpContinuationID), lk)); err != nil {
		return err
	}

	pj := PendingJoin{
		OpContinuationID: sp.OpContinuationID, Op: sp.Op,
		Ordinal: sp.Ordinal, JoinAtScope: sp.JoinAtScope, Dest: sp.Dest,
		DispatchStack: sp.DispatchStack,
	}
	return ignoreExists(r.createJSON(ctx, pendingJoinKey(runID, sp.OpContinuationID), pj))
}

// RecordDeferredTerminal records a deferred op's result at its opc-keyed
// location. First terminal (success OR failure) wins; a duplicate/late
// callback gets ErrExists on the terminal doc ⇒ recorded=false, a harmless
// no-op. Mirrors RecordTerminal. status must be "completed" or "failed".
func (r *Runs) RecordDeferredTerminal(ctx context.Context, runID, opc, status string, payload []byte) (recorded bool, err error) {
	term := OpTerminal{Status: status, RecordedAt: time.Now().UTC()}
	switch status {
	case "completed":
		k := deferredOutputKey(runID, opc)
		if _, e := r.s.Create(ctx, k, bytes.NewReader(payload), Meta{ContentType: "application/json"}); ignoreExists(e) != nil {
			return false, e
		}
		term.OutputKey = k
	case "failed":
		k := deferredErrorKey(runID, opc)
		if _, e := r.s.Create(ctx, k, bytes.NewReader(payload), Meta{ContentType: "application/json"}); ignoreExists(e) != nil {
			return false, e
		}
		term.ErrorKey = k
	default:
		return false, fmt.Errorf("continuation: bad terminal status %q", status)
	}
	err = r.createJSON(ctx, deferredTerminalKey(runID, opc), term)
	if err == ErrExists {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ReadDeferredTerminal loads a deferred op's terminal doc. ErrNotFound
// (passed through) means the op hasn't completed yet — the join checker
// reads this to decide merge-vs-suspend.
func (r *Runs) ReadDeferredTerminal(ctx context.Context, runID, opc string) (OpTerminal, error) {
	b, _, err := r.s.Get(ctx, deferredTerminalKey(runID, opc))
	if err != nil {
		return OpTerminal{}, err
	}
	var d OpTerminal
	return d, json.Unmarshal(b, &d)
}

// MarkJoined records, create-if-absent, that a deferred op's result has
// been merged into the run. Returns won=true for the single writer that
// created the marker. The pending-join doc lingers (immutability), so the
// per-boundary join check calls this to merge a given opc EXACTLY ONCE: a
// second boundary (or a resume racing the in-request merge) sees won=false
// and skips the re-merge.
func (r *Runs) MarkJoined(ctx context.Context, runID, opc string) (won bool, err error) {
	e := r.createJSON(ctx, deferredJoinedKey(runID, opc), map[string]any{"joined_at": time.Now().UTC()})
	if e == ErrExists {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	return true, nil
}

// IsJoined reports whether a deferred op has already been merged.
func (r *Runs) IsJoined(ctx context.Context, runID, opc string) (bool, error) {
	return r.s.Exists(ctx, deferredJoinedKey(runID, opc))
}

// SetDeferredSuspendedAt records (create-if-absent) the join scope a run
// suspended at while waiting for a deferred op. The worker callback reads
// this to learn which dynamic stage to ClaimResume — without it the
// callback can't find the suspend (the join scope isn't known at dispatch).
func (r *Runs) SetDeferredSuspendedAt(ctx context.Context, runID, opc, stage string) error {
	return ignoreExists(r.createJSON(ctx, deferredSuspendedAtKey(runID, opc),
		map[string]any{"stage": stage, "at": time.Now().UTC()}))
}

// ReadDeferredSuspendedAt returns the join stage the run suspended at for
// this op, or exists=false if the run hasn't suspended on it (still
// in-request — the in-request join will pick up the terminal itself).
func (r *Runs) ReadDeferredSuspendedAt(ctx context.Context, runID, opc string) (stage string, exists bool, err error) {
	b, _, gerr := r.s.Get(ctx, deferredSuspendedAtKey(runID, opc))
	if gerr == ErrNotFound {
		return "", false, nil
	}
	if gerr != nil {
		return "", false, gerr
	}
	var d struct {
		Stage string `json:"stage"`
	}
	if uerr := json.Unmarshal(b, &d); uerr != nil {
		return "", false, uerr
	}
	return d.Stage, true, nil
}

// ListPendingJoins returns every outstanding deferred-join record on a run.
// Used by the per-boundary join check and the sweeper. Order is not
// guaranteed; callers sort by Ordinal where determinism matters.
func (r *Runs) ListPendingJoins(ctx context.Context, runID string) ([]PendingJoin, error) {
	keys, err := r.s.List(ctx, pendingJoinsDir(runID))
	if err != nil {
		return nil, err
	}
	out := make([]PendingJoin, 0, len(keys))
	for _, k := range keys {
		if !strings.HasSuffix(k, ".json") {
			continue
		}
		b, _, gerr := r.s.Get(ctx, k)
		if gerr != nil {
			return nil, gerr
		}
		var pj PendingJoin
		if uerr := json.Unmarshal(b, &pj); uerr != nil {
			return nil, uerr
		}
		out = append(out, pj)
	}
	return out, nil
}
