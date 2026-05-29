package ingress

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// newDBResolverTestStore builds the schema chunk the DBResolver
// queries against. Mirrors db/schema/sqlite/runtime/{0002,0003,0004}.
func newDBResolverTestStore(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE tenants (
			tenant_id  TEXT PRIMARY KEY,
			slug       TEXT NOT NULL UNIQUE,
			name       TEXT,
			created_at TEXT NOT NULL,
			revoked_at TEXT
		);
		CREATE TABLE tenant_hostnames (
			id          TEXT PRIMARY KEY,
			hostname    TEXT NOT NULL,
			tenant_id   TEXT NOT NULL,
			stack       TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			created_by  TEXT,
			revoked_at  TEXT,
			verified_at TEXT
		);
		CREATE UNIQUE INDEX tenant_hostnames_active_hostname_idx
		    ON tenant_hostnames(hostname)
		    WHERE revoked_at IS NULL;
	`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	return db
}

func seedTenant(t *testing.T, db *sql.DB, id, slug string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tenants (tenant_id, slug, created_at) VALUES (?, ?, '2026-01-01T00:00:00Z')`,
		id, slug); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
}

func seedHostname(t *testing.T, db *sql.DB, id, hostname, tenantID, stack string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at)
		 VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z')`,
		id, hostname, tenantID, stack); err != nil {
		t.Fatalf("seed hostname: %v", err)
	}
}

// stubResolver implements Resolver and returns whatever's in `targets`
// keyed on the RouteKey hostname. Lets us simulate YAML-first hit and
// YAML-miss behaviour without touching files.
type stubResolver struct {
	targets map[string]RouteTarget
}

func (s *stubResolver) Resolve(key RouteKey) (RouteTarget, bool) {
	if t, ok := s.targets[key.Hostname]; ok {
		return t, true
	}
	return RouteTarget{}, false
}

// TestDBResolverYAMLPrecedence — a hostname present in YAML wins,
// even if a different mapping exists in the DB.
func TestDBResolverYAMLPrecedence(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostname(t, db, "thn_1", "shared.local", "tnt_db", "db-tenant/web")

	yaml := &stubResolver{targets: map[string]RouteTarget{
		"shared.local": {Tenant: "yaml-tenant", Stack: "yaml-tenant/web", Ingress: "shared.local"},
	}}
	r := NewDBResolver(yaml, db, nil, false)
	got, ok := r.Resolve(RouteKey{Src: "http", Hostname: "shared.local"})
	if !ok {
		t.Fatal("expected hit from YAML")
	}
	if got.Tenant != "yaml-tenant" {
		t.Errorf("got tenant=%q, want yaml-tenant (YAML must win)", got.Tenant)
	}
}

// TestDBResolverFallsThroughToDB — YAML doesn't know this hostname;
// DB lookup succeeds.
func TestDBResolverFallsThroughToDB(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostname(t, db, "thn_1", "foo.local", "tnt_db", "db-tenant/web")

	r := NewDBResolver(&stubResolver{targets: map[string]RouteTarget{}}, db, nil, false)
	got, ok := r.Resolve(RouteKey{Src: "http", Hostname: "foo.local"})
	if !ok {
		t.Fatal("expected DB hit")
	}
	if got.Tenant != "db-tenant" || got.Stack != "db-tenant/web" {
		t.Errorf("got %+v, want tenant=db-tenant stack=db-tenant/web", got)
	}
	if got.Ingress != "host:foo.local" {
		t.Errorf("Ingress: got %q, want host:foo.local", got.Ingress)
	}
}

