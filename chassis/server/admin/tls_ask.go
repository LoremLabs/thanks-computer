package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// tlsAskPath is Caddy's on_demand_tls `ask` hook (global option in the
// Caddyfile points here). Kept as a constant so the chassis route and
// the Caddy config can't drift.
const tlsAskPath = "/_txc/tls-ask"

// tlsAskTimeout bounds the in-memory lookup. It's on the TLS-handshake
// path (Caddy blocks the handshake on this call), so fail fast — the
// dbcache mirror answers in microseconds; this only guards a
// pathological stall.
const tlsAskTimeout = 250 * time.Millisecond

// handleTLSAsk authorizes (or denies) on-demand certificate issuance
// for customer-owned domains — see internal docs/todo-custom-domains.md. Caddy
// calls `GET /_txc/tls-ask?domain=<sni>` on the first TLS handshake for
// an otherwise-unknown host; a 2xx authorizes issuance, anything else
// denies it (Caddy then serves no cert → the handshake fails, which is
// the correct outcome for an unproven domain).
//
// Loopback-only by deployment: the admin server is published on host
// loopback and Caddy dials 127.0.0.1; the response is only a yes/no
// with no secrets, so it needs no auth (registered alongside /healthz,
// outside the auth middleware).
//
// Fail closed. A cert is authorized ONLY for an ACTIVE, VERIFIED,
// stack-attached, non-revoked tenant_hostnames row whose tenant is
// live — i.e. a domain whose owner completed the shipped
// claim→challenge→/verify→/attach flow. Reads the LIVE dbcache mirror
// via Snapshot(); a captured handle would reintroduce the stale-mirror
// routing bug.
func (c *Controller) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	domain := strings.TrimSpace(r.URL.Query().Get("domain"))
	if domain == "" {
		http.Error(w, "missing domain", http.StatusBadRequest)
		return
	}
	canon, ok := tenants.CanonicalizeHost(domain)
	if !ok || !tenants.IsValidHostname(canon) {
		http.Error(w, "deny", http.StatusNotFound)
		return
	}
	// Names under the structured-host apex (*.stacks…) are served by
	// the single wildcard DNS-01 cert, never on-demand — never mint a
	// per-host cert for them even though they carry verified rows.
	if suf := c.pu.Conf.StructuredHostSuffix; suf != "" && strings.HasSuffix(canon, suf) {
		http.Error(w, "deny", http.StatusNotFound)
		return
	}

	db := c.pu.Dbc.Snapshot()
	if db == nil {
		http.Error(w, "deny", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), tlsAskTimeout)
	defer cancel()

	var one int
	// Same predicate the resolver routes on, PLUS verified_at is
	// mandatory here regardless of the resolver's permissive/strict
	// flag: a cert must never be issued for an unproven domain.
	err := db.QueryRowContext(ctx,
		`SELECT 1
		   FROM tenant_hostnames h
		   JOIN tenants t ON t.tenant_id = h.tenant_id
		  WHERE h.hostname = ?
		    AND h.revoked_at IS NULL
		    AND h.stack != ''
		    AND h.verified_at IS NOT NULL
		    AND t.revoked_at IS NULL
		  LIMIT 1`, canon).Scan(&one)
	if err != nil {
		// sql.ErrNoRows or any transient error → deny (fail closed).
		http.Error(w, "deny", http.StatusNotFound)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
