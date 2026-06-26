package vecseed_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/vecseed"
	"github.com/loremlabs/thanks-computer/chassis/vector"
	"github.com/loremlabs/thanks-computer/chassis/vector/sqlitevec"
)

func newStore(t *testing.T) vector.Store {
	t.Helper()
	s, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("open sqlitevec: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func pack(name string, lines ...string) storeseed.RawPack {
	body := []byte{}
	for i, ln := range lines {
		if i > 0 {
			body = append(body, '\n')
		}
		body = append(body, []byte(ln)...)
	}
	p, _ := storeseed.NewRawPack("VECTORS/"+name+".jsonl", body)
	return p
}

func idsIn(t *testing.T, s vector.Store, tenant, coll string) []string {
	t.Helper()
	ids, err := s.ListIDs(context.Background(), tenant, coll)
	if err != nil {
		t.Fatalf("ListIDs: %v", err)
	}
	return ids
}

func has(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestReconcileUpsertDeleteMissing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	m := vecseed.New(s, false)
	scope := storeseed.Scope{Tenant: "acme", Stack: "recs", Version: 1}

	// First apply: 3 books, model pinned via per-item "model".
	p1 := pack("books",
		`{"id":"pooh","vector":[1,0,0],"metadata":{"pd":true},"text":"a bear","model":"text-embedding-3-small"}`,
		`{"id":"alice","vector":[0,1,0],"metadata":{"pd":true},"text":"a girl","model":"text-embedding-3-small"}`,
		`{"id":"moby","vector":[0,0,1],"metadata":{"pd":false},"text":"a whale","model":"text-embedding-3-small"}`,
	)
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{p1}); err != nil {
		t.Fatalf("reconcile v1: %v", err)
	}
	// Collection pinned: dims=3 derived, model from the pack.
	coll, found, err := s.DescribeCollection(ctx, "acme", "books")
	if err != nil || !found {
		t.Fatalf("describe: found=%v err=%v", found, err)
	}
	if coll.Dimensions != 3 || coll.EmbeddingModel != "text-embedding-3-small" {
		t.Fatalf("pin wrong: dims=%d model=%q", coll.Dimensions, coll.EmbeddingModel)
	}
	if got := idsIn(t, s, "acme", "books"); len(got) != 3 {
		t.Fatalf("after v1: %v want 3 items", got)
	}

	// Second apply: drop moby, keep pooh+alice, add wind. Desired-state sync:
	// moby must be deleted (delete-missing), wind added.
	p2 := pack("books",
		`{"id":"pooh","vector":[1,0,0],"metadata":{"pd":true}}`,
		`{"id":"alice","vector":[0,1,0],"metadata":{"pd":true}}`,
		`{"id":"wind","vector":[0.5,0.5,0],"metadata":{"pd":true}}`,
	)
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{p2}); err != nil {
		t.Fatalf("reconcile v2: %v", err)
	}
	got := idsIn(t, s, "acme", "books")
	if len(got) != 3 || has(got, "moby") || !has(got, "wind") {
		t.Fatalf("after v2: %v want [pooh alice wind] (moby dropped)", got)
	}

	// Third apply: empty pack → existing collection emptied, not dropped.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack("books")}); err != nil {
		t.Fatalf("reconcile empty: %v", err)
	}
	if got := idsIn(t, s, "acme", "books"); len(got) != 0 {
		t.Fatalf("after empty pack: %v want 0", got)
	}
	if _, found, _ := s.DescribeCollection(ctx, "acme", "books"); !found {
		t.Fatal("empty pack should empty, not drop, the collection")
	}
}

func TestReconcileMalformedPacks(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	m := vecseed.New(s, false)
	scope := storeseed.Scope{Tenant: "acme", Version: 1}

	bad := map[string]storeseed.RawPack{
		"dim mismatch": pack("c",
			`{"id":"a","vector":[1,0,0]}`,
			`{"id":"b","vector":[1,0]}`),
		"duplicate id": pack("c",
			`{"id":"a","vector":[1,0,0]}`,
			`{"id":"a","vector":[0,1,0]}`),
		"missing id":   pack("c", `{"vector":[1,0,0]}`),
		"empty vector": pack("c", `{"id":"a","vector":[]}`),
		"bad json":     pack("c", `{"id":"a","vector":[1,0,0]`),
	}
	for name, p := range bad {
		if err := m.Reconcile(ctx, scope, []storeseed.RawPack{p}); err == nil {
			t.Errorf("%s: reconcile = nil, want error", name)
		}
	}
}

func TestSharedFlag(t *testing.T) {
	if vecseed.New(newStore(t), true).Shared() != true {
		t.Error("Shared()=false, want true")
	}
	if vecseed.New(newStore(t), false).Shared() != false {
		t.Error("Shared()=true, want false")
	}
	if k := vecseed.New(newStore(t), false).Kind(); k != storeseed.KindVector {
		t.Errorf("Kind()=%q, want %q", k, storeseed.KindVector)
	}
}
