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
// handled per the chassis-level requireVerified flag. The key
// difference is the route SHAPE — we synthesize `<slug>/_mail`
// regardless of `h.stack`, since `h.stack` is the HTTP-bound stack
// and may be empty for mail-only deployments.
func (r *DBResolver) lookupMailDomain(db *sql.DB, canonical string) (RouteTarget, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var slug string
	var verifiedAt sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT t.slug, h.verified_at
		   FROM tenant_hostnames h
		   JOIN tenants t ON t.tenant_id = h.tenant_id
		  WHERE h.hostname = ?
		    AND h.revoked_at IS NULL
		    AND t.revoked_at IS NULL`, canonical).Scan(&slug, &verifiedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
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
	return RouteTarget{
		Tenant:   slug,
		Stack:    slug + "/_mail",
		Ingress:  "domain:" + canonical,
		Verified: verifiedAt.Valid,
	}, true
}

// Resolve implements Resolver. YAML wins for backward compatibility;
// the DB lookup is consulted only on YAML miss and only for HTTP
// sources (the `tenant_hostnames` table doesn't carry TCP listeners
// or cron jobs).
func (r *DBResolver) Resolve(key RouteKey) (RouteTarget, bool) {
	if r.inner != nil {
		if t, ok := r.inner.Resolve(key); ok {
			return t, true
		}
	}
	if r.dbFn == nil || key.Src != "http" || key.Hostname == "" {
		return RouteTarget{}, false
	}
	db := r.dbFn() // current mirror, post any reload
	if db == nil {
		return RouteTarget{}, false
	}
	canon, ok := canonicalizeHost(key.Hostname)
	if !ok {
		return RouteTarget{}, false
	}
	target, ok := r.lookupHTTP(db, canon)
	if !ok {
		return RouteTarget{}, false
	}
	return target, true
}

// lookupHTTP runs the resolver query against the dbcache mirror. One
// row at most thanks to the partial unique index on hostname WHERE
// revoked_at IS NULL. The 250ms timeout is defensive — in practice the
// in-memory mirror returns in microseconds, but bounding the query
// stops a pathological lock event from stalling the bus loop.
//
// `verified_at` decides routing alongside the chassis-level
// `requireVerified` flag:
//   - requireVerified=false (permissive default): unverified rows
//     still route; emit a once-per-row WARN.
//   - requireVerified=true (production): unverified rows miss.
func (r *DBResolver) lookupHTTP(db *sql.DB, canonical string) (RouteTarget, bool) {
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
		return RouteTarget{}, false
	case err != nil:
		// Don't crash the data plane on a transient cache error — log
		// and miss. The bus falls back to boot/%/0, same as any other
		// unmatched route.
		r.logger.Warn("ingress db lookup failed",
			zap.String("hostname", canonical),
			zap.Error(err))
		return RouteTarget{}, false
	}
	if !verifiedAt.Valid {
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
		Verified: verifiedAt.Valid,
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
