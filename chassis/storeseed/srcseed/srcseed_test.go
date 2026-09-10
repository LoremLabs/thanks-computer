package srcseed

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/source"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

func newStore(t *testing.T) *source.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := source.NewStore(db, registry.SQLite)
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return s
}

func pack(name string, lines ...string) storeseed.RawPack {
	var body []byte
	for i, ln := range lines {
		if i > 0 {
			body = append(body, '\n')
		}
		body = append(body, []byte(ln)...)
	}
	p, ok := storeseed.NewRawPack("SOURCES/"+name+".jsonl", body)
	if !ok {
		panic("bad pack path")
	}
	return p
}

func line(id, every string, enabled *bool) string {
	m := map[string]any{
		"id": id, "kind": "imap", "host": "imap.example.com",
		"user": "d@example.com", "secret": "MBOX", "on_processed": "move:Done",
	}
	if every != "" {
		var v any
		_ = json.Unmarshal([]byte(every), &v)
		m["every"] = v
	}
	if enabled != nil {
		m["enabled"] = *enabled
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func statusByID(t *testing.T, s *source.Store, tenant string) map[string]source.Status {
	t.Helper()
	sts, err := s.List(context.Background(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]source.Status{}
	for _, st := range sts {
		out[st.DeclaredID] = st
	}
	return out
}

func TestReconcileUpsertsAndSoftRetires(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	m := New(s)
	scope := storeseed.Scope{Tenant: "acme", Stack: "desk", Version: 1}

	// First version: two sources.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{
		pack("boxes", line("support", `"5m"`, nil), line("billing", `300`, nil)),
	}); err != nil {
		t.Fatal(err)
	}
	st := statusByID(t, s, "acme")
	if len(st) != 2 || st["support"].Retired || st["billing"].Retired {
		t.Fatalf("after v1: %+v", st)
	}

	// Give "support" a cursor (as the poller would).
	won, _ := s.ClaimDue(ctx, "n", 10)
	for _, w := range won {
		cur := source.Cursor(``)
		if w.DeclaredID == "support" {
			cur = source.Cursor(`{"uidvalidity":3,"uid_hwm":88}`)
		}
		if len(cur) == 0 {
			cur = nil
		}
		_ = s.Release(ctx, w.SourceID, cur, 300, nil)
	}

	// Second version: "billing" dropped from the pack.
	scope.Version = 2
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{
		pack("boxes", line("support", `"10m"`, nil)),
	}); err != nil {
		t.Fatal(err)
	}
	st = statusByID(t, s, "acme")
	if st["billing"].Retired != true {
		t.Errorf("billing should be soft-retired, got %+v", st["billing"])
	}
	if st["support"].Retired {
		t.Errorf("support should stay live: %+v", st["support"])
	}
	// The reconcile (a declared-column write) must NOT have touched the cursor.
	if st["support"].Cursor != `{"uidvalidity":3,"uid_hwm":88}` {
		t.Errorf("reconcile disturbed the cursor: %q", st["support"].Cursor)
	}
}

func TestReconcileEmptyPackRetiresAll(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	m := New(s)
	scope := storeseed.Scope{Tenant: "acme", Stack: "desk", Version: 1}
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{
		pack("boxes", line("a", "", nil), line("b", "", nil)),
	}); err != nil {
		t.Fatal(err)
	}
	// An emptied pack (present file, no lines) retires everything it owned.
	scope.Version = 2
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack("boxes")}); err != nil {
		t.Fatal(err)
	}
	for id, st := range statusByID(t, s, "acme") {
		if !st.Retired {
			t.Errorf("%q should be retired after empty pack: %+v", id, st)
		}
	}
}

func TestReconcileRejectsBadLines(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	m := New(s)
	scope := storeseed.Scope{Tenant: "acme", Stack: "desk", Version: 1}
	// Missing id.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{
		pack("boxes", `{"kind":"imap","host":"h"}`),
	}); err == nil {
		t.Error("expected error for a line with no id")
	}
	// Duplicate id in one pack.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{
		pack("boxes", line("dup", "", nil), line("dup", "", nil)),
	}); err == nil {
		t.Error("expected error for a duplicate id")
	}
}

func TestParseEvery(t *testing.T) {
	cases := map[string]int{
		`"5m"`:   300,
		`"90s"`:  90,
		`"1h"`:   3600,
		`300`:    300,
		`45`:     45,
		`""`:     300, // default
		`"nope"`: 300,
		`0`:      300, // non-positive → default
		`-5`:     300,
	}
	for raw, want := range cases {
		if got := parseEvery(json.RawMessage(raw)); got != want {
			t.Errorf("parseEvery(%s) = %d, want %d", raw, got, want)
		}
	}
	if got := parseEvery(nil); got != 300 {
		t.Errorf("parseEvery(nil) = %d, want 300", got)
	}
}
