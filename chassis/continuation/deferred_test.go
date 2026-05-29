package continuation_test

import (
	"context"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
)

// newRuns builds a Runs over a fresh temp filestore.
func newRuns(t *testing.T) *continuation.Runs {
	t.Helper()
	return continuation.NewRuns(newStore(t))
}

func TestDeferredOpLifecycle(t *testing.T) {
	ctx := context.Background()
	r := newRuns(t)

	runID, _, err := r.CreateRun(ctx, "tnt", "web", "", "web/50", "rid-1",
		time.Now().UTC().Add(10*time.Minute))
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	opc := "opc_research"
	sp := continuation.DeferredOpSpec{
		OpContinuationID: opc,
		Op:               "research",
		Ordinal:          0,
		JoinAtScope:      200,
		Dest:             ".research",
		DispatchStage:    "web/50",
		TokenHash:        "hash123",
		Input:            []byte(`{"topic":"tcp"}`),
		ExpiresAt:        time.Now().UTC().Add(10 * time.Minute),
	}
	if err := r.CreateDeferredOp(ctx, runID, sp); err != nil {
		t.Fatalf("CreateDeferredOp: %v", err)
	}
	// Idempotent re-dispatch.
	if err := r.CreateDeferredOp(ctx, runID, sp); err != nil {
		t.Fatalf("CreateDeferredOp (re-entry): %v", err)
	}

	// Pending join is listed.
	pjs, err := r.ListPendingJoins(ctx, runID)
	if err != nil {
		t.Fatalf("ListPendingJoins: %v", err)
	}
	if len(pjs) != 1 {
		t.Fatalf("pending joins = %d, want 1", len(pjs))
	}
	if pjs[0].OpContinuationID != opc || pjs[0].JoinAtScope != 200 || pjs[0].Dest != ".research" {
		t.Fatalf("pending join = %+v, unexpected", pjs[0])
	}

	// No terminal yet.
	if _, err := r.ReadDeferredTerminal(ctx, runID, opc); err != continuation.ErrNotFound {
		t.Fatalf("ReadDeferredTerminal before completion: err = %v, want ErrNotFound", err)
	}

	// First terminal wins.
	recorded, err := r.RecordDeferredTerminal(ctx, runID, opc, "completed", []byte(`{"summary":"ok"}`))
	if err != nil {
		t.Fatalf("RecordDeferredTerminal: %v", err)
	}
	if !recorded {
		t.Fatal("first RecordDeferredTerminal recorded = false, want true")
	}

	// Duplicate/late callback is a no-op.
	recorded, err = r.RecordDeferredTerminal(ctx, runID, opc, "completed", []byte(`{"summary":"DIFFERENT"}`))
	if err != nil {
		t.Fatalf("RecordDeferredTerminal (dup): %v", err)
	}
	if recorded {
		t.Fatal("duplicate RecordDeferredTerminal recorded = true, want false (single-use)")
	}

	// Terminal reads back as the FIRST payload (immutable).
	term, err := r.ReadDeferredTerminal(ctx, runID, opc)
	if err != nil {
		t.Fatalf("ReadDeferredTerminal: %v", err)
	}
	if term.Status != "completed" {
		t.Fatalf("terminal status = %q, want completed", term.Status)
	}
	out, err := r.Get(ctx, term.OutputKey)
	if err != nil {
		t.Fatalf("Get output: %v", err)
	}
	if string(out) != `{"summary":"ok"}` {
		t.Fatalf("output = %s, want first payload (immutable)", out)
	}
}

