package ipp

import (
	"context"
	"database/sql"
	"errors"

	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// lookupTenant answers "whose is `ipp.<x>`?":
//
//  1. x is EXACTLY the origin of an active, verified zone delegated to this
//     chassis → that zone's tenant. Exact: `ipp.sub.dripl.it` is not
//     `ipp.dripl.it`, even though the dripl.it zone covers both names.
//  2. else x is a verified hostname bound to a stack → that hostname's
//     tenant (the stack is ignored; print jobs are tenant-level). This is
//     what makes `ipp.localhost` work under `txco dev`, and it covers a
//     custom domain that is a hostname row rather than a delegated zone.
//
// Always reads the LIVE dbcache mirror via Snapshot() — never a captured
// handle, which goes stale at the next reload.
func (c *Controller) lookupTenant(ctx context.Context, x string) (slug, ingressKey string, ok bool, err error) {
	if c.pu != nil && c.pu.Dbc != nil {
		if db := c.pu.Dbc.Snapshot(); db != nil {
			qctx, cancel := context.WithTimeout(ctx, lookupTimeout)
			s, origin, found, zerr := tenants.TenantForZone(qctx, db, x, nil)
			cancel()
			if zerr != nil {
				return "", "", false, zerr
			}
			if found && origin == x {
				return s, "zone:" + x, true, nil
			}
		}
	}
	if c.resolver == nil {
		return "", "", false, nil
	}
	t, found, rerr := c.resolver.ResolveErr(ingress.RouteKey{Src: "http", Hostname: x})
	if rerr != nil || !found {
		return "", "", false, rerr
	}
	if !t.Verified {
		// A name that could not get a certificate must not route a print
		// job either — the tls-ask rule is strict the same way.
		return "", "", false, nil
	}
	return t.Tenant, "host:" + x, true, nil
}

// snapshotSubscribed asks the mirror whether the tenant has an active
// `_ipp` stack (the imap head's subscription query, for `_ipp`).
func (c *Controller) snapshotSubscribed(ctx context.Context, slug string) (bool, error) {
	if c.pu == nil || c.pu.Dbc == nil {
		return false, nil
	}
	db := c.pu.Dbc.Snapshot()
	if db == nil {
		return false, nil
	}
	qctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	var one int
	err := db.QueryRowContext(qctx, `SELECT 1 FROM stacks s
	                       JOIN tenants t ON t.tenant_id = s.tenant_id
	                      WHERE t.slug = ? AND t.revoked_at IS NULL
	                        AND s.name = ? AND s.active_version IS NOT NULL
	                      LIMIT 1`, slug, SubscriptionStack).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// printerSite is a resolved, ENABLED tenant: it has a zone/host, an active
// `_ipp` stack and a credential. Anything short of that is "not found".
type printerSite struct {
	tenant     string
	ingressKey string
	username   string
	password   []byte
}

// site resolves a target to an enabled tenant. ok=false with a nil error is
// the fail-closed 404 — and it is ONE answer for every reason (no such
// zone, unverified, a reserved tenant, no `_ipp` stack, no IPP_PASSWORD), so
// a prober learns nothing about which tenants exist or what they run.
func (c *Controller) site(ctx context.Context, t target) (printerSite, bool, error) {
	slug, key, ok, err := c.tenantFor(ctx, t.x)
	if err != nil || !ok {
		return printerSite{}, false, err
	}
	if tenants.ReservedSlug(slug) {
		return printerSite{}, false, nil
	}
	sub, err := c.subscribed(ctx, slug)
	if err != nil || !sub {
		return printerSite{}, false, err
	}
	if c.secret == nil {
		return printerSite{}, false, nil
	}
	pw, found, err := c.secret(ctx, slug, SecretPassword)
	if err != nil || !found || len(pw) == 0 {
		return printerSite{}, false, err
	}
	user := DefaultUsername
	if u, found, uerr := c.secret(ctx, slug, SecretUsername); uerr != nil {
		return printerSite{}, false, uerr
	} else if found && len(u) > 0 {
		user = string(u)
	}
	return printerSite{tenant: slug, ingressKey: key, username: user, password: pw}, true, nil
}
