package scheduled

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// newTestStore opens a per-test temp-file SQLite DB (cgo sqlite + :memory:
// shared-cache is finicky across connections; a temp file works everywhere),
// applies the embedded scheduled schema, and returns a Store whose clock is
// driven by *clk so tests can move time deterministically.
func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scheduled.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	clk := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	s := NewStore(db, registry.SQLite)
	s.now = func() time.Time { return clk }
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return s, &clk
}

func countStatus(t *testing.T, s *Store, status string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scheduled_events WHERE status = ?`, status).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", status, err)
	}
	return n
}

func TestEnqueueClaimAndDone(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	due := clk.Add(-time.Minute) // already due

	if _, err := s.Enqueue(ctx, "acme", "drip:1", due, json.RawMessage(`{"kind":"drip"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	won, err := s.ClaimDue(ctx, "node-a", 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(won) != 1 {
		t.Fatalf("want 1 claimed, got %d", len(won))
	}
	if won[0].Tenant != "acme" || won[0].IdempotencyKey != "drip:1" {
		t.Fatalf("unexpected claimed row: %+v", won[0])
	}
	if string(won[0].Payload) != `{"kind":"drip"}` {
		t.Fatalf("payload not preserved: %s", won[0].Payload)
	}

	// A second poll must not re-claim the now-'claimed' row.
	again, err := s.ClaimDue(ctx, "node-b", 10)
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("claimed row re-claimed: got %d", len(again))
	}

	if err := s.MarkDone(ctx, won[0].ID); err != nil {
		t.Fatalf("mark done: %v", err)
	}
	if got := countStatus(t, s, "done"); got != 1 {
		t.Fatalf("want 1 done, got %d", got)
	}
}

func TestClaimRespectsScheduleAt(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Enqueue(ctx, "acme", "future", clk.Add(time.Hour), nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	won, err := s.ClaimDue(ctx, "node-a", 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(won) != 0 {
		t.Fatalf("not-yet-due row claimed: got %d", len(won))
	}

	// Move time past schedule_at → now due.
	*clk = clk.Add(2 * time.Hour)
	won, err = s.ClaimDue(ctx, "node-a", 10)
	if err != nil {
		t.Fatalf("claim after due: %v", err)
	}
	if len(won) != 1 {
		t.Fatalf("want 1 due after time advance, got %d", len(won))
	}
}

func TestRescheduleWhilePending(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Enqueue(ctx, "acme", "rem:1", clk.Add(time.Hour), nil); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	// Re-enqueue the same key, further out + new payload: reschedules in place.
	if _, err := s.Enqueue(ctx, "acme", "rem:1", clk.Add(3*time.Hour), json.RawMessage(`{"v":2}`)); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if got := countStatus(t, s, "pending"); got != 1 {
		t.Fatalf("reschedule duplicated the row: %d pending", got)
	}

	// Still not due at +2h (original would have been due at +1h).
	*clk = clk.Add(2 * time.Hour)
	if won, _ := s.ClaimDue(ctx, "n", 10); len(won) != 0 {
		t.Fatalf("rescheduled row fired early: got %d", len(won))
	}
	// Due past the new time.
	*clk = clk.Add(2 * time.Hour)
	won, _ := s.ClaimDue(ctx, "n", 10)
	if len(won) != 1 || string(won[0].Payload) != `{"v":2}` {
		t.Fatalf("reschedule didn't take: %+v", won)
	}
}

func TestSpentKeyNotResurrected(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	due := clk.Add(-time.Minute)

	if _, err := s.Enqueue(ctx, "acme", "once", due, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	won, _ := s.ClaimDue(ctx, "n", 10)
	if len(won) != 1 {
		t.Fatalf("setup claim: %d", len(won))
	}
	if err := s.MarkDone(ctx, won[0].ID); err != nil {
		t.Fatalf("done: %v", err)
	}
	// Re-enqueue the spent key: ON CONFLICT WHERE status='pending' no-ops.
	if _, err := s.Enqueue(ctx, "acme", "once", due, nil); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if got := countStatus(t, s, "done"); got != 1 {
		t.Fatalf("spent key was resurrected: done=%d", got)
	}
	if won, _ := s.ClaimDue(ctx, "n", 10); len(won) != 0 {
		t.Fatalf("spent key re-fired: %d", len(won))
	}
}

func TestCancel(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()

	if _, err := s.Enqueue(ctx, "acme", "rem", clk.Add(time.Hour), nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ok, err := s.Cancel(ctx, "acme", "rem")
	if err != nil || !ok {
		t.Fatalf("cancel pending: ok=%v err=%v", ok, err)
	}
	if got := countStatus(t, s, "pending"); got != 0 {
		t.Fatalf("cancel left a pending row: %d", got)
	}
	// Cancelling again is a no-op (false, no error).
	ok, err = s.Cancel(ctx, "acme", "rem")
	if err != nil || ok {
		t.Fatalf("cancel of absent: ok=%v err=%v", ok, err)
	}
}

func TestReclaimStale(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	due := clk.Add(-time.Minute)

	if _, err := s.Enqueue(ctx, "acme", "stuck", due, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	won, _ := s.ClaimDue(ctx, "dead-node", 10)
	if len(won) != 1 {
		t.Fatalf("setup claim: %d", len(won))
	}

	// Not stale yet: claimed_at == now, staleAfter 10m.
	if n, err := s.ReclaimStale(ctx, 10*time.Minute); err != nil || n != 0 {
		t.Fatalf("premature reclaim: n=%d err=%v", n, err)
	}

	// Advance past staleAfter → the abandoned claim is reset to pending.
	*clk = clk.Add(20 * time.Minute)
	n, err := s.ReclaimStale(ctx, 10*time.Minute)
	if err != nil || n != 1 {
		t.Fatalf("reclaim: n=%d err=%v", n, err)
	}
	again, _ := s.ClaimDue(ctx, "live-node", 10)
	if len(again) != 1 {
		t.Fatalf("reclaimed row not re-claimable: %d", len(again))
	}
}

func TestPurge(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	due := clk.Add(-time.Minute)

	if _, err := s.Enqueue(ctx, "acme", "old", due, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	won, _ := s.ClaimDue(ctx, "n", 10)
	if err := s.MarkDone(ctx, won[0].ID); err != nil {
		t.Fatalf("done: %v", err)
	}

	// Nothing old enough yet.
	if n, _ := s.Purge(ctx, 7*24*time.Hour); n != 0 {
		t.Fatalf("premature purge: %d", n)
	}
	// Age past retention.
	*clk = clk.Add(8 * 24 * time.Hour)
	n, err := s.Purge(ctx, 7*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	if got := countStatus(t, s, "done"); got != 0 {
		t.Fatalf("purge left a done row: %d", got)
	}
}
