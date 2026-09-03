package mail

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// fromDomainVerified reports whether `domain` is a valid sender domain for
// tenant `slug`: either a non-revoked hostname with verified_at set, OR a
// domain we serve authoritative DNS for (an active dns_zones row — delegation
// is itself proof of control, so no separate verify step). Anti-spoof guard:
// a tenant may only send as a domain it owns. Reads the mirror snapshot (see
// readDB) — these tables are fully mirrored and this runs on every send.
func (m *Mailer) fromDomainVerified(ctx context.Context, slug, domain string) (bool, error) {
	db, dia := m.readDB()
	return DomainOwnedByTenant(ctx, db, dia, slug, domain)
}

// DomainOwnedByTenant is the single ownership rule for "may tenant `slug`
// act as `domain`": a non-revoked tenant_hostnames row with verified_at
// set, OR a domain the chassis serves authoritative DNS for (an active
// dns_zones row — delegation is itself proof of control). Applied by
// txco://sendmail (the From: guard) and by txco://imap/account (the
// username's domain); package-level so both read the same tables the same
// way. db is normally the dbcache mirror snapshot (dialect SQLite).
func DomainOwnedByTenant(ctx context.Context, db *sql.DB, dia registry.Dialect, slug, domain string) (bool, error) {
	if slug == "" || domain == "" || db == nil {
		return false, nil
	}
	if dia == nil {
		dia = registry.SQLite
	}
	var verifiedAt sql.NullString
	err := db.QueryRowContext(ctx,
		dia.Rebind(`SELECT h.verified_at
		   FROM tenant_hostnames h
		   JOIN tenants t ON t.tenant_id = h.tenant_id
		  WHERE h.hostname = ? AND t.slug = ?
		    AND h.revoked_at IS NULL AND t.revoked_at IS NULL`),
		domain, slug).Scan(&verifiedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no hostname row — fall through to the DNS-zone check
	case err != nil:
		return false, err
	default:
		if verifiedAt.Valid && verifiedAt.String != "" {
			return true, nil
		}
	}
	// We serve DNS for this domain (apex or subdomain) ⟹ verified.
	return tenants.DomainCoveredByZone(ctx, db, slug, domain, dia)
}

// domainOf extracts the lowercased domain from a bare email address
// ("user@host" → "host"). Returns "" when there is no usable domain.
func domainOf(bareAddr string) string {
	at := strings.LastIndex(bareAddr, "@")
	if at < 0 || at == len(bareAddr)-1 {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(bareAddr[at+1:]))
}
