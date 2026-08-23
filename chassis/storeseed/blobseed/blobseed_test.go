package blobseed_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/blobseed"
)

func newIndex(t *testing.T) blob.Index {
	t.Helper()
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &boltdb.Config{Bucket: "txco"})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return blob.NewKVIndex(kvstore.New(s, 0, 0))
}

func newCAS(t *testing.T) filecas.Store {
	t.Helper()
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// put stores data in the CAS and returns its hash — what the CLI's blob-plane
// upload does before the draft references the row.
func put(t *testing.T, cas filecas.Store, data string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(data))
	h := hex.EncodeToString(sum[:])
	if err := cas.Put(context.Background(), h, []byte(data)); err != nil {
		t.Fatal(err)
	}
	return h
}

func row(path, hash string) storeseed.RawPack {
	p, ok := storeseed.NewRawPack(path, nil)
	if !ok {
		panic("bad pack path " + path)
	}
	p.Hash = hash
	return p
}

var scope = storeseed.Scope{Tenant: "acme", Stack: "demo", Version: 1}

func TestReconcileSeedReplaceDeleteMissing(t *testing.T) {
	ctx := context.Background()
	ix, cas := newIndex(t), newCAS(t)
	m := blobseed.New(ix, cas, false)

	h1 := put(t, cas, "house manual v1")
	h2 := put(t, cas, "bookings,2026")
	// A runtime-written name in the same tenant — never touched by the pack.
	if err := ix.PutName(ctx, "acme", blob.NameRow{Name: "docs/runtime.txt", SHA256: h1, Size: 3,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// v1: seed two names.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{
		row("BLOBS/faqs/house-01.txt", h1), row("BLOBS/bookings/2026.csv", h2),
	}); err != nil {
		t.Fatalf("v1: %v", err)
	}
	a, found, _ := ix.GetName(ctx, "acme", "faqs/house-01.txt")
	if !found || a.SHA256 != h1 || a.Size != int64(len("house manual v1")) || a.SeededBy != "demo" ||
		a.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("seeded row: found=%v %+v", found, a)
	}
	if s, found, _ := ix.GetSha(ctx, "acme", h1); !found || s.Size != a.Size {
		t.Fatalf("sha ownership row: found=%v %+v", found, s)
	}

	// v2: house-01 edited (new bytes), bookings dropped.
	h1b := put(t, cas, "house manual v2 — longer")
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{row("BLOBS/faqs/house-01.txt", h1b)}); err != nil {
		t.Fatalf("v2: %v", err)
	}
	a2, _, _ := ix.GetName(ctx, "acme", "faqs/house-01.txt")
	if a2.SHA256 != h1b || a2.Size != int64(len("house manual v2 — longer")) || !a2.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("replace: %+v (created_at must survive)", a2)
	}
	if _, found, _ := ix.GetSha(ctx, "acme", h1); !found {
		t.Fatal("old sha ownership row must stay (CAS keeps history)")
	}
	if _, found, _ := ix.GetName(ctx, "acme", "bookings/2026.csv"); found {
		t.Fatal("dropped seeded name must be deleted")
	}
	if _, found, _ := ix.GetName(ctx, "acme", "docs/runtime.txt"); !found {
		t.Fatal("runtime-owned name was deleted by the pack")
	}

	// v3: tree removed entirely → EmptyTree marker deletes the stack's seeded
	// names, keeps runtime ones and another stack's.
	if err := ix.PutName(ctx, "acme", blob.NameRow{Name: "other/x.txt", SHA256: h2, SeededBy: "otherstack",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{storeseed.EmptyTree(storeseed.KindBlob)}); err != nil {
		t.Fatalf("v3: %v", err)
	}
	if _, found, _ := ix.GetName(ctx, "acme", "faqs/house-01.txt"); found {
		t.Fatal("EmptyTree must delete the stack's seeded names")
	}
	for _, keep := range []string{"docs/runtime.txt", "other/x.txt"} {
		if _, found, _ := ix.GetName(ctx, "acme", keep); !found {
			t.Fatalf("%s must survive EmptyTree", keep)
		}
	}
}