// TestDBResolverCanonicalizesHostname — a request with uppercase and
// port still matches the lowercase stored row.
func TestDBResolverCanonicalizesHostname(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostname(t, db, "thn_1", "foo.local", "tnt_db", "db-tenant/web")

	r := NewDBResolver(nil, db, nil, false)
	for _, in := range []string{"foo.local", "FOO.LOCAL", "Foo.Local:8080", "foo.local."} {
		got, ok := r.Resolve(RouteKey{Src: "http", Hostname: in})
		if !ok {
			t.Errorf("input %q: want hit, got miss", in)
			continue
		}
		if got.Tenant != "db-tenant" {
			t.Errorf("input %q: tenant=%q want db-tenant", in, got.Tenant)
		}
	}
}

// TestDBResolverMissReturnsFalse — neither YAML nor DB has the
// hostname; resolver returns false so the bus loop falls back to
// boot/%/0.
func TestDBResolverMissReturnsFalse(t *testing.T) {
	db := newDBResolverTestStore(t)
	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "nope.local"}); ok {
		t.Errorf("expected miss for absent hostname")
	}
}

// TestDBResolverSkipsNonHTTP — TCP and cron sources never hit the DB
// (the table only carries HTTP hostnames).
func TestDBResolverSkipsNonHTTP(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostname(t, db, "thn_1", "tcp-listener", "tnt_db", "db-tenant/tcp")
	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "tcp-listener"}); ok {
		t.Errorf("TCP source must not match DB hostnames")
	}
	if _, ok := r.Resolve(RouteKey{Src: "cron", Job: "tcp-listener"}); ok {
		t.Errorf("cron source must not match DB hostnames")
	}
}

// TestDBResolverRevokedRowMisses — a revoked row should not match,
// even though it's still in the table.
func TestDBResolverRevokedRowMisses(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	if _, err := db.Exec(
		`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at, revoked_at)
		 VALUES ('thn_revoked', 'gone.local', 'tnt_db', 'db-tenant/web',
		         '2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed revoked: %v", err)
	}
	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "gone.local"}); ok {
		t.Errorf("revoked hostname must not match")
	}
}

// TestDBResolverRevokedTenantMisses — a hostname pointing at a
// revoked tenant misses (`AND t.revoked_at IS NULL`).
func TestDBResolverRevokedTenantMisses(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	if _, err := db.Exec(
		`UPDATE tenants SET revoked_at = '2026-02-01T00:00:00Z' WHERE tenant_id = 'tnt_db'`); err != nil {
		t.Fatalf("revoke tenant: %v", err)
	}
	seedHostname(t, db, "thn_1", "foo.local", "tnt_db", "db-tenant/web")
	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "foo.local"}); ok {
		t.Errorf("hostname under revoked tenant must not match")
	}
}

// TestDBResolverNilInnerNoCrash — DBResolver with a nil inner resolver
// behaves like a DB-only router.
func TestDBResolverNilInnerNoCrash(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostname(t, db, "thn_1", "only.local", "tnt_db", "db-tenant/web")
	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "only.local"}); !ok {
		t.Errorf("expected DB hit even with nil inner")
	}
}

func seedVerifiedHostname(t *testing.T, db *sql.DB, id, hostname, tenantID, stack, verifiedAt string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at, verified_at)
		 VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z', ?)`,
		id, hostname, tenantID, stack, verifiedAt); err != nil {
		t.Fatalf("seed verified hostname: %v", err)
	}
}

// TestDBResolverStrictMissesUnverified — with requireVerified=true,
// unverified rows are filtered out of the JOIN and the bus falls
// through to boot/%/0.
func TestDBResolverStrictMissesUnverified(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostname(t, db, "thn_unv", "unverified.local", "tnt_db", "db-tenant/web")
	r := NewDBResolver(nil, db, nil, true)
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "unverified.local"}); ok {
		t.Errorf("strict mode: unverified hostname must not match")
	}
}

// TestDBResolverStrictHitsVerified — same setup with verified_at set
// routes normally.
func TestDBResolverStrictHitsVerified(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedVerifiedHostname(t, db, "thn_v", "verified.local", "tnt_db", "db-tenant/web",
		"2026-02-01T00:00:00Z")
	r := NewDBResolver(nil, db, nil, true)
	got, ok := r.Resolve(RouteKey{Src: "http", Hostname: "verified.local"})
	if !ok {
		t.Fatalf("strict mode: verified hostname should route")
	}
	if got.Tenant != "db-tenant" {
		t.Errorf("tenant: got %q", got.Tenant)
	}
}

