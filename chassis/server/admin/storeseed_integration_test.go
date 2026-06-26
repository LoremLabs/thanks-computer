package admin

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/vecseed"
	"github.com/loremlabs/thanks-computer/chassis/vector"
	"github.com/loremlabs/thanks-computer/chassis/vector/sqlitevec"
)

func newVecReconciler(t *testing.T, c *Controller) vector.Store {
	t.Helper()
	vs, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("sqlitevec: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })
	c.SetStoreReconciler(storeseed.NewReconciler(vecseed.New(vs, false)))
	return vs
}

func seedVersion(t *testing.T, c *Controller, versionID int64, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if _, err := c.pu.RuntimeDB.ExecContext(context.Background(),
			`INSERT INTO stack_files (version_id, path, content) VALUES (?,?,?)`,
			versionID, path, content); err != nil {
			t.Fatalf("seed file %s: %v", path, err)
		}
	}
}

// End-to-end at the admin layer: a version's inline VECTORS pack reconciles into
// the vector store (control-plane shape), re-apply is idempotent, and a new
// version that drops an item delete-misses it. Non-pack files are ignored.
func TestReconcileStorePacksFromVersion(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	vs := newVecReconciler(t, c)

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES ('stk1','acme','recs',1,'t')`); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (1,'stk1',1,'superseded','test','t')`); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	seedVersion(t, c, 1, map[string]string{
		"VECTORS/books.jsonl": `{"id":"pooh","vector":[1,0,0],"metadata":{"pd":true},"text":"bear","model":"m"}
{"id":"moby","vector":[0,0,1],"metadata":{"pd":false},"text":"whale","model":"m"}`,
		"100/x.txcl": "NOOP", // a normal op — reconcile must ignore it
	})

	c.ReconcileStorePacks(ctx, "acme", "recs", 1, 0 /*prior*/, true)

	coll, found, err := vs.DescribeCollection(ctx, "acme", "books")
	if err != nil || !found {
		t.Fatalf("collection: found=%v err=%v", found, err)
	}
	if coll.Dimensions != 3 || coll.EmbeddingModel != "m" {
		t.Fatalf("pin wrong: %+v", coll)
	}
	if ids, _ := vs.ListIDs(ctx, "acme", "books"); len(ids) != 2 {
		t.Fatalf("after v1: %v want 2", ids)
	}

	// Idempotent re-apply of the same version (prior=0 ⇒ reconcile all).
	c.ReconcileStorePacks(ctx, "acme", "recs", 1, 0, true)
	if ids, _ := vs.ListIDs(ctx, "acme", "books"); len(ids) != 2 {
		t.Fatalf("re-apply: %v want 2", ids)
	}

	// New version drops moby → within-collection delete-missing (prior=1, the
	// pack hash changed, so it reconciles).
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (2,'stk1',2,'superseded','test','t')`); err != nil {
		t.Fatalf("seed v2: %v", err)
	}
	seedVersion(t, c, 2, map[string]string{
		"VECTORS/books.jsonl": `{"id":"pooh","vector":[1,0,0],"metadata":{"pd":true},"model":"m"}`,
	})
	c.ReconcileStorePacks(ctx, "acme", "recs", 2, 1, true)
	ids, _ := vs.ListIDs(ctx, "acme", "books")
	if len(ids) != 1 || ids[0] != "pooh" {
		t.Fatalf("after v2: %v want [pooh]", ids)
	}

	// Change-driven SKIP: v3 carries the SAME pack as v2 forward (a code-only
	// deploy). Reconcile with prior=2 must do nothing — proven by deleting an
	// item out-of-band and confirming the unchanged-pack reconcile does NOT
	// re-add it (a full reconcile would).
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (3,'stk1',3,'superseded','test','t')`); err != nil {
		t.Fatalf("seed v3: %v", err)
	}
	seedVersion(t, c, 3, map[string]string{
		"VECTORS/books.jsonl": `{"id":"pooh","vector":[1,0,0],"metadata":{"pd":true},"model":"m"}`, // identical to v2
	})
	if _, err := vs.Delete(ctx, "acme", "books", []string{"pooh"}); err != nil {
		t.Fatalf("out-of-band delete: %v", err)
	}
	c.ReconcileStorePacks(ctx, "acme", "recs", 3, 2 /*prior=v2, unchanged*/, true)
	if ids, _ := vs.ListIDs(ctx, "acme", "books"); len(ids) != 0 {
		t.Fatalf("change-driven should have skipped the unchanged pack; got %v (re-added → not skipped)", ids)
	}
}

