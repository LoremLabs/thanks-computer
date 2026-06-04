package admin

import (
	"context"
	"database/sql"
	"errors"
)

// SetGate engages or releases the programmatic admission gate for a tenant,
// identified by slug — the identity background services see in usage data (the
// envelope `_txc.tenant`). It owns the `suspended` column exclusively (operator
// disable owns `enabled`) and routes through applyRuntimeRow so the full-row
// write + fleet emit + dbcache reload is reused, never a partial row. Unknown
// or revoked slugs are a no-op (the caller logs). This makes *Controller
// satisfy bgservice.Gate, which is injected into background services at boot.
func (c *Controller) SetGate(ctx context.Context, slug string, suspended bool, denyStatus int, denyReason string) error {
	var tenantID string
	err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT tenant_id FROM tenants WHERE slug = ? AND revoked_at IS NULL`, slug).Scan(&tenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // unknown/revoked tenant: nothing to gate
	}
	if err != nil {
		return err
	}

	rr, err := c.loadRuntimeRow(ctx, tenantID)
	if err != nil {
		return err
	}
	if suspended {
		rr.Suspended, rr.DenyStatus, rr.DenyReason = 1, denyStatus, denyReason
	} else {
		rr.Suspended = 0
		rr.clearDenyIfOpen()
	}
	return c.applyRuntimeRow(ctx, tenantID, rr)
}
