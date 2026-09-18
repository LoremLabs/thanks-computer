package ingress

import (
	"database/sql"
	"fmt"
	"sync/atomic"
)

// HostRouteCache is the precomputed hostname → (tenant, stack, verified)
// map the data-plane HTTP resolver reads instead of querying the dbcache
// mirror per request (todo-route-resolution-404-under-load, fix 1).
//
// Why it exists: the mirror is a `:memory:` SQLite handle pinned to ONE
// connection (each go-sqlite3 :memory: connection is a separate database,
// so the pin is load-bearing — see dbcache.New). Every per-request lookup
// therefore serializes through that single connection; under sustained
// request concurrency the queue blows the lookup's 250ms deadline and a
// hostname that resolves fine gets mis-served. The cache turns the hot
// path into an O(1) read of an atomically-swapped map that never touches
// the mirror connection.
//
// Freshness contract: Rebuild is called once at startup and then from the
// dbcache.OnReload chain, against the db handle the hook is passed (the
// still-private post-reload mirror — never Snapshot(), which is the
// PREVIOUS mirror at that point). Every tenant_hostnames mutation —
// admin write or fleet control event — funnels through a mirror reload,
// so the cache can never be staler than the mirror it fronts. On rebuild
// failure the previous map keeps serving, matching the mirror's own
// keep-the-old-copy failure mode.
//
// Scope: HTTP host routing only — exactly lookupHTTP's row filters
// (active hostname, attached stack, live tenant). Mail-domain resolution
// (ResolveRecipient/AcceptMailDomain) keeps the per-request SQL path: it
// is low-volume and matches a different row set (unattached rows route
// mail; the zone fallback needs live SQL).
//
// It also carries the set of active nested INLET stacks (`<stack>/_tcp`,
// …) — the opt-in a hostname-routed connection needs before it may enter
// a stack (see DBResolver.resolveTCP). Same reload, same freshness: a
// stack activation funnels through a mirror reload like a hostname write.
type HostRouteCache struct {
	// snap is nil until the first successful Rebuild — lookup reports
	// not-ready and the resolver falls back to per-request SQL, which is
	// exactly the pre-cache behavior.
	snap atomic.Pointer[routeSnapshot]
}

// routeSnapshot is one reload's worth of routing facts, swapped whole so
// a hostname and the inlets of the stack it names are never from two
// different mirrors.
type routeSnapshot struct {
	hosts map[string]hostRouteRow
	// inlets holds inletKey(tenant, stack) for every active nested inlet
	// stack. nil ⇒ that query failed on this reload: hosts still serve
	// (HTTP routing must never hang on the inlet query), and inlet checks
	// fall back to per-request SQL.
	inlets map[string]struct{}
}

func inletKey(tenant, stack string) string { return tenant + "\x00" + stack }

// hostRouteRow is one active, attached tenant_hostnames row, reduced to
// what routing needs. Verified mirrors `verified_at IS NOT NULL`; the
// requireVerified policy (and the once-per-row unverified WARN) is
// applied at lookup time by the DBResolver so cached and SQL resolution
// share one policy path.
type hostRouteRow struct {
	Tenant   string
	Stack    string
	Verified bool
}

// NewHostRouteCache returns an empty, not-yet-ready cache.
func NewHostRouteCache() *HostRouteCache {
	return &HostRouteCache{}
}

// Rebuild replaces the map from one query against db — the same filters
// as the resolver's live lookupHTTP query, so cached resolution is
// row-for-row identical (the oracle test pins this). The new map is
// published only on success: a mid-build error leaves the previous map
// (or the not-ready state) serving.
func (c *HostRouteCache) Rebuild(db *sql.DB) error {
	rows, err := db.Query(
		`SELECT h.hostname, t.slug, h.stack, h.verified_at
		   FROM tenant_hostnames h
		   JOIN tenants t ON t.tenant_id = h.tenant_id
		  WHERE h.revoked_at IS NULL
		    AND h.stack != ''
		    AND t.revoked_at IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()

	fresh := make(map[string]hostRouteRow)
	for rows.Next() {
		var hostname, slug, stack string
		var verifiedAt sql.NullString
		if err := rows.Scan(&hostname, &slug, &stack, &verifiedAt); err != nil {
			return err
		}
		// Hostnames are stored canonicalized (writes go through
		// tenants.CanonicalizeHost) and the partial unique index keeps
		// one active row per hostname, so plain assignment is exact.
		fresh[hostname] = hostRouteRow{
			Tenant:   slug,
			Stack:    stack,
			Verified: verifiedAt.Valid,
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	inlets, ierr := loadInletStacks(db)
	c.snap.Store(&routeSnapshot{hosts: fresh, inlets: inlets})
	if ierr != nil {
		return fmt.Errorf("inlet stacks (hosts published; inlet checks fall back to SQL): %w", ierr)
	}
	return nil
}

// loadInletStacks returns every active nested inlet stack — a name of
// the form `<stack>/_<inlet>` with an active version, under a live
// tenant. The LIKE is a coarse prefilter (its `_` is a wildcard); the
// exact name is matched at lookup.
func loadInletStacks(db *sql.DB) (map[string]struct{}, error) {
	rows, err := db.Query(
		`SELECT t.slug, s.name
		   FROM stacks s
		   JOIN tenants t ON t.tenant_id = s.tenant_id
		  WHERE s.active_version IS NOT NULL
		    AND s.name LIKE '%/_%'
		    AND t.revoked_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var slug, name string
		if err := rows.Scan(&slug, &name); err != nil {
			return nil, err
		}
		out[inletKey(slug, name)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// lookup returns the cached row for a canonical hostname. ready=false
// means no successful Rebuild has happened yet — the caller must use its
// SQL path; found=false with ready=true is an authoritative miss (the
// caller must NOT fall back to SQL, or the hot path regains the mirror
// dependency the cache exists to remove).
func (c *HostRouteCache) lookup(canonical string) (row hostRouteRow, found, ready bool) {
	s := c.snap.Load()
	if s == nil {
		return hostRouteRow{}, false, false
	}
	row, found = s.hosts[canonical]
	return row, found, true
}

// inletActive reports whether tenant has an active stack named stack
// (an inlet such as `shop/_tcp`). ready=false ⇒ use the SQL path.
func (c *HostRouteCache) inletActive(tenant, stack string) (active, ready bool) {
	s := c.snap.Load()
	if s == nil || s.inlets == nil {
		return false, false
	}
	_, active = s.inlets[inletKey(tenant, stack)]
	return active, true
}
