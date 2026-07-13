package ingress

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// DBResolver wraps an inner Resolver (typically a yamlResolver) with a
// DB-backed hostname → tenant lookup. The inner Resolver is consulted
// first so existing YAML deployments keep their behavior; the DB lookup
// is the fallback for hostnames the YAML doesn't know about.
//
// Lookups go against the dbcache mirror (`*sql.DB` pinned to one
// connection, in-memory) so the data-plane hot path stays in-process.
// Admin writes to the on-disk runtime.db trigger a synchronous reload
// (see chassis/server/admin/tenant_hostname_endpoints.go) so changes
// are visible on the next request.
//
// The resolver does NOT import chassis/tenants; the host-parsing rules
// are duplicated here (canonicalizeHost) to keep the data-plane router
// independent of the admin package surface. Both implementations must
// stay in sync — the production wiring routes both writes and reads
// through the same canonical form.
type DBResolver struct {
	inner Resolver
	// dbFn returns the CURRENT dbcache mirror handle, evaluated per
	// Resolve. It must NOT be a captured *sql.DB: dbcache.Reload()
	// swaps dbc.Db to a brand-new :memory: DB on every reload, so a
	// fixed handle would only ever see rows present at chassis boot —
	// any hostname added afterwards (operator `hostnames add` or an
	// auto-minted structured host) would never route until restart.
	dbFn            func() *sql.DB
	logger          *zap.Logger
	requireVerified bool

	// hostCache, when set (SetHostRouteCache), serves HTTP host→stack
	// lookups from the precomputed in-process map instead of a
	// per-request mirror query — see route_cache.go. Mail lookups keep
	// the SQL path. Set once at startup, before the resolver serves
	// traffic; not synchronized for later swaps.
	hostCache *HostRouteCache

	// warned tracks hostnames we've already emitted a "routing an
	// unverified hostname" WARN for, so a misconfigured row doesn't
	// spam the log on every request. Cleared on each chassis reboot.
	warned sync.Map // map[string]struct{}
}

// NewDBResolver builds a DBResolver. `inner` may be nil for chassis
// configured without an `--ingress-config` YAML; in that case the DB
// lookup is the only routing path. `db` should be the dbcache mirror
// handle (`dbc.Db`), not the on-disk runtime.db — admin mutations
// reload the mirror synchronously so the data plane sees them.
//
// requireVerified gates whether unverified `tenant_hostnames` rows
// participate in routing. Permissive (false): unverified rows still
// route, with a once-per-row WARN. Strict (true): unverified rows
// miss the resolver and the bus falls through to `boot/%/0`.
// NewDBResolver pins a FIXED *sql.DB handle. Correct for tests and any
// caller whose DB pointer is stable for the resolver's lifetime. The
// data plane must NOT use this with dbc.Db (that pointer is swapped on
// every dbcache reload) — use NewDBResolverFunc with dbc.Snapshot.
func NewDBResolver(inner Resolver, db *sql.DB, logger *zap.Logger, requireVerified bool) *DBResolver {
	return NewDBResolverFunc(inner, func() *sql.DB { return db }, logger, requireVerified)
}

// NewDBResolverFunc takes a provider that returns the CURRENT mirror
// handle, evaluated per request. The data plane passes dbc.Snapshot so
// hostnames added after boot (operator-bound or auto-minted) route
// without a restart — the bug this seam fixes.
func NewDBResolverFunc(inner Resolver, dbFn func() *sql.DB, logger *zap.Logger, requireVerified bool) *DBResolver {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &DBResolver{inner: inner, dbFn: dbFn, logger: logger, requireVerified: requireVerified}
}

// SetHostRouteCache installs the precomputed host→stack cache the HTTP
// resolve path reads instead of querying the mirror per request. Call
// once at startup before the resolver serves traffic.
func (r *DBResolver) SetHostRouteCache(c *HostRouteCache) { r.hostCache = c }