// Data-plane shape: a fingerprint-only pack row (content blanked, content_hash
// set) resolves its bytes from the shared CAS before reconciling.
func TestReconcileStorePacksResolvesCAS(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	vs := newVecReconciler(t, c)

	pack := `{"id":"a","vector":[1,0],"model":"m"}`
	h := sha256Hex(pack)
	c.fcas = &mapStore{m: map[string][]byte{h: []byte(pack)}}

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES ('stk2','acme','recs',1,'t')`); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (10,'stk2',1,'superseded','test','t')`); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	// content blanked, only the fingerprint — exactly what applyStackActivated writes.
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_files (version_id, path, content, content_hash) VALUES (10,'VECTORS/books.jsonl','',?)`, h); err != nil {
		t.Fatalf("seed pack row: %v", err)
	}

	c.ReconcileStorePacks(ctx, "acme", "recs", 1, 0 /*prior; first activation*/, false)

	if ids, _ := vs.ListIDs(ctx, "acme", "books"); len(ids) != 1 || ids[0] != "a" {
		t.Fatalf("CAS-resolved reconcile: %v want [a]", ids)
	}
}

// Regression: the store key is the tenant SLUG, not the tenant_id. The runtime
// reads the vector store by processor.TenantScope (the slug); a reconcile that
// keyed by tenant_id would seed a collection the runtime can NEVER find — the
// prod "collection not found for tenant <slug>" bug. With a tenant whose id≠slug,
// the collection must land under the slug, not the id.
func TestReconcileStorePacksKeysBySlug(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	vs := newVecReconciler(t, c)

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_x','myco','myco','t')`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES ('stk1','tnt_x','recs',1,'t')`); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (1,'stk1',1,'superseded','test','t')`); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	seedVersion(t, c, 1, map[string]string{
		"VECTORS/books.jsonl": `{"id":"pooh","vector":[1,0,0],"metadata":{},"model":"m"}`,
	})

	// Reconcile is called with the tenant_ID (the control-plane identifier).
	c.ReconcileStorePacks(ctx, "tnt_x", "recs", 1, 0 /*prior*/, true)

	if _, found, _ := vs.DescribeCollection(ctx, "myco", "books"); !found {
		t.Fatal("collection not under slug 'myco' (the runtime key) — keyed by id instead?")
	}
	if _, found, _ := vs.DescribeCollection(ctx, "tnt_x", "books"); found {
		t.Fatal("collection found under id 'tnt_x' — the slug-keying regression is back")
	}
}

// Regression: self-healing reconcile. A code-only deploy carries an UNCHANGED
// pack forward (change-driven would skip), but if the collection is absent from
// the store — a fresh/wiped node, or the one-time re-key migration — it must
// re-seed anyway rather than leave the runtime with no collection.
func TestReconcileStorePacksSelfHealsMissingCollection(t *testing.T) {
	ctx := context.Background()
	c := newTestController(t, config.Config{})
	vs := newVecReconciler(t, c)
	c.SetVectorStore(vs) // the self-heal presence-probe reads c.vstore (same store in prod)

	if _, err := c.pu.RuntimeDB.ExecContext(ctx,
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at) VALUES ('stk1','acme','recs',2,'t')`); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
	for _, v := range []int64{1, 2} {
		if _, err := c.pu.RuntimeDB.ExecContext(ctx,
			`INSERT INTO stack_versions (version_id, stack_id, version_number, status, created_by, created_at) VALUES (?, 'stk1', ?, 'superseded','test','t')`, v, v); err != nil {
			t.Fatalf("seed v%d: %v", v, err)
		}
		seedVersion(t, c, v, map[string]string{ // identical pack across versions
			"VECTORS/books.jsonl": `{"id":"pooh","vector":[1,0,0],"metadata":{},"model":"m"}`,
		})
	}

	c.ReconcileStorePacks(ctx, "acme", "recs", 1, 0, true) // first activation → seeds
	if _, found, _ := vs.DescribeCollection(ctx, "acme", "books"); !found {
		t.Fatal("v1 did not seed the collection")
	}

	// Simulate a wiped node / re-key migration: the collection is gone, but the
	// pack content is identical to the prior version (change-driven would skip).
	if _, err := vs.DropCollection(ctx, "acme", "books"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	c.ReconcileStorePacks(ctx, "acme", "recs", 2, 1 /*prior, unchanged pack*/, true)
	if _, found, _ := vs.DescribeCollection(ctx, "acme", "books"); !found {
		t.Fatal("self-heal failed: unchanged pack + missing collection was not re-seeded")
	}
}
