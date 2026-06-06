package static

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// tenantDDL mirrors the runtime schema closely enough for RebuildTenant's
// join (stacks.active_version = stack_files.version_id, joined to tenants).
const tenantDDL = `
CREATE TABLE tenants (
  tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT,
  created_at TEXT NOT NULL, revoked_at TEXT
);
CREATE TABLE stacks (
  stack_id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, name TEXT NOT NULL,
  active_version INTEGER, created_at TEXT NOT NULL
);
CREATE TABLE stack_files (
  version_id INTEGER NOT NULL, path TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL DEFAULT '', PRIMARY KEY (version_id, path)
);`

func hhex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func tenantDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(tenantDDL); err != nil {
		t.Fatal(err)
	}
	return db
}

func insTenant(t *testing.T, db *sql.DB, id, slug string, revoked bool) {
	t.Helper()
	var rev any
	if revoked {
		rev = "2026-01-01T00:00:00Z"
	}
	if _, err := db.Exec(`INSERT INTO tenants(tenant_id,slug,name,created_at,revoked_at) VALUES(?,?,?,?,?)`,
		id, slug, slug, "2026-01-01T00:00:00Z", rev); err != nil {
		t.Fatal(err)
	}
}

func insStack(t *testing.T, db *sql.DB, stackID, tenantID, name string, active int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO stacks(stack_id,tenant_id,name,active_version,created_at) VALUES(?,?,?,?,?)`,
		stackID, tenantID, name, active, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
}

func insFile(t *testing.T, db *sql.DB, version int, path, content, hash string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO stack_files(version_id,path,content,content_hash) VALUES(?,?,?,?)`,
		version, path, content, hash); err != nil {
		t.Fatal(err)
	}
}

func TestRebuildTenant(t *testing.T) {
	db := tenantDB(t)
	insTenant(t, db, "tnt_a", "acme", false)
	insTenant(t, db, "tnt_g", "globex", false)
	insTenant(t, db, "tnt_r", "gone", true)

	// acme/web @ active version 10
	insStack(t, db, "s_a", "tnt_a", "web", 10)
	insFile(t, db, 10, "FILES/page.html", "ACME", hhex("ACME"))
	insFile(t, db, 10, "FILES/assets/app.css", "css", hhex("css"))
	insFile(t, db, 10, "100/route.txcl", "rule", hhex("rule")) // non-FILES → excluded
	insFile(t, db, 10, "FILES/empty.html", "x", "")            // empty hash → skipped
	insFile(t, db, 9, "FILES/old.html", "OLD", hhex("OLD"))    // inactive version → excluded

	// globex/web @ active version 20 — SAME stack name, different bytes.
	insStack(t, db, "s_g", "tnt_g", "web", 20)
	insFile(t, db, 20, "FILES/page.html", "GLOBEX", hhex("GLOBEX"))

	// revoked tenant — excluded entirely.
	insStack(t, db, "s_r", "tnt_r", "web", 30)
	insFile(t, db, 30, "FILES/page.html", "R", hhex("R"))

	ix := NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatalf("RebuildTenant: %v", err)
	}

	// Tenant isolation: colliding stack name resolves to each tenant's bytes.
	ra := ix.Lookup("acme", "web", "/page.html")
	if !ra.Found || ra.Hash != hhex("ACME") {
		t.Fatalf("acme page: %+v", ra)
	}
	if ra.ETag != `"`+hhex("ACME")+`"` {
		t.Fatalf("acme ETag=%q", ra.ETag)
	}
	if ra.Ctype != "text/html; charset=utf-8" {
		t.Fatalf("acme ctype=%q", ra.Ctype)
	}
	if ra.Body != nil {
		t.Fatalf("tenant entry must carry no inline body")
	}
	rg := ix.Lookup("globex", "web", "/page.html")
	if !rg.Found || rg.Hash != hhex("GLOBEX") {
		t.Fatalf("globex page: %+v", rg)
	}
	if ra.Hash == rg.Hash {
		t.Fatal("tenant isolation broken: same hash across tenants")
	}

	// Non-FILES rows excluded.
	if r := ix.Lookup("acme", "web", "/100/route.txcl"); r.Found {
		t.Fatalf(".txcl must not be in the static index: %+v", r)
	}
	// Empty content_hash skipped.
	if r := ix.Lookup("acme", "web", "/empty.html"); r.Found {
		t.Fatalf("empty-hash row must be skipped: %+v", r)
	}
	// Inactive version excluded.
	if r := ix.Lookup("acme", "web", "/old.html"); r.Found {
		t.Fatalf("inactive-version file must be excluded: %+v", r)
	}
	// Revoked tenant excluded.
	if r := ix.Lookup("gone", "web", "/page.html"); r.Found {
		t.Fatalf("revoked tenant must be excluded: %+v", r)
	}
	// Owned directory prefix (a miss under FILES/assets/ is Owned → 404).
	if r := ix.Lookup("acme", "web", "/assets/missing.css"); r.Found || !r.Owned {
		t.Fatalf("miss under owned tenant dir must be Owned: %+v", r)
	}
	if r := ix.Lookup("acme", "web", "/assets/app.css"); !r.Found || r.Hash != hhex("css") ||
		r.Ctype != "text/css; charset=utf-8" {
		t.Fatalf("assets file: %+v", r)
	}
	// Unknown tenant / unknown stack → nothing.
	if r := ix.Lookup("nobody", "web", "/page.html"); r.Found || r.Owned {
		t.Fatalf("unknown tenant must miss: %+v", r)
	}
}

func TestRebuildTenantNilSafe(t *testing.T) {
	ix := NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(nil); err != nil {
		t.Fatalf("nil db must be a no-op, got %v", err)
	}
	// No tenant build done → tenant lookup must not panic and must miss.
	if r := ix.Lookup("acme", "web", "/page.html"); r.Found {
		t.Fatalf("fresh index tenant lookup: %+v", r)
	}
}
