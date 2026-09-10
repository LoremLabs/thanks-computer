package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	s := NewStore(db, registry.SQLite)
	s.now = func() time.Time { return clk }
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return s, &clk
}

func decl(tenant, stack, pack, id string, enabled bool, every int) Declared {
	cfg, _ := json.Marshal(map[string]any{
		"id": id, "kind": "imap", "host": "imap.example.com",
		"user": "desk@example.com", "secret": "MBOX", "on_processed": "move:Done",
	})
	return Declared{
		Tenant: tenant, Stack: stack, Pack: pack, DeclaredID: id,
		Kind: "imap", Config: cfg, Enabled: enabled, EverySeconds: every, Version: 1,
	}
}

func TestUpsertClaimRelease(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	if err := s.Upsert(ctx, decl("acme", "desk", "boxes", "support", true, 300)); err != nil {
		t.Fatal(err)
	}

	won, err := s.ClaimDue(ctx, "node-a", 10)
	if err != nil || len(won) != 1 {
		t.Fatalf("claim: won=%d err=%v", len(won), err)
	}
	c := won[0]
	if c.Tenant != "acme" || c.Kind != "imap" || c.EverySeconds != 300 {
		t.Fatalf("claimed row wrong: %+v", c)
	}
	if c.Cursor != nil {
		t.Errorf("fresh source should have nil cursor, got %q", c.Cursor)
	}
	// A second claim while it's still claimed wins nothing.
	if again, _ := s.ClaimDue(ctx, "node-b", 10); len(again) != 0 {
		t.Fatalf("second claim won %d, want 0 (still claimed)", len(again))
	}

	// Release with an advanced cursor reschedules 300s out and clears the claim.
	cur := Cursor(`{"uidvalidity":1,"uid_hwm":42}`)
	if err := s.Release(ctx, c.SourceID, cur, 300, nil); err != nil {
		t.Fatal(err)
	}
	// Not due yet (next_poll_at = now+300).
	if due, _ := s.ClaimDue(ctx, "node-a", 10); len(due) != 0 {
		t.Fatalf("claimed before next_poll_at, won %d", len(due))
	}
	// Advance past the interval → due again, and the cursor survived.
	*clk = clk.Add(301 * time.Second)
	due, _ := s.ClaimDue(ctx, "node-a", 10)
	if len(due) != 1 {
		t.Fatalf("not due after interval: won %d", len(due))
	}
	if string(due[0].Cursor) != string(cur) {
		t.Errorf("cursor not preserved across release/claim: %q", due[0].Cursor)
	}
}

func TestUpsertNeverRewindsCursor(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	d := decl("acme", "desk", "boxes", "support", true, 300)
	if err := s.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	won, _ := s.ClaimDue(ctx, "n", 10)
	cur := Cursor(`{"uidvalidity":7,"uid_hwm":99}`)
	if err := s.Release(ctx, won[0].SourceID, cur, 300, nil); err != nil {
		t.Fatal(err)
	}
	// Redeploy: same declared source, changed interval — the Upsert must touch
	// only declared columns, never the cursor or claim state.
	d.EverySeconds = 600
	if err := s.Upsert(ctx, d); err != nil {
		t.Fatal(err)
	}
	var storedCursor, status string
	row := s.db.QueryRow(`SELECT cursor, status FROM tenant_sources WHERE source_id=?`,
		SourceID("acme", "desk", "boxes", "support"))
	if err := row.Scan(&storedCursor, &status); err != nil {
		t.Fatal(err)
	}
	if storedCursor != string(cur) {
		t.Errorf("redeploy rewound the cursor: %q", storedCursor)
	}
	if status != "idle" {
		t.Errorf("redeploy disturbed claim status: %q", status)
	}
}

