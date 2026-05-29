package sysops

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// opsSchema mirrors the post-migration runtime shape the dbcache
// snapshot carries (0001 ops + 0002 tenants + 0006 tenant-scoped
// uniqueness + 0007 _sys seed).
func newSnap(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT, revoked_at TEXT);
		CREATE TABLE ops (stack TEXT, scope INTEGER, name TEXT NOT NULL DEFAULT '', txcl TEXT, mock_req TEXT, mock_res TEXT, tenant_id TEXT, UNIQUE(stack,scope,txcl,tenant_id));
		INSERT OR IGNORE INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_sys','_sys','System', '');
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func TestLoadEmbeddedDefault(t *testing.T) {
	l, err := Load(Config{})
	if err != nil {
		t.Fatalf("Load embedded: %v", err)
	}
	if _, ok := l.bySlug()[tenants.SystemTenantSlug]; !ok {
		t.Fatalf("embedded bundle missing %q", tenants.SystemTenantSlug)
	}
	if n := l.BootOpCount(); n < 2 {
		t.Fatalf("BootOpCount = %d, want >= 2 (detect + route)", n)
	}
}

func TestApplyIdempotent(t *testing.T) {
	db := newSnap(t)
	l, err := Load(Config{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ops WHERE tenant_id = ?`, tenants.SystemTenantID).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if err := l.Apply(db); err != nil {
		t.Fatalf("Apply #1: %v", err)
	}
	first := count()
	if first < 2 {
		t.Fatalf("after Apply: %d _sys ops, want >= 2", first)
	}
	// Re-apply (simulates dbcache reload re-running OnReload). Must not
	// double-insert or trip UNIQUE(stack,scope,txcl,tenant_id).
	if err := l.Apply(db); err != nil {
		t.Fatalf("Apply #2: %v", err)
	}
	if got := count(); got != first {
		t.Fatalf("re-apply changed op count: %d -> %d (not idempotent)", first, got)
	}
	// Ops are tenant-attributed to _sys and the boot stack resolves.
	var boot int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ops WHERE tenant_id=? AND stack='boot'`, tenants.SystemTenantID).Scan(&boot); err != nil {
		t.Fatalf("boot count: %v", err)
	}
	if boot < 2 {
		t.Fatalf("_sys boot ops = %d, want >= 2", boot)
	}
}

func TestDirOverrideMergesAndExtends(t *testing.T) {
	dir := t.TempDir()
	// Unified layout: system stacks live in the same OPS/ tree as app
	// stacks, under a `_`-prefixed segment. A second system tenant
	// _playground, defined only on disk:
	writeFile(t, dir, "OPS/_playground/scratch/0/try.txcl", `EXEC "txco://noop"`)
	// Override the embedded _sys boot/0 (same stack/scope/name key)
	// and ADD a new boot/10 rule. boot/20 stays from the embed.
	writeFile(t, dir, "OPS/_sys/boot/0/detect.txcl", `EXEC "txco://detect-tenant"`)
	writeFile(t, dir, "OPS/_sys/boot/10/limit.txcl", `EXEC "txco://noop"`)

	l, err := Load(Config{Dir: dir})
	if err != nil {
		t.Fatalf("Load override: %v", err)
	}
	groups := l.bySlug()
	if _, ok := groups["_playground"]; !ok {
		t.Fatalf("override did not add _playground")
	}
	// File-granular merge of the _sys `boot` stack: embed has scopes
	// 0,50,1000,100 (50 = static); disk overrides boot/0 and adds
	// boot/10 → boot scopes {0,10,50,100,1000}. (_sys may carry other
	// stacks too, e.g. txc-continuation — assert on `boot` specifically.)
	scopes := map[int]bool{}
	for _, o := range groups[tenants.SystemTenantSlug] {
		if o.Stack == "boot" {
			scopes[o.Scope] = true
		}
	}
	if len(scopes) != 5 {
		t.Fatalf("_sys boot merge: scopes %v, want {0,10,50,100,1000}", scopes)
	}
	for _, want := range []int{0, 10, 50, 100, 1000} {
		if !scopes[want] {
			t.Errorf("_sys boot merge missing scope %d", want)
		}
	}

	db := newSnap(t)
	if err := l.Apply(db); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// _playground got its own (loader-created) tenant row + ops.
	var pid string
	if err := db.QueryRow(`SELECT tenant_id FROM tenants WHERE slug='_playground'`).Scan(&pid); err != nil {
		t.Fatalf("_playground tenant not created: %v", err)
	}
	if pid != "tnt_playground" {
		t.Errorf("_playground tenant_id = %q, want tnt_playground", pid)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ops WHERE tenant_id=?`, pid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("_playground ops = %d (err %v), want 1", n, err)
	}
}

func TestLoadRejectsInvalidTxcl(t *testing.T) {
	dir := t.TempDir()
	// `EXEC` with no target is a deterministic parser error
	// ("exec missing execname"). A broken system rule must fail the
	// chassis at startup, not silently misroute.
	writeFile(t, dir, "OPS/_sys/boot/0/bad.txcl", `EXEC`)
	if _, err := Load(Config{Dir: dir}); err == nil {
		t.Fatalf("Load accepted invalid txcl, want error")
	}
}

func TestLoadRejectsInvalidRuleName(t *testing.T) {
	dir := t.TempDir()
	// Valid txcl, but the rule name (the .txcl stem) contains a char
	// outside [A-Za-z0-9_-]. The loader must reject it at load with the
	// same loud contract as a parse error (the watcher then keeps the
	// previous good bundle).
	writeFile(t, dir, "OPS/_sys/boot/0/he%llo.txcl", `EXEC "txco://noop"`)
	if _, err := Load(Config{Dir: dir}); err == nil {
		t.Fatalf("Load accepted invalid rule name, want error")
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