func TestDeferredOpFailedTerminal(t *testing.T) {
	ctx := context.Background()
	r := newRuns(t)
	runID, _, err := r.CreateRun(ctx, "tnt", "web", "", "web/50", "rid-2", time.Time{})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	opc := "opc_x"
	if err := r.CreateDeferredOp(ctx, runID, continuation.DeferredOpSpec{
		OpContinuationID: opc, Op: "x", Ordinal: 0, JoinAtScope: 100, DispatchStage: "web/50",
	}); err != nil {
		t.Fatalf("CreateDeferredOp: %v", err)
	}
	recorded, err := r.RecordDeferredTerminal(ctx, runID, opc, "failed", []byte(`{"error":"boom"}`))
	if err != nil || !recorded {
		t.Fatalf("RecordDeferredTerminal failed: recorded=%v err=%v", recorded, err)
	}
	term, err := r.ReadDeferredTerminal(ctx, runID, opc)
	if err != nil {
		t.Fatalf("ReadDeferredTerminal: %v", err)
	}
	if term.Status != "failed" || term.ErrorKey == "" {
		t.Fatalf("terminal = %+v, want failed with ErrorKey", term)
	}
}

func TestMarkJoinedFireOnce(t *testing.T) {
	ctx := context.Background()
	r := newRuns(t)
	runID, _, _ := r.CreateRun(ctx, "tnt", "web", "", "web/50", "rid-j", time.Time{})
	opc := "opc_j"

	if joined, err := r.IsJoined(ctx, runID, opc); err != nil || joined {
		t.Fatalf("IsJoined before mark = (%v,%v), want (false,nil)", joined, err)
	}
	won, err := r.MarkJoined(ctx, runID, opc)
	if err != nil || !won {
		t.Fatalf("first MarkJoined = (%v,%v), want (true,nil)", won, err)
	}
	// Second writer (e.g. a resume racing the in-request merge) loses.
	won, err = r.MarkJoined(ctx, runID, opc)
	if err != nil || won {
		t.Fatalf("second MarkJoined = (%v,%v), want (false,nil)", won, err)
	}
	if joined, err := r.IsJoined(ctx, runID, opc); err != nil || !joined {
		t.Fatalf("IsJoined after mark = (%v,%v), want (true,nil)", joined, err)
	}
}

func TestDeferredSuspendedAt(t *testing.T) {
	ctx := context.Background()
	r := newRuns(t)
	runID, _, _ := r.CreateRun(ctx, "tnt", "web", "", "web/50", "rid-s", time.Time{})
	opc := "opc_s"

	if _, exists, err := r.ReadDeferredSuspendedAt(ctx, runID, opc); err != nil || exists {
		t.Fatalf("ReadDeferredSuspendedAt before set = (exists=%v,%v), want (false,nil)", exists, err)
	}
	if err := r.SetDeferredSuspendedAt(ctx, runID, opc, "web/200"); err != nil {
		t.Fatalf("SetDeferredSuspendedAt: %v", err)
	}
	stage, exists, err := r.ReadDeferredSuspendedAt(ctx, runID, opc)
	if err != nil || !exists || stage != "web/200" {
		t.Fatalf("ReadDeferredSuspendedAt = (%q,%v,%v), want (web/200,true,nil)", stage, exists, err)
	}
	// Idempotent re-set (immutable; first wins).
	if err := r.SetDeferredSuspendedAt(ctx, runID, opc, "web/999"); err != nil {
		t.Fatalf("SetDeferredSuspendedAt (re-entry): %v", err)
	}
	stage, _, _ = r.ReadDeferredSuspendedAt(ctx, runID, opc)
	if stage != "web/200" {
		t.Fatalf("suspended-at after re-set = %q, want web/200 (immutable)", stage)
	}
}

func TestRecordDeferredTerminalBadStatus(t *testing.T) {
	ctx := context.Background()
	r := newRuns(t)
	runID, _, _ := r.CreateRun(ctx, "tnt", "web", "", "web/50", "rid-3", time.Time{})
	if _, err := r.RecordDeferredTerminal(ctx, runID, "opc_z", "weird", []byte(`{}`)); err == nil {
		t.Fatal("RecordDeferredTerminal with bad status: err = nil, want error")
	}
}