// TestDBResolverMissesUnattachedHostname — a hostname row with an
// empty stack column (the unattached state in the decoupled flow)
// never routes, even when verified. The Vercel-style two-step:
// verification proves ownership, attachment chooses routing target;
// both required for routing.
func TestDBResolverMissesUnattachedHostname(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	if _, err := db.Exec(
		`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at, verified_at)
		 VALUES ('thn_unattached', 'unattached.local', 'tnt_db', '',
		         '2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed unattached: %v", err)
	}
	// Both permissive and strict modes should miss unattached rows.
	for _, requireVerified := range []bool{false, true} {
		r := NewDBResolver(nil, db, nil, requireVerified)
		if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "unattached.local"}); ok {
			t.Errorf("requireVerified=%v: unattached hostname must not route", requireVerified)
		}
	}
}

// TestDBResolverPermissiveRoutesUnverified — default mode routes
// unverified rows but logs a WARN once. We don't capture the log here
// (zap.NewNop is in use), just confirm the route succeeds.
func TestDBResolverPermissiveRoutesUnverified(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostname(t, db, "thn_unv", "permissive.local", "tnt_db", "db-tenant/web")
	r := NewDBResolver(nil, db, nil, false)
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "permissive.local"}); !ok {
		t.Errorf("permissive mode: unverified hostname should still route")
	}
	// Second request — the warn-once dedup should keep the log quiet
	// (verified manually by inspecting r.warned). Functionally the
	// route still works.
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "permissive.local"}); !ok {
		t.Errorf("permissive mode: second request should still route")
	}
}

// TestDBResolverLiveHandleAfterSwap is the regression guard for the
// captured-mirror bug: dbcache.Reload() swaps dbc.Db to a fresh
// *sql.DB, so a resolver holding a fixed handle never sees hostnames
// written after boot (operator-bound or auto-minted) — they 404 until
// restart. NewDBResolverFunc(provider) must read the CURRENT handle so
// post-swap rows route; NewDBResolver(fixed) intentionally does not.
func TestDBResolverLiveHandleAfterSwap(t *testing.T) {
	db1 := newDBResolverTestStore(t) // "boot" mirror — no app hostname
	db2 := newDBResolverTestStore(t) // "post-reload" mirror — has it
	seedTenant(t, db1, "tnt_x", "x")
	seedTenant(t, db2, "tnt_x", "x")
	seedHostname(t, db2, "thn_live", "minted.stacks.test", "tnt_x", "shop")

	current := db1 // dbcache.Db starts here; Reload() will swap it
	live := NewDBResolverFunc(nil, func() *sql.DB { return current }, nil, false)
	fixed := NewDBResolver(nil, db1, nil, false) // captured at "boot"

	key := RouteKey{Src: "http", Hostname: "minted.stacks.test"}

	// Pre-swap: nobody resolves it (it's only in db2).
	if _, ok := live.Resolve(key); ok {
		t.Fatal("pre-swap: live resolver should miss (row not in current mirror)")
	}

	current = db2 // <- simulates dbcache.Reload swapping dbc.Db

	got, ok := live.Resolve(key)
	if !ok || got.Stack != "shop" {
		t.Fatalf("post-swap: live resolver must see the new handle; got %+v ok=%v", got, ok)
	}
	if _, ok := fixed.Resolve(key); ok {
		t.Fatal("fixed resolver must stay pinned to the boot handle (documents why the data plane uses NewDBResolverFunc)")
	}

	// nil provider must not panic.
	if _, ok := NewDBResolverFunc(nil, nil, nil, false).Resolve(key); ok {
		t.Fatal("nil dbFn should miss safely")
	}
}