// ResolveRecipient implements MailResolver with Strategy B layered
// in. Resolution order, per `internal docs/todo-lmtp-routing-v2.md` §2.5:
//
//  1. Inner.resolveBeforeListener — rules 1, 2, 3, 4(YAML).
//     Operator overrides + Strategy A + YAML verified_domains.
//  2. THIS resolver's tenant_hostnames lookup — DB-backed rule 4.
//     `anything@<verified hostname>` → `<tenant>/_mail`.
//  3. Inner.resolveListener — rule 5 (operator listener catch-all).
//
// The split is load-bearing: a tenant-specific rule (Strategy B)
// must win over the operator's last-resort catch-all (rule 5),
// otherwise a verified tenant's mail gets blackholed into the
// `system/mail_drop` stack instead of their `<tenant>/_mail`.
//
// Inner type-assertion via concrete *yamlResolver: the bundled
// inner is always a *yamlResolver (or nil). Downstream overlays that
// want a different inner write their own resolver wrapper.
func (r *DBResolver) ResolveRecipient(rcpt, listener string) (RouteTarget, bool) {
	rcpt = strings.ToLower(rcpt)

	inner, _ := r.inner.(*yamlResolver)

	// Rules 1, 2, 3, 4(YAML)
	if inner != nil {
		if t, ok := inner.resolveBeforeListener(rcpt); ok {
			return t, true
		}
	}

	// Rule 4 (DB) — tenant_hostnames lookup. Only when we have a DB
	// handle AND the rcpt has a parseable domain.
	if r.dbFn != nil && rcpt != "" {
		if at := strings.LastIndex(rcpt, "@"); at >= 0 {
			domain := rcpt[at+1:]
			canon, ok := canonicalizeHost(domain)
			if ok {
				if db := r.dbFn(); db != nil {
					if t, ok := r.lookupMailDomain(db, canon); ok {
						return t, true
					}
				}
			}
		}
	}

	// Rule 5 (listener)
	if inner != nil {
		if t, ok := inner.resolveListener(listener); ok {
			return t, true
		}
	}
	return RouteTarget{}, false
}

// AcceptMailDomain implements MailDomainAccepter with Strategy B layered
// in — the same domain-acceptance decision as ResolveRecipient minus the
// local-part (Strategy A) and listener-catch-all rules:
//
//  1. Inner YAML domain rules (operator @domain override + verified_domains).
//  2. THIS resolver's tenant_hostnames lookup (DB-backed Strategy B).
//
// The mailmap head calls this to answer the edge Postfix's relay_domains
// lookup. It honors the SAME verified/requireVerified gating as routing
// (via lookupMailDomain), so Postfix only ACCEPTS mail for a domain the
// chassis would actually ROUTE — no point accepting mail destined to
// bounce at the LMTP head.
func (r *DBResolver) AcceptMailDomain(domain string) (RouteTarget, bool) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return RouteTarget{}, false
	}

	// Rule 1: inner YAML domain rules.
	inner, _ := r.inner.(*yamlResolver)
	if inner != nil {
		if t, ok := inner.AcceptMailDomain(domain); ok {
			return t, true
		}
	}

	// Rule 2: DB tenant_hostnames lookup.
	canon, ok := canonicalizeHost(domain)
	if !ok {
		return RouteTarget{}, false
	}
	if db := r.dbFn(); db != nil {
		if t, ok := r.lookupMailDomain(db, canon); ok {
			return t, true
		}
	}
	return RouteTarget{}, false
}

