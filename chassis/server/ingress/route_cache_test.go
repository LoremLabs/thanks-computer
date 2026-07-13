package ingress

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// seedHostnameFull is seedHostname with the verified_at/revoked_at
// columns exposed — the oracle fixture needs every row shape the
// resolver's filters distinguish. Empty string means NULL.
func seedHostnameFull(t *testing.T, db *sql.DB, id, hostname, tenantID, stack, verifiedAt, revokedAt string) {
	t.Helper()
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	if _, err := db.Exec(
		`INSERT INTO tenant_hostnames (id, hostname, tenant_id, stack, created_at, verified_at, revoked_at)
		 VALUES (?, ?, ?, ?, '2026-01-01T00:00:00Z', ?, ?)`,
		id, hostname, tenantID, stack, nullable(verifiedAt), nullable(revokedAt)); err != nil {
		t.Fatalf("seed hostname %s: %v", hostname, err)
	}
}

// newRouteCacheFixture builds a store with one row per filter case the
// live lookupHTTP query distinguishes. MaxOpenConns(1) mirrors the
// production dbcache pin (and keeps the :memory: db coherent — a second
// pool connection would be a separate empty database).
func newRouteCacheFixture(t *testing.T) *sql.DB {
	t.Helper()
	db := newDBResolverTestStore(t)
	db.SetMaxOpenConns(1)
	seedTenant(t, db, "t1", "acme")
	seedTenant(t, db, "t2", "dead-corp")
	if _, err := db.Exec(`UPDATE tenants SET revoked_at = '2026-02-01T00:00:00Z' WHERE tenant_id = 't2'`); err != nil {
		t.Fatalf("revoke tenant: %v", err)
	}
	const v, r = "2026-01-02T00:00:00Z", "2026-02-01T00:00:00Z"
	seedHostnameFull(t, db, "h1", "verified.example", "t1", "shop", v, "")
	seedHostnameFull(t, db, "h2", "unverified.example", "t1", "blog", "", "")
	seedHostnameFull(t, db, "h3", "revoked.example", "t1", "shop", v, r)
	seedHostnameFull(t, db, "h4", "unattached.example", "t1", "", v, "")
	seedHostnameFull(t, db, "h5", "deadtenant.example", "t2", "shop", v, "")
	return db
}

// oracleHosts is every fixture hostname plus a genuine miss.
var oracleHosts = []string{
	"verified.example",
	"unverified.example",
	"revoked.example",
	"unattached.example",
	"deadtenant.example",
	"missing.example",
}

// TestHostRouteCacheOracle pins fix 1's correctness contract: cached
// resolution must be answer-identical to the live SQL lookup for every
// row shape the query filters distinguish, under both permissive and
// strict verification. The cache-backed resolver gets a nil DB handle,
// PROVING the answers come from the map and never fall back to SQL.
func TestHostRouteCacheOracle(t *testing.T) {
	for _, strict := range []bool{false, true} {
		db := newRouteCacheFixture(t)

		sqlResolver := NewDBResolver(nil, db, zap.NewNop(), strict)

		cache := NewHostRouteCache()
		if err := cache.Rebuild(db); err != nil {
			t.Fatalf("Rebuild: %v", err)
		}
		cachedResolver := NewDBResolverFunc(nil,
			func() *sql.DB { return nil }, // any SQL fallback would nil-miss and fail the oracle
			zap.NewNop(), strict)
		cachedResolver.SetHostRouteCache(cache)

		for _, host := range oracleHosts {
			key := RouteKey{Src: "http", Hostname: host}
			wantT, wantOK := sqlResolver.Resolve(key)
			gotT, gotOK := cachedResolver.Resolve(key)
			if wantOK != gotOK || wantT != gotT {
				t.Errorf("strict=%v host=%s: cache=(%+v,%v) sql=(%+v,%v)",
					strict, host, gotT, gotOK, wantT, wantOK)
			}
		}
	}
}

// TestHostRouteCacheNotReadyFallsBackToSQL pins the degradation mode:
// a cache that never built (boot-time rebuild failure) must leave the
// per-request SQL path serving, not turn every hostname into a miss.
func TestHostRouteCacheNotReadyFallsBackToSQL(t *testing.T) {
	db := newRouteCacheFixture(t)
	r := NewDBResolver(nil, db, zap.NewNop(), false)
	r.SetHostRouteCache(NewHostRouteCache()) // installed but never Rebuild()t

	target, ok := r.Resolve(RouteKey{Src: "http", Hostname: "verified.example"})
	if !ok || target.Tenant != "acme" || target.Stack != "shop" {
		t.Fatalf("not-ready cache must fall back to SQL; got (%+v, %v)", target, ok)
	}
}

// TestHostRouteCacheConcurrentRebuild hammers cached resolution while
// Rebuild loops — the production shape (requests racing dbcache
// reloads). Run under -race; every resolve must return the stable
// verified row, never a fabricated miss.
func TestHostRouteCacheConcurrentRebuild(t *testing.T) {
	db := newRouteCacheFixture(t)
	cache := NewHostRouteCache()
	if err := cache.Rebuild(db); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	r := NewDBResolverFunc(nil, func() *sql.DB { return nil }, zap.NewNop(), false)
	r.SetHostRouteCache(cache)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := cache.Rebuild(db); err != nil {
				t.Errorf("Rebuild: %v", err)
				return
			}
		}
	}()

	var readers sync.WaitGroup
	for g := 0; g < 8; g++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 500; i++ {
				target, ok := r.Resolve(RouteKey{Src: "http", Hostname: "verified.example"})
				if !ok || target.Tenant != "acme" {
					t.Errorf("resolve under rebuild churn: (%+v, %v)", target, ok)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	wg.Wait()
}

// TestResolveErrReportsTransientFailure pins fix 2's resolver half: a
// lookup that FAILS (here: the mirror's one connection is held past the
// 250ms deadline) must surface an error — distinct from a miss — while
// Resolve() still collapses it to a miss for callers with no error
// channel. Without this distinction a saturated mirror mis-serves real
// hostnames as 404s.
func TestResolveErrReportsTransientFailure(t *testing.T) {
	db := newRouteCacheFixture(t)
	r := NewDBResolver(nil, db, zap.NewNop(), false)

	// A genuine miss reports NO error.
	if _, ok, err := r.ResolveErr(RouteKey{Src: "http", Hostname: "missing.example"}); ok || err != nil {
		t.Fatalf("genuine miss: want (false, nil), got (%v, %v)", ok, err)
	}

	// Wedge the mirror: check out its only connection so the lookup
	// can't get one before its deadline.
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("checkout conn: %v", err)
	}
	if _, ok, rerr := r.ResolveErr(RouteKey{Src: "http", Hostname: "verified.example"}); ok || rerr == nil {
		t.Fatalf("wedged mirror: want (false, transient error), got (%v, %v)", ok, rerr)
	}
	if _, ok := r.Resolve(RouteKey{Src: "http", Hostname: "verified.example"}); ok {
		t.Fatal("Resolve must collapse a transient failure to a miss")
	}
	_ = conn.Close()

	// Released: the same hostname resolves cleanly again.
	target, ok, rerr := r.ResolveErr(RouteKey{Src: "http", Hostname: "verified.example"})
	if !ok || rerr != nil || target.Tenant != "acme" {
		t.Fatalf("post-release: want acme route, got (%+v, %v, %v)", target, ok, rerr)
	}
}
