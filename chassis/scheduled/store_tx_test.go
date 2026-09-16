package scheduled

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEnqueueTxRollsBackWithCaller(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := NewEventID()
	if !strings.HasPrefix(id, "sched_") {
		t.Fatalf("id %q", id)
	}
	if err := s.EnqueueTx(ctx, tx, id, "t1", "k1", *clk, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := countStatus(t, s, "pending"); n != 0 {
		t.Fatalf("rolled-back enqueue left %d rows", n)
	}

	tx, _ = s.db.BeginTx(ctx, nil)
	if err := s.EnqueueTx(ctx, tx, id, "t1", "k1", *clk, json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatalf("EnqueueTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := countStatus(t, s, "pending"); n != 1 {
		t.Fatalf("committed enqueue: %d rows", n)
	}
	// Same validation as Enqueue.
	tx, _ = s.db.BeginTx(ctx, nil)
	defer tx.Rollback()
	if err := s.EnqueueTx(ctx, tx, "", "t1", "k2", *clk, nil); err == nil {
		t.Error("empty id accepted")
	}
	if err := s.EnqueueTx(ctx, tx, NewEventID(), "", "k2", *clk, nil); err == nil {
		t.Error("empty tenant accepted")
	}
	if err := s.EnqueueTx(ctx, tx, NewEventID(), "t1", "k2", *clk, json.RawMessage(`{`)); err == nil {
		t.Error("invalid JSON accepted")
	}
	if err := s.EnqueueTx(ctx, tx, NewEventID(), "t1", strings.Repeat("k", 513), *clk, nil); err == nil {
		t.Error("oversized key accepted")
	}
	// Enqueue still mints its own id and lands a row.
	got, err := s.Enqueue(ctx, "t1", "k3", clk.Add(time.Minute), nil)
	if err != nil || !strings.HasPrefix(got, "sched_") {
		t.Fatalf("Enqueue: %q %v", got, err)
	}
}
