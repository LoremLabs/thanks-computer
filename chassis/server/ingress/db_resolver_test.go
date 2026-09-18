package ingress

import (
	"database/sql"
	"sync/atomic"
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
			verified_at TEXT,
			dkim_selector    TEXT NOT NULL DEFAULT '',
			dkim_private_pem TEXT NOT NULL DEFAULT '',
			dkim_public_b64  TEXT NOT NULL DEFAULT ''
		);
		CREATE UNIQUE INDEX tenant_hostnames_active_hostname_idx
		    ON tenant_hostnames(hostname)
		    WHERE revoked_at IS NULL;
		CREATE TABLE stacks (
			stack_id        TEXT PRIMARY KEY,
			tenant_id       TEXT NOT NULL,
			name            TEXT NOT NULL,
			active_version  INTEGER,
			created_at      TEXT NOT NULL,
			UNIQUE(tenant_id, name)
		);
		-- The materialised ops the processor runs. Deliberately NO
		-- stack_files table here: a Postgres-backed mirror carries only its
		-- FILES/ and DATASETS/ rows, so routing must never depend on it.
		CREATE TABLE ops (
			stack       TEXT,
			scope       INTEGER,
			name        TEXT NOT NULL DEFAULT '',
			txcl        TEXT,
			tenant_id   TEXT
		);
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

// seedStack inserts a stack row; active=false leaves it without an
// active version (pushed as a draft, or deactivated).
var seedVersionID atomic.Int64

// seedStack inserts a stack; active gives it an active version with one
// materialised op.
func seedStack(t *testing.T, db *sql.DB, tenantID, name string, active bool) {
	t.Helper()
	var ver any
	if active {
		id := seedVersionID.Add(1)
		ver = id
		if _, err := db.Exec(`INSERT INTO ops (stack, scope, name, txcl, tenant_id) VALUES (?, 100, 'x', 'EMIT .x = 1', ?)`, name, tenantID); err != nil {
			t.Fatalf("seed op: %v", err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at)
		 VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z')`,
		"stk_"+tenantID+"_"+name, tenantID, name, ver); err != nil {
		t.Fatalf("seed stack: %v", err)
	}
}

// seedRetiredStack is a stack after `txco deactivate`: its active version
// is set, and empty — activation left it no ops.
func seedRetiredStack(t *testing.T, db *sql.DB, tenantID, name string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO stacks (stack_id, tenant_id, name, active_version, created_at)
		 VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z')`,
		"stk_"+tenantID+"_"+name, tenantID, name, seedVersionID.Add(1)); err != nil {
		t.Fatalf("seed retired stack: %v", err)
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

// listenerStub is a YAML-shaped resolver with one tcp listener entry.
type listenerStub struct {
	listener string
	target   RouteTarget
}

func (s *listenerStub) Resolve(key RouteKey) (RouteTarget, bool) {
	if key.Src == "tcp" && key.Listener == s.listener {
		return s.target, true
	}
	return RouteTarget{}, false
}

// TestDBResolverTCPHostname — a tcp key routes by its connection hostname:
// verified rows only (strict, whatever the chassis policy says), the YAML
// listener entry as the fallback when there is no hostname or no verified
// row for it. A listener name is never looked up as a hostname; cron never
// touches the table.
func TestDBResolverTCPHostname(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_db", "db-tenant")
	seedHostnameFull(t, db, "thn_v", "irc.verified.local", "tnt_db", "db-tenant/irc", "2026-01-02T00:00:00Z", "")
	seedHostname(t, db, "thn_u", "irc.unverified.local", "tnt_db", "db-tenant/irc")
	seedHostname(t, db, "thn_l", "raw", "tnt_db", "db-tenant/nope")
	seedStack(t, db, "tnt_db", "db-tenant/irc/_tcp", true) // the stack opted in
	yaml := &listenerStub{listener: "raw", target: RouteTarget{Tenant: "yaml-tenant", Stack: "yaml-tenant/raw", Ingress: "raw", Verified: true}}
	r := NewDBResolver(yaml, db, nil, false) // permissive chassis policy

	got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "irc", Hostname: "IRC.verified.local."})
	if !ok || got.Tenant != "db-tenant" || got.Stack != "db-tenant/irc/_tcp" || got.Ingress != "host:irc.verified.local" || !got.Verified {
		t.Errorf("verified hostname must route into the stack's _tcp inlet: got %+v ok=%v", got, ok)
	}
	if got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "irc", Hostname: "irc.unverified.local"}); ok {
		t.Errorf("unverified hostname must not route a connection even in permissive mode; got %+v", got)
	}
	if got, ok := r.Resolve(RouteKey{Src: "http", Hostname: "irc.unverified.local"}); !ok || got.Verified {
		t.Errorf("http stays permissive: got %+v ok=%v", got, ok)
	}
	if got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "raw"}); !ok || got.Tenant != "yaml-tenant" {
		t.Errorf("no hostname → YAML listener: got %+v ok=%v", got, ok)
	}
	if got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "raw", Hostname: "irc.unverified.local"}); !ok || got.Tenant != "yaml-tenant" {
		t.Errorf("unroutable hostname → YAML listener: got %+v ok=%v", got, ok)
	}
	if got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "raw", Hostname: "irc.verified.local"}); !ok || got.Tenant != "db-tenant" {
		t.Errorf("hostname wins over the YAML listener: got %+v ok=%v", got, ok)
	}
	if _, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "irc"}); ok {
		t.Errorf("a listener name is not a hostname")
	}
	if _, ok := r.Resolve(RouteKey{Src: "cron", Job: "irc.verified.local"}); ok {
		t.Errorf("cron source must not match DB hostnames")
	}
}

