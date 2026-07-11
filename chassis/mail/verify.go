package mail

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// fromDomainVerified reports whether `domain` is a valid sender domain for
// tenant `slug`: either a non-revoked hostname with verified_at set, OR a
// domain we serve authoritative DNS for (an active dns_zones row — delegation
// is itself proof of control, so no separate verify step). Anti-spoof guard:
// a tenant may only send as a domain it owns. Reads the mirror snapshot (see
// readDB) — these tables are fully mirrored and this runs on every send.
func (m *Mailer) fromDomainVerified(ctx context.Context, slug, domain string) (bool, error) {
	if slug == "" || domain == "" {
		return false, nil
	}
	db, dia := m.readDB()
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