// TestReconcileDriftRules — the git model. A runtime put repoints a seeded
// name (drift); an unforced reconcile leaves it alone while the tree's file is
// unchanged, re-asserts it when the tree's file changes, keeps it when the
// tree drops the file, and --force makes the tree win in every case.
func TestReconcileDriftRules(t *testing.T) {
	ctx := context.Background()
	ix, cas := newIndex(t), newCAS(t)
	m := blobseed.New(ix, cas, false)
	hSeed := put(t, cas, "hello")
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{row("BLOBS/hello.txt", hSeed)}); err != nil {
		t.Fatal(err)
	}
	seeded, _, _ := ix.GetName(ctx, "acme", "hello.txt")
	if seeded.SeededBy != "demo" || seeded.SeededSHA != hSeed {
		t.Fatalf("bookkeeping: %+v", seeded)
	}

	// Runtime put → "hola": exactly what blob/put does — sha moves, the pack
	// bookkeeping stays, permissions get attached.
	hRun := put(t, cas, "hola")
	drift := seeded
	drift.SHA256, drift.Size = hRun, 4
	drift.Permissions = []string{"blob:internal:read"}
	if err := ix.PutName(ctx, "acme", drift); err != nil {
		t.Fatal(err)
	}

	// 1. Tree unchanged (same hash shipped again, e.g. an unrelated file
	//    changed and fanned out) → the runtime edit STANDS.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{row("BLOBS/hello.txt", hSeed)}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := ix.GetName(ctx, "acme", "hello.txt"); r.SHA256 != hRun || !r.Drifted() {
		t.Fatalf("unchanged tree clobbered runtime drift: %+v", r)
	}

	// 2. Tree dropped the file → a drifted document survives the drop.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{storeseed.EmptyTree(storeseed.KindBlob)}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := ix.GetName(ctx, "acme", "hello.txt"); !found {
		t.Fatal("drifted name deleted by an unforced tree drop")
	}

	// 3. Tree CHANGED the file → the tree's new content wins, bookkeeping
	//    moves, runtime permissions preserved.
	hV2 := put(t, cas, "hello v2")
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{row("BLOBS/hello.txt", hV2)}); err != nil {
		t.Fatal(err)
	}
	r, _, _ := ix.GetName(ctx, "acme", "hello.txt")
	if r.SHA256 != hV2 || r.SeededSHA != hV2 || r.Drifted() || len(r.Permissions) != 1 {
		t.Fatalf("changed tree must re-assert: %+v", r)
	}

	// 4. Drift again, then --force with the UNCHANGED tree → the tree wins.
	drift = r
	drift.SHA256 = hRun
	if err := ix.PutName(ctx, "acme", drift); err != nil {
		t.Fatal(err)
	}
	forced := scope
	forced.Force = true
	if err := m.Reconcile(ctx, forced, []storeseed.RawPack{row("BLOBS/hello.txt", hV2)}); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := ix.GetName(ctx, "acme", "hello.txt"); r.SHA256 != hV2 || r.Drifted() {
		t.Fatalf("--force must re-assert over drift: %+v", r)
	}
	// 5. Drift, then --force with the tree DROPPING the file → unlinked.
	drift.SHA256 = hRun
	if err := ix.PutName(ctx, "acme", drift); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(ctx, forced, []storeseed.RawPack{storeseed.EmptyTree(storeseed.KindBlob)}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := ix.GetName(ctx, "acme", "hello.txt"); found {
		t.Fatal("--force drop left the drifted name")
	}
	// A purely runtime name (never seeded) is never touched, forced or not.
	if err := ix.PutName(ctx, "acme", blob.NameRow{Name: "docs/mine.txt", SHA256: hRun, CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(ctx, forced, []storeseed.RawPack{storeseed.EmptyTree(storeseed.KindBlob)}); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := ix.GetName(ctx, "acme", "docs/mine.txt"); !found {
		t.Fatal("runtime-only name deleted")
	}
}

func TestReconcileErrors(t *testing.T) {
	ctx := context.Background()
	ix, cas := newIndex(t), newCAS(t)
	m := blobseed.New(ix, cas, false)
	// Bytes not resident → error, nothing seeded.
	missing := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{row("BLOBS/faqs/a.txt", missing)}); err == nil {
		t.Fatal("non-resident bytes seeded")
	}
	if _, found, _ := ix.GetName(ctx, "acme", "faqs/a.txt"); found {
		t.Fatal("row written despite missing bytes")
	}
	// Bad name, missing hash.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{row("BLOBS/_reserved/a.txt", put(t, cas, "x"))}); err == nil {
		t.Fatal("reserved name seeded")
	}
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{row("BLOBS/faqs/a.txt", "")}); err == nil {
		t.Fatal("row without hash seeded")
	}
	// No filecas on this node.
	if err := blobseed.New(ix, nil, false).Reconcile(ctx, scope,
		[]storeseed.RawPack{row("BLOBS/faqs/a.txt", put(t, cas, "y"))}); err == nil {
		t.Fatal("seeded without a filecas")
	}
	// Flags.
	if m.Kind() != storeseed.KindBlob || m.Shared() {
		t.Fatal("flags")
	}
	if !blobseed.New(ix, cas, true).Shared() {
		t.Fatal("shared flag")
	}
}