// TestDBResolverTCPIsOptIn — a verified hostname is not a TCP endpoint
// until its stack has an ACTIVE `_tcp` inlet. HTTP on the same hostname
// is untouched either way. Checked through both lookup paths: per-request
// SQL, and the route cache (with a nil DB, proving no SQL fallback).
func TestDBResolverTCPIsOptIn(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedTenant(t, db, "tnt_b", "other")
	for i, h := range []struct{ host, stack string }{
		{"irc.acme.example", "chat"},     // chat/_tcp active        → routes
		{"www.acme.example", "shop"},     // no _tcp at all          → closed
		{"draft.acme.example", "beta"},   // beta/_tcp never activated → closed
		{"retired.acme.example", "old"},  // old/_tcp deactivated (empty active version) → closed
		{"lookalike.acme.example", "ch"}, // `ch` has no inlet; LIKE's `_` wildcard must not invent one
		{"irc.other.example", "chat"},    // same stack name, another tenant, no inlet of its own
	} {
		tid := "tnt_a"
		if h.host == "irc.other.example" {
			tid = "tnt_b"
		}
		seedHostnameFull(t, db, "thn_"+string(rune('a'+i)), h.host, tid, h.stack, "2026-01-02T00:00:00Z", "")
	}
	seedStack(t, db, "tnt_a", "chat", true)
	seedStack(t, db, "tnt_a", "chat/_tcp", true)
	seedStack(t, db, "tnt_a", "shop", true)
	seedStack(t, db, "tnt_a", "shop/_mail", true) // a different inlet is not a TCP opt-in
	seedStack(t, db, "tnt_a", "beta", true)
	seedStack(t, db, "tnt_a", "beta/_tcp", false)
	seedStack(t, db, "tnt_a", "old", true)
	seedRetiredStack(t, db, "tnt_a", "old/_tcp")
	seedStack(t, db, "tnt_a", "ch", true)
	seedStack(t, db, "tnt_a", "ch/xtcp", true)
	seedStack(t, db, "tnt_b", "chat", true)

	cache := NewHostRouteCache()
	if err := cache.Rebuild(db); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	cached := NewDBResolverFunc(nil, func() *sql.DB { return nil }, nil, false)
	cached.SetHostRouteCache(cache)

	for name, r := range map[string]*DBResolver{"sql": NewDBResolver(nil, db, nil, false), "cache": cached} {
		got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "edge", Hostname: "irc.acme.example"})
		if !ok || got.Tenant != "acme" || got.Stack != "chat/_tcp" {
			t.Errorf("%s: opted-in stack: got %+v ok=%v", name, got, ok)
		}
		for _, host := range []string{"www.acme.example", "draft.acme.example", "retired.acme.example", "lookalike.acme.example", "irc.other.example"} {
			if got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "edge", Hostname: host}); ok {
				t.Errorf("%s: %s has no active _tcp inlet and must not route; got %+v", name, host, got)
			}
		}
		if got, ok := r.Resolve(RouteKey{Src: "http", Hostname: "www.acme.example"}); !ok || got.Stack != "shop" {
			t.Errorf("%s: http routing must not depend on a _tcp inlet: got %+v ok=%v", name, got, ok)
		}
	}
}

