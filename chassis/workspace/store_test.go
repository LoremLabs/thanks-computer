package workspace

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "rt.db")+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewStore(db, nil)
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return base }

	if r, err := s.Get(ctx, "acme", "agents", "tools"); err != nil || r != nil {
		t.Fatalf("Get on empty = %+v, %v", r, err)
	}
	if err := s.Upsert(ctx, Row{Tenant: "acme", Stack: "agents", Name: "tools", Provider: "local", ProviderRef: "/w/acme/agents/tools"}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, "acme", "agents", "tools")
	if err != nil || r == nil {
		t.Fatalf("Get = %+v, %v", r, err)
	}
	if r.ID != ID("acme", "agents", "tools") || r.Status != StatusCreated || r.Network != "public" || !r.CreatedAt.Equal(base) || !r.LastUsedAt.Equal(base) || r.DestroyedAt != nil {
		t.Errorf("row = %+v", r)
	}

	later := base.Add(time.Hour)
	if err := s.Touch(ctx, r.ID, later, "run-1", ""); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(ctx, "acme", "agents", "tools")
	if r.RunID != "run-1" || r.Status != StatusRunning || !r.LastUsedAt.Equal(later) {
		t.Errorf("after touch = %+v", r)
	}
	if err := s.SetCheckpoint(ctx, r.ID, "v2"); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(ctx, "acme", "agents", "tools")
	if r.CheckpointRef != "v2" {
		t.Errorf("checkpoint_ref = %q", r.CheckpointRef)
	}

	// Destroy keeps the row, flips status, clears the run.
	if err := s.MarkDestroyed(ctx, r.ID, later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	r, _ = s.Get(ctx, "acme", "agents", "tools")
	if r.Status != StatusDestroyed || r.DestroyedAt == nil || r.RunID != "" {
		t.Errorf("after destroy = %+v", r)
	}

	// Recreate in place: same row id, status reset, destroyed_at cleared,
	// the fresh environment's empty run/checkpoint written.
	if err := s.Upsert(ctx, Row{Tenant: "acme", Stack: "agents", Name: "tools", Provider: "sprites", ProviderRef: "acme-agents-tools-abcd1234"}); err != nil {
		t.Fatal(err)
	}
	r2, _ := s.Get(ctx, "acme", "agents", "tools")
	if r2.ID != r.ID || r2.Status != StatusCreated || r2.DestroyedAt != nil || r2.Provider != "sprites" || r2.CheckpointRef != "" || !r2.CreatedAt.Equal(base) {
		t.Errorf("after recreate = %+v", r2)
	}
}

func TestStoreListIdleAndList(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	seed := func(tenant, name string, lastUsed time.Time, destroyed bool) {
		t.Helper()
		if err := s.Upsert(ctx, Row{Tenant: tenant, Stack: "s", Name: name, Provider: "local", ProviderRef: "/" + name, LastUsedAt: lastUsed, CreatedAt: lastUsed}); err != nil {
			t.Fatal(err)
		}
		if destroyed {
			if err := s.MarkDestroyed(ctx, ID(tenant, "s", name), lastUsed); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("acme", "old", base.Add(-40*24*time.Hour), false)
	seed("acme", "older", base.Add(-50*24*time.Hour), false)
	seed("acme", "fresh", base.Add(-time.Hour), false)
	seed("acme", "gone", base.Add(-90*24*time.Hour), true)
	seed("beta", "old", base.Add(-31*24*time.Hour), false)

	idle, err := s.ListIdle(ctx, base.Add(-30*24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range idle {
		got = append(got, r.Tenant+"/"+r.Name)
	}
	want := []string{"acme/older", "acme/old", "beta/old"} // oldest first; destroyed and fresh excluded
	if len(got) != len(want) {
		t.Fatalf("ListIdle = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ListIdle = %v, want %v", got, want)
		}
	}
	if idle2, _ := s.ListIdle(ctx, base.Add(-30*24*time.Hour), 1); len(idle2) != 1 || idle2[0].Name != "older" {
		t.Errorf("ListIdle limit: %+v", idle2)
	}

	all, err := s.List(ctx, "acme", false, 0)
	if err != nil || len(all) != 3 || all[0].Name != "fresh" {
		t.Errorf("List(acme) = %+v, %v", all, err)
	}
	withGone, _ := s.List(ctx, "acme", true, 0)
	if len(withGone) != 4 {
		t.Errorf("List(acme, destroyed) = %d rows", len(withGone))
	}
	every, _ := s.List(ctx, "", false, 0)
	if len(every) != 4 {
		t.Errorf("List(all) = %d rows", len(every))
	}
}

func TestStoreUpsertValidation(t *testing.T) {
	s := newTestStore(t)
	for _, r := range []Row{
		{Stack: "s", Name: "n", Provider: "p", ProviderRef: "r"},
		{Tenant: "t", Name: "n", Provider: "p", ProviderRef: "r"},
		{Tenant: "t", Stack: "s", Provider: "p", ProviderRef: "r"},
		{Tenant: "t", Stack: "s", Name: "n", ProviderRef: "r"},
		{Tenant: "t", Stack: "s", Name: "n", Provider: "p"},
	} {
		if err := s.Upsert(context.Background(), r); err == nil {
			t.Errorf("Upsert(%+v) accepted", r)
		}
	}
}