// lookupMailDomain is Strategy B's DB-backed half: query
// `tenant_hostnames` for an exact (canonical) hostname match and
// synthesize `<tenant>/_mail` as the route. The same authorization
// proof that lets a tenant route HTTP traffic to `acme.example`
// also authorizes mail to `anything@acme.example` — operationally
// the tenant still has to point MX records at the chassis or
// Postfix, but no separate verification flow is required (per
// `internal docs/todo-lmtp-routing-v2.md` §2.3).
//
// Subdomain matching is exact: a tenant who verifies `app.acme.example`
// (no apex) receives mail to `*@app.acme.example` but NOT to
// `*@acme.example`. Operators who want all subdomains verify the apex.
//
// Filters mirror lookupHTTP: revoked rows excluded; verified_at
// handled per the chassis-level requireVerified flag. The route SHAPE
// NESTS mail under the hostname's bound stack: `<h.stack>/_mail`, so one
// stack owns its hostname across HTTP + mail. A mail-only hostname
// (empty `h.stack`) falls back to the tenant-level `_mail`. The name is
// BARE (no tenant prefix) — the tenant rides RouteTarget.Tenant and
// OpsForStage scopes by tenant_id, so a tenant-prefixed stack name would
// never match the materialised (bare) ops rows.
func (r *DBResolver) lookupMailDomain(db *sql.DB, canonical string) (RouteTarget, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var slug string
	var hStack, verifiedAt sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT t.slug, h.stack, h.verified_at
		   FROM tenant_hostnames h
		   JOIN tenants t ON t.tenant_id = h.tenant_id
		  WHERE h.hostname = ?
		    AND h.revoked_at IS NULL
		    AND t.revoked_at IS NULL`, canonical).Scan(&slug, &hStack, &verifiedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No tenant_hostnames row. Fall back to DNS-zone coverage: if we
		// serve authoritative DNS for this domain (apex or subdomain), that
		// delegation is itself proof of control — route mail to the owning
		// tenant's <tenant>/_mail, verified. Longest-zone-match inside.
		// nil dialect ⇒ SQLite: db here is the dbcache snapshot, always a
		// SQLite :memory: mirror even when the runtime store is Postgres.
		if zslug, zok, zerr := tenants.TenantForMailZone(ctx, db, canonical, nil); zerr == nil && zok {
			return RouteTarget{
				Tenant:   zslug,
				Stack:    "_mail",
				Ingress:  "zone:" + canonical,
				Verified: true,
			}, true
		}
		return RouteTarget{}, false
	case err != nil:
		r.logger.Warn("ingress db mail lookup failed",
			zap.String("domain", canonical),
			zap.Error(err))
		return RouteTarget{}, false
	}
	if !verifiedAt.Valid {
		if r.requireVerified {
			return RouteTarget{}, false
		}
		// Same once-per-row WARN cadence as the HTTP path. Key the
		// dedup on "mail:<domain>" so an unverified row warns once
		// for HTTP AND once for mail (different concerns).
		warnKey := "mail:" + canonical
		if _, alreadyWarned := r.warned.LoadOrStore(warnKey, struct{}{}); !alreadyWarned {
			r.logger.Warn("routing mail to an unverified hostname; verify or enable --require-hostname-verification",
				zap.String("domain", canonical),
				zap.String("tenant", slug))
		}
	}
	// Nest under the hostname's bound stack; mail-only hostnames (no bound
	// stack) fall back to the tenant-level `_mail`. BARE name — tenant is
	// carried in RouteTarget.Tenant.
	stack := "_mail"
	if s := strings.TrimSpace(hStack.String); s != "" {
		stack = s + "/_mail"
	}
	return RouteTarget{
		Tenant:   slug,
		Stack:    stack,
		Ingress:  "domain:" + canonical,
		Verified: verifiedAt.Valid,
	}, true
}

// Resolve implements Resolver. YAML wins for backward compatibility;
// the DB lookup is consulted only on YAML miss and only for HTTP
// sources (the `tenant_hostnames` table doesn't carry TCP listeners
// or cron jobs). A transient lookup failure collapses to a miss here —
// callers that must distinguish a saturated mirror from a genuinely
// unknown hostname (the detect-tenant op, which serves an honest 503
// instead of fabricating a 404) use ResolveErr.
func (r *DBResolver) Resolve(key RouteKey) (RouteTarget, bool) {
	t, ok, _ := r.ResolveErr(key)
	return t, ok
}

// ResolveErr is Resolve with the failure channel exposed: err is
// non-nil ONLY for a transient lookup failure (mirror-connection
// contention blowing the query deadline, a mid-reload hiccup) — never
// for a genuine miss, which stays (zero, false, nil). Introduced for
// todo-route-resolution-404-under-load fix 2: under request
// concurrency the single-connection mirror was blowing the 250ms
// lookup deadline and the swallowed error mis-served real hostnames
// as 404s.
func (r *DBResolver) ResolveErr(key RouteKey) (RouteTarget, bool, error) {
	if r.inner != nil {
		if t, ok := r.inner.Resolve(key); ok {
			return t, true, nil
		}
	}
	if key.Src != "http" || key.Hostname == "" {
		return RouteTarget{}, false, nil
	}
	canon, ok := canonicalizeHost(key.Hostname)
	if !ok {
		return RouteTarget{}, false, nil
	}
	// Fast path (todo-route-resolution-404-under-load fix 1): the
	// precomputed host→stack map, rebuilt on every dbcache reload. An
	// O(1) in-process read — the hot path never queues on the mirror's
	// single connection, so request concurrency can't manufacture
	// lookup timeouts. A ready-cache miss is authoritative (same row
	// filters as the query below); before the first successful build
	// the cache reports not-ready and the SQL path keeps serving.
	if r.hostCache != nil {
		if row, found, ready := r.hostCache.lookup(canon); ready {
			if !found {
				return RouteTarget{}, false, nil
			}
			t, ok := r.shapeHTTPTarget(row.Tenant, row.Stack, row.Verified, canon)
			return t, ok, nil
		}
	}
	if r.dbFn == nil {
		return RouteTarget{}, false, nil
	}
	db := r.dbFn() // current mirror, post any reload
	if db == nil {
		return RouteTarget{}, false, nil
	}
	return r.lookupHTTP(db, canon)
}

// lookupHTTP runs the resolver query against the dbcache mirror. One
// row at most thanks to the partial unique index on hostname WHERE
// revoked_at IS NULL. The 250ms timeout is defensive — an idle
// in-memory mirror returns in microseconds, but the handle is pinned
// to ONE connection, so under request concurrency lookups queue and
// the deadline CAN fire (the todo-route-resolution-404-under-load
// incident). That failure is returned as an error, not conflated with
// a miss — see ResolveErr.
//
// `verified_at` decides routing alongside the chassis-level
// `requireVerified` flag — see shapeHTTPTarget.
func (r *DBResolver) lookupHTTP(db *sql.DB, canonical string) (RouteTarget, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var slug, stack string
	var verifiedAt sql.NullString
	// h.stack != '' filters out unattached rows: the Vercel-style
	// decoupled flow lets a tenant claim+verify a hostname without
	// binding it to a stack. Unattached rows participate in /verify
	// but never route — they fall through to boot/%/0 alongside
	// unverified rows under strict mode.
	err := db.QueryRowContext(ctx,
		`SELECT t.slug, h.stack, h.verified_at
		   FROM tenant_hostnames h
		   JOIN tenants t ON t.tenant_id = h.tenant_id
		  WHERE h.hostname = ?
		    AND h.revoked_at IS NULL
		    AND h.stack != ''
		    AND t.revoked_at IS NULL`, canonical).Scan(&slug, &stack, &verifiedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RouteTarget{}, false, nil
	case err != nil:
		// A transient failure is NOT a miss: we don't know whether the
		// hostname routes, so reporting "no route" here would mis-serve
		// a real site as a 404. Log and surface the error; Resolve()
		// collapses it to a miss for callers with no error channel,
		// detect-tenant turns it into a retryable 503.
		r.logger.Warn("ingress db lookup failed",
			zap.String("hostname", canonical),
			zap.Error(err))
		return RouteTarget{}, false, err
	}
	t, ok := r.shapeHTTPTarget(slug, stack, verifiedAt.Valid, canonical)
	return t, ok, nil
}

// shapeHTTPTarget applies the verified/requireVerified routing policy
// and shapes the RouteTarget. Shared by the cached and SQL lookup
// paths so both enforce one policy:
//   - requireVerified=false (permissive default): unverified rows
//     still route; emit a once-per-row WARN.
//   - requireVerified=true (production): unverified rows miss.
func (r *DBResolver) shapeHTTPTarget(slug, stack string, verified bool, canonical string) (RouteTarget, bool) {
	if !verified {
		if r.requireVerified {
			return RouteTarget{}, false
		}
		// First sight of this unverified row since boot — log once.
		if _, alreadyWarned := r.warned.LoadOrStore(canonical, struct{}{}); !alreadyWarned {
			r.logger.Warn("routing an unverified hostname; verify or enable --require-hostname-verification",
				zap.String("hostname", canonical),
				zap.String("tenant", slug),
				zap.String("stack", stack))
		}
	}
	return RouteTarget{
		Tenant:   slug,
		Stack:    stack,
		Ingress:  "host:" + canonical,
		Verified: verified,
	}, true
}

// canonicalizeHost mirrors chassis/tenants.CanonicalizeHost. Kept
// inline here so the ingress package doesn't import chassis/tenants
// (which would pull the admin-package layer into the data-plane
// router). Both implementations must agree on the canonical form
// because writes and reads use them as a shared lookup key.
//
// Rules: trim, SplitHostPort to peel the port (and IPv6 brackets when
// a port is present), strip surrounding brackets when no port,
// lowercase, drop one trailing dot. Bare IPv6 ("::1") and malformed
// multi-colon strings are rejected.
func canonicalizeHost(input string) (string, bool) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", false
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	} else if strings.Contains(err.Error(), "missing port") {
		if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
			s = s[1 : len(s)-1]
		}
	} else {
		return "", false
	}
	s = strings.ToLower(s)
	if strings.HasSuffix(s, ".") {
		s = s[:len(s)-1]
	}
	if s == "" {
		return "", false
	}
	return s, true
}