// TestDBResolverTCPInletFollowsHandler — a listener with a protocol
// handler asks for that protocol's inlet (RouteKey.Inlet): a stack opts
// into `_echo` separately from `_tcp`, and one never stands in for the
// other. A name that is not a single `_name` segment never routes by
// hostname.
func TestDBResolverTCPInletFollowsHandler(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostnameFull(t, db, "thn_a", "both.acme.example", "tnt_a", "both", "2026-01-02T00:00:00Z", "")
	seedHostnameFull(t, db, "thn_b", "line.acme.example", "tnt_a", "lineonly", "2026-01-02T00:00:00Z", "")
	seedStack(t, db, "tnt_a", "both/_tcp", true)
	seedStack(t, db, "tnt_a", "both/_echo", true)
	seedStack(t, db, "tnt_a", "lineonly/_tcp", true)

	cache := NewHostRouteCache()
	if err := cache.Rebuild(db); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	cached := NewDBResolverFunc(nil, func() *sql.DB { return nil }, nil, false)
	cached.SetHostRouteCache(cache)

	for name, r := range map[string]*DBResolver{"sql": NewDBResolver(nil, db, nil, false), "cache": cached} {
		for _, tc := range []struct {
			host, inlet, want string
		}{
			{"both.acme.example", "", "both/_tcp"},
			{"both.acme.example", "_tcp", "both/_tcp"},
			{"both.acme.example", "_echo", "both/_echo"},
			{"line.acme.example", "_tcp", "lineonly/_tcp"},
			{"line.acme.example", "_echo", ""}, // opted into line, not echo
			{"both.acme.example", "echo", ""},  // not an inlet name
			{"both.acme.example", "_", ""},
			{"both.acme.example", "_Echo", ""},
			{"both.acme.example", "_echo/0", ""},
			{"both.acme.example", "_tcp/../_echo", ""},
		} {
			got, ok := r.Resolve(RouteKey{Src: "tcp", Listener: "edge", Hostname: tc.host, Inlet: tc.inlet})
			if ok != (tc.want != "") || got.Stack != tc.want {
				t.Errorf("%s: %s inlet %q: got %+v ok=%v, want stack %q", name, tc.host, tc.inlet, got, ok, tc.want)
			}
		}
	}

	// The inlet is a tcp fact: KeyFromEnvelope reads it only for tcp.
	if k := KeyFromEnvelope(`{"_txc":{"src":"tcp","tcp":{"host":"h.example","inlet":"_echo"}}}`); k.Inlet != "_echo" {
		t.Errorf("tcp key inlet = %q", k.Inlet)
	}
	if k := KeyFromEnvelope(`{"_txc":{"src":"http","tcp":{"inlet":"_echo"}}}`); k.Inlet != "" {
		t.Errorf("http key must not carry an inlet, got %q", k.Inlet)
	}
}

// TestHostRouteCacheInletQueryFailureKeepsHosts — HTTP host routing must
// never hang on the inlet query: if it fails, the fresh host map is still
// published and inlet checks fall back to per-request SQL.
func TestHostRouteCacheInletQueryFailureKeepsHosts(t *testing.T) {
	db := newDBResolverTestStore(t)
	seedTenant(t, db, "tnt_a", "acme")
	seedHostnameFull(t, db, "thn_a", "irc.acme.example", "tnt_a", "chat", "2026-01-02T00:00:00Z", "")
	seedStack(t, db, "tnt_a", "chat/_tcp", true)

	broken := newDBResolverTestStore(t) // same hosts, but no stacks table
	seedTenant(t, broken, "tnt_a", "acme")
	seedHostnameFull(t, broken, "thn_a", "irc.acme.example", "tnt_a", "chat", "2026-01-02T00:00:00Z", "")
	if _, err := broken.Exec(`DROP TABLE stacks`); err != nil {
		t.Fatal(err)
	}

	cache := NewHostRouteCache()
	if err := cache.Rebuild(broken); err == nil {
		t.Fatal("Rebuild must report the inlet query failure")
	}
	if _, found, ready := cache.lookup("irc.acme.example"); !ready || !found {
		t.Fatalf("hosts must be published despite the inlet failure (found=%v ready=%v)", found, ready)
	}
	if _, ready := cache.inletActive("acme", "chat/_tcp"); ready {
		t.Fatal("inlets must report not-ready after a failed inlet query")
	}
	r := NewDBResolver(nil, db, nil, false)
	r.SetHostRouteCache(cache)
	if got, ok := r.Resolve(RouteKey{Src: "tcp", Hostname: "irc.acme.example"}); !ok || got.Stack != "chat/_tcp" {
		t.Errorf("not-ready inlets must fall back to SQL: got %+v ok=%v", got, ok)
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