func TestRetireMissingSoftKeepsCursor(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Upsert(ctx, decl("acme", "desk", "boxes", id, true, 300)); err != nil {
			t.Fatal(err)
		}
	}
	// Give "a" a cursor.
	won, _ := s.ClaimDue(ctx, "n", 10)
	var aID string
	for _, w := range won {
		if w.DeclaredID == "a" {
			aID = w.SourceID
		}
		_ = s.Release(ctx, w.SourceID, nil, 300, nil)
	}
	_ = s.Release(ctx, aID, Cursor(`{"uidvalidity":1,"uid_hwm":5}`), 300, nil)

	// New version keeps only "a"; "b" and "c" are dropped from the pack.
	n, err := s.RetireMissing(ctx, "acme", "desk", "boxes", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("retired %d, want 2 (b and c)", n)
	}
	// "a" is live with its cursor; "b"/"c" are retired (row kept) and not due.
	sts, _ := s.List(ctx, "acme")
	byID := map[string]Status{}
	for _, st := range sts {
		byID[st.DeclaredID] = st
	}
	if byID["a"].Retired || byID["a"].Cursor == "" {
		t.Errorf("a should be live with a cursor: %+v", byID["a"])
	}
	if !byID["b"].Retired || !byID["c"].Retired {
		t.Errorf("b/c should be retired: %+v %+v", byID["b"], byID["c"])
	}
	// A later re-add of "b" un-retires it AND keeps its (empty) row — resuming.
	if err := s.Upsert(ctx, decl("acme", "desk", "boxes", "b", true, 300)); err != nil {
		t.Fatal(err)
	}
	sts, _ = s.List(ctx, "acme")
	for _, st := range sts {
		if st.DeclaredID == "b" && st.Retired {
			t.Errorf("re-adding b should have un-retired it: %+v", st)
		}
	}
}

func TestClaimDuePartitionsAndRespectsFilters(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := s.Upsert(ctx, decl("acme", "desk", "boxes", id, true, 300)); err != nil {
			t.Fatal(err)
		}
	}
	// A disabled source and a retired one must never be claimed.
	if err := s.Upsert(ctx, decl("acme", "desk", "boxes", "off", false, 300)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireMissing(ctx, "acme", "desk", "boxes", []string{"a", "b", "c", "d", "off"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, decl("acme", "desk", "boxes", "gone", true, 300)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RetireMissing(ctx, "acme", "desk", "boxes", []string{"a", "b", "c", "d", "off"}); err != nil {
		t.Fatal(err) // retires "gone"
	}

	// Two pollers, limit 2 each: disjoint union covering exactly the 4 live.
	first, _ := s.ClaimDue(ctx, "node-1", 2)
	second, _ := s.ClaimDue(ctx, "node-2", 2)
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("partition sizes: %d, %d (want 2,2)", len(first), len(second))
	}
	seen := map[string]bool{}
	for _, c := range append(first, second...) {
		if seen[c.DeclaredID] {
			t.Errorf("source %q claimed twice", c.DeclaredID)
		}
		seen[c.DeclaredID] = true
		if c.DeclaredID == "off" || c.DeclaredID == "gone" {
			t.Errorf("claimed a disabled/retired source: %q", c.DeclaredID)
		}
	}
	if len(seen) != 4 {
		t.Fatalf("claimed %d distinct, want 4", len(seen))
	}
}

func TestReclaimStale(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	if err := s.Upsert(ctx, decl("acme", "desk", "boxes", "a", true, 300)); err != nil {
		t.Fatal(err)
	}
	won, _ := s.ClaimDue(ctx, "dead-node", 10)
	if len(won) != 1 {
		t.Fatal("claim failed")
	}
	// Not stale yet.
	if n, _ := s.ReclaimStale(ctx, 600*time.Second); n != 0 {
		t.Fatalf("reclaimed %d before stale window", n)
	}
	*clk = clk.Add(601 * time.Second)
	if n, _ := s.ReclaimStale(ctx, 600*time.Second); n != 1 {
		t.Fatalf("reclaimed %d, want 1", n)
	}
	// Reclaimed → idle and immediately due again (next_poll_at untouched).
	if due, _ := s.ClaimDue(ctx, "live-node", 10); len(due) != 1 {
		t.Fatalf("reclaimed source not due: won %d", len(due))
	}
}

func TestReleaseRecordsError(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	if err := s.Upsert(ctx, decl("acme", "desk", "boxes", "a", true, 300)); err != nil {
		t.Fatal(err)
	}
	won, _ := s.ClaimDue(ctx, "n", 10)
	if err := s.Release(ctx, won[0].SourceID, nil, 300, errors.New("boom: login failed")); err != nil {
		t.Fatal(err)
	}
	sts, _ := s.List(ctx, "acme")
	if len(sts) != 1 || sts[0].LastError == "" {
		t.Fatalf("last_error not recorded: %+v", sts)
	}
}
