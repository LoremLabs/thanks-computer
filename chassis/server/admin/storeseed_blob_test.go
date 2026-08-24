package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/blobseed"
)

func newBlobIndexForTest(t *testing.T) blob.Index {
	t.Helper()
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &boltdb.Config{Bucket: "txco"})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return blob.NewKVIndex(kvstore.New(s, 0, 0))
}

func seedStackVersions(t *testing.T, c *Controller, stackID, tenant, name string, versions ...int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES (?,?,?,1,'t')`,
		stackID, tenant, name); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	for _, v := range versions {
		if _, err := c.pu.RuntimeDB.ExecContext(ctx,
			`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (?,?,?,'superseded','test','t')`,
			v, stackID, v); err != nil {
			t.Fatalf("seed version %d: %v", v, err)
		}
	}
}

// seedCASRow inserts a fingerprint-only row (content ”, hash set) — the
// shape every BLOBS/ row has after the CLI streamed its bytes to the CAS.
func seedCASRow(t *testing.T, c *Controller, versionID int64, path, hash string) {
	t.Helper()
	if _, err := c.pu.RuntimeDB.ExecContext(context.Background(),
		`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (?,?,'',?)`,
		versionID, path, hash); err != nil {
		t.Fatalf("seed row %s: %v", path, err)
	}
}

// TestChangedPackPathsRemovalsAndBlobFanout — the change-driven diff surfaces
// REMOVED pack paths (not just added/modified), and any BLOBS/ change fans
// out to every current BLOBS/ row (the tree is one logical pack).
func TestChangedPackPathsRemovalsAndBlobFanout(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	seedStackVersions(t, c, "stk", "acme", "demo", 1, 2, 3)
	hA, hB, hCfg := sha256Hex("A"), sha256Hex("B"), sha256Hex(`{"key":"k","value":1}`)
	// v1: two blobs + a KV pack.
	seedCASRow(t, c, 1, "BLOBS/faqs/a.txt", hA)
	seedCASRow(t, c, 1, "BLOBS/faqs/b.txt", hB)
	seedCASRow(t, c, 1, "KV/cfg.jsonl", hCfg)
	// v2: b.txt removed, everything else carried forward unchanged.
	seedCASRow(t, c, 2, "BLOBS/faqs/a.txt", hA)
	seedCASRow(t, c, 2, "KV/cfg.jsonl", hCfg)
	// v3: the whole BLOBS/ tree removed.
	seedCASRow(t, c, 3, "KV/cfg.jsonl", hCfg)

	changed, ok := c.changedPackPaths(ctx, "acme", "demo", 2, 1)
	if !ok {
		t.Fatal("canDiff=false")
	}
	if _, has := changed["BLOBS/faqs/b.txt"]; !has {
		t.Errorf("removed blob row not surfaced: %v", changed)
	}
	if _, has := changed["BLOBS/faqs/a.txt"]; !has {
		t.Errorf("unchanged blob row not fanned out: %v", changed)
	}
	if _, has := changed["KV/cfg.jsonl"]; has {
		t.Errorf("unchanged KV pack wrongly marked changed: %v", changed)
	}
	changed, _ = c.changedPackPaths(ctx, "acme", "demo", 3, 2)
	if _, has := changed["BLOBS/faqs/a.txt"]; !has || len(changed) != 1 {
		t.Errorf("tree removal: %v", changed)
	}
	// A code-only deploy (identical packs) changes nothing.
	seedStackVersions(t, c, "stk2", "acme", "same", 10, 11)
	seedCASRow(t, c, 10, "BLOBS/x.txt", hA)
	seedCASRow(t, c, 11, "BLOBS/x.txt", hA)
	if changed, _ = c.changedPackPaths(ctx, "acme", "same", 11, 10); len(changed) != 0 {
		t.Errorf("identical blob rows marked changed: %v", changed)
	}
}

// TestReconcileStorePacksBlobs — end to end at the admin layer: BLOBS/ rows
// seed the name index (origin), a new version that drops a row unlinks it,
// and a version that drops the tree unlinks everything the stack seeded.
// The controller's own filecas is EMPTY: loadStorePacks must never fetch a
// blob row's bytes (the materializer probes residency through its own
// store, which does hold them).
func TestReconcileStorePacksBlobs(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	c.fcas = &mapStore{m: map[string][]byte{}} // nothing resident here on purpose

	cas, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	put := func(s string) string {
		h := sha256Hex(s)
		if err := cas.Put(ctx, h, []byte(s)); err != nil {
			t.Fatal(err)
		}
		return h
	}
	ix := newBlobIndexForTest(t)
	c.SetStoreReconciler(storeseed.NewReconciler(blobseed.New(ix, cas, false)))

	seedStackVersions(t, c, "stk", "acme", "demo", 1, 2, 3)
	hA, hB := put("house manual"), put("bookings,2026")
	seedCASRow(t, c, 1, "BLOBS/faqs/house-01.txt", hA)
	seedCASRow(t, c, 1, "BLOBS/bookings/2026.csv", hB)
	seedCASRow(t, c, 1, "100/x.txcl", sha256Hex("NOOP")) // ignored
	seedCASRow(t, c, 2, "BLOBS/faqs/house-01.txt", hA)   // bookings dropped
	// v3 has no BLOBS/ rows at all.

	// storeTenantKey falls back to the id when no tenant row exists — the
	// index is keyed "acme" either way here.
	c.ReconcileStorePacks(ctx, "acme", "demo", 1, 0, true, false)
	row, found, _ := ix.GetName(ctx, "acme", "faqs/house-01.txt")
	if !found || row.SHA256 != hA || row.Size != int64(len("house manual")) || row.SeededBy != "demo" {
		t.Fatalf("v1 seed: found=%v %+v", found, row)
	}
	if _, found, _ = ix.GetName(ctx, "acme", "bookings/2026.csv"); !found {
		t.Fatal("v1: bookings not seeded")
	}

	// v2 (prior=1): the change-driven diff sees the removal, fans out, and
	// the materializer delete-misses bookings while keeping house-01.
	c.ReconcileStorePacks(ctx, "acme", "demo", 2, 1, true, false)
	if _, found, _ = ix.GetName(ctx, "acme", "bookings/2026.csv"); found {
		t.Fatal("v2: dropped row still linked")
	}
	if _, found, _ = ix.GetName(ctx, "acme", "faqs/house-01.txt"); !found {
		t.Fatal("v2: carried-forward row unlinked")
	}

	// v3 (prior=2): tree gone → EmptyTree marker → all seeded names unlinked.
	c.ReconcileStorePacks(ctx, "acme", "demo", 3, 2, true, false)
	if _, found, _ = ix.GetName(ctx, "acme", "faqs/house-01.txt"); found {
		t.Fatal("v3: seeded name survived tree removal")
	}

	// A shared-index materializer is skipped on a non-origin node.
	c.SetStoreReconciler(storeseed.NewReconciler(blobseed.New(ix, cas, true)))
	c.ReconcileStorePacks(ctx, "acme", "demo", 1, 0, false, false)
	if _, found, _ = ix.GetName(ctx, "acme", "faqs/house-01.txt"); found {
		t.Fatal("shared materializer ran on a data-plane node")
	}
}

// TestListStackBlobsEndpoint — the seeded-names + drift view `txco data apply`
// consults: only rows seeded by the named stack, with drift computed from the
// pack bookkeeping; 503 without an index.
func TestListStackBlobsEndpoint(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	rec := httptest.NewRecorder()
	req := mux.SetURLVars(withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/stacks/demo/blobs", nil), "tnt_default"),
		map[string]string{"name": "demo"})
	c.handleListStackBlobs(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no index: status=%d", rec.Code)
	}

	ix := newBlobIndexForTest(t)
	c.SetBlobIndex(ix)
	hA, hB := sha256Hex("A"), sha256Hex("B")
	now := time.Now()
	rows := []blob.NameRow{
		{Name: "faqs/clean.txt", SHA256: hA, SeededBy: "demo", SeededSHA: hA, CreatedAt: now, UpdatedAt: now},
		{Name: "faqs/edited.txt", SHA256: hB, SeededBy: "demo", SeededSHA: hA, CreatedAt: now, UpdatedAt: now}, // drift
		{Name: "docs/runtime.txt", SHA256: hA, CreatedAt: now, UpdatedAt: now},                                 // runtime-only
		{Name: "other/x.txt", SHA256: hA, SeededBy: "otherstack", SeededSHA: hA, CreatedAt: now, UpdatedAt: now},
	}
	for _, r := range rows {
		if err := ix.PutName(ctx, "default", r); err != nil {
			t.Fatal(err)
		}
	}
	rec = httptest.NewRecorder()
	req = mux.SetURLVars(withTenantAdminCtx(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/stacks/demo/blobs", nil), "tnt_default"),
		map[string]string{"name": "demo"})
	c.handleListStackBlobs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out stackBlobsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 2 || len(out.Names) != 2 {
		t.Fatalf("want exactly the 2 names seeded by demo: %+v", out)
	}
	byName := map[string]stackBlobRow{}
	for _, r := range out.Names {
		byName[r.Name] = r
	}
	if byName["faqs/clean.txt"].Drifted || !byName["faqs/edited.txt"].Drifted || byName["faqs/edited.txt"].SeededSHA != hA {
		t.Fatalf("drift flags: %+v", out.Names)
	}
}

// TestReconcileStorePacksForce — `txco data apply --force` on an UNCHANGED
// version still reconciles and the tree wins over a runtime edit.
func TestReconcileStorePacksForce(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	c.fcas = &mapStore{m: map[string][]byte{}}
	cas, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ix := newBlobIndexForTest(t)
	c.SetStoreReconciler(storeseed.NewReconciler(blobseed.New(ix, cas, false)))
	seedStackVersions(t, c, "stk", "acme", "demo", 1, 2)
	hHello := sha256Hex("hello")
	if err := cas.Put(ctx, hHello, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	seedCASRow(t, c, 1, "BLOBS/hello.txt", hHello)
	seedCASRow(t, c, 2, "BLOBS/hello.txt", hHello) // identical tree
	c.ReconcileStorePacks(ctx, "acme", "demo", 1, 0, true, false)

	// Runtime edit → drift.
	hHola := sha256Hex("hola")
	if err := cas.Put(ctx, hHola, []byte("hola")); err != nil {
		t.Fatal(err)
	}
	row, _, _ := ix.GetName(ctx, "acme", "hello.txt")
	row.SHA256 = hHola
	if err := ix.PutName(ctx, "acme", row); err != nil {
		t.Fatal(err)
	}
	// Unforced re-apply of the identical tree: change-driven skip → drift stands.
	c.ReconcileStorePacks(ctx, "acme", "demo", 2, 1, true, false)
	if r, _, _ := ix.GetName(ctx, "acme", "hello.txt"); r.SHA256 != hHola {
		t.Fatalf("unforced apply clobbered the runtime edit: %+v", r)
	}
	// --force: every pack reconciled, tree wins.
	c.ReconcileStorePacks(ctx, "acme", "demo", 2, 1, true, true)
	if r, _, _ := ix.GetName(ctx, "acme", "hello.txt"); r.SHA256 != hHello || r.Drifted() {
		t.Fatalf("--force must re-assert the tree: %+v", r)
	}
}
