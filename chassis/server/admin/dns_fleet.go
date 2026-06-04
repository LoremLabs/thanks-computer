package admin

// Fleet propagation for delegated-zone routing hostnames.
//
// A pattern-mode delegated zone auto-mints a verified tenant_hostnames row
// `<StackLabel(stack)>.<origin>` per active stack (tenants.EnsureZoneHostnameTx,
// created_by = SystemZoneHostCreatedBy). That row is what a chassis routes on
// AND what the on-demand-TLS `ask` gate checks — so EVERY node behind the LB
// needs it, not just the admin node.
//
// The dns_zones row itself is NOT fleet-synced yet (see dns_crud_endpoints.go's
// "Fleet note"), so a data-plane node replaying a stack.activated event can't
// re-derive the delegated-zone host — its local mint sees no zone and falls back
// to the structured-host suffix instead. The fix: ship the minted row directly,
// the same way explicit hostname CRUD does (fleet_resync.go / tenant_hostname_
// endpoints.go) — a content-addressed RowsArtifact upsert that the consumer's
// applyRows writes verbatim (id-stable, so later upserts stay idempotent).
//
// Two producers call queueZoneHostnameUpserts: zone create (reconcile every
// already-active stack) and stack activation (propagate the one just minted).

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// zoneHostTSLayout matches the RFC3339-UTC text the tenant_hostnames row
// serializer (hostnameToRow) emits, so a minted-then-published row round-trips
// to the consumer byte-identically.
const zoneHostTSLayout = "2006-01-02T15:04:05Z"

// activeMintableStacks returns the tenant's active, non-system stack names —
// the set that gets a `<label>.<origin>` host synthesized + routed. Read from
// the passed tx so just-committed-in-tx state is visible. Mirrors the dns
// head's synthesis filter (isSynthesizableStack) via isMintableStack.
func (c *Controller) activeMintableStacks(ctx context.Context, tx *sql.Tx, tenantID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name FROM stacks WHERE tenant_id = ? AND active_version IS NOT NULL`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if isMintableStack(name) {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// reconcileZoneHostnames mints the routing host for every active stack of the
// tenant under origin (idempotent), then fleet-publishes them. Called from zone
// create so a zone created AFTER its stacks were activated still wires them up —
// the activation-time mint only fires when the zone already exists. A single
// mint failure is logged and skipped (it must never fail the zone create, like
// the activation path); a fleet-publish failure is returned (atomic with the tx).
func (c *Controller) reconcileZoneHostnames(ctx context.Context, tx *sql.Tx, tenantID, origin string) error {
	now := time.Now().UTC().Format(zoneHostTSLayout)
	stacks, err := c.activeMintableStacks(ctx, tx, tenantID)
	if err != nil {
		return fmt.Errorf("load active stacks: %w", err)
	}
	for _, s := range stacks {
		if _, merr := tenants.EnsureZoneHostnameTx(ctx, tx, tenantID, s, origin, now); merr != nil {
			c.pu.Logger.Warn("zone-create reconcile: hostname mint skipped (zone create unaffected)",
				zap.String("tenant", tenantID), zap.String("stack", s),
				zap.String("origin", origin), zap.String("err", merr.Error()))
		}
	}
	return c.queueZoneHostnameUpserts(ctx, tx, tenantID, "")
}

// queueZoneHostnameUpserts fleet-publishes the tenant's delegated-zone routing
// hostnames (created_by = SystemZoneHostCreatedBy) as TypeHostnameBound row
// upserts — all of them, or just one stack's when stack != "". Rows are read
// from tx so a same-tx mint is visible. No-op when fleet sync is off.
//
// Artifact-before-outbox ordering holds: the Put precedes the in-tx outbox
// append, so an accepted DB mutation never lacks its artifact (a Put whose tx
// later rolls back just orphans the artifact, which the sweeper GCs).
func (c *Controller) queueZoneHostnameUpserts(ctx context.Context, tx *sql.Tx, tenantID, stack string) error {
	if !c.fleetEnabled() {
		return nil
	}
	q := `SELECT id, hostname, tenant_id, stack, created_at, created_by, verified_at
	        FROM tenant_hostnames
	       WHERE tenant_id = ? AND created_by = ? AND revoked_at IS NULL`
	args := []any{tenantID, tenants.SystemZoneHostCreatedBy}
	if stack != "" {
		q += ` AND stack = ?`
		args = append(args, stack)
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	type hostRow struct {
		id, hostname, tenantID, stack, createdAt, createdBy string
		verifiedAt                                          sql.NullString
	}
	var collected []hostRow
	for rows.Next() {
		var h hostRow
		if err := rows.Scan(&h.id, &h.hostname, &h.tenantID, &h.stack,
			&h.createdAt, &h.createdBy, &h.verifiedAt); err != nil {
			rows.Close()
			return err
		}
		collected = append(collected, h)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, h := range collected {
		row := map[string]any{
			"id":         h.id,
			"hostname":   h.hostname,
			"tenant_id":  h.tenantID,
			"stack":      h.stack,
			"created_at": h.createdAt,
			"created_by": h.createdBy,
		}
		if h.verifiedAt.Valid && h.verifiedAt.String != "" {
			row["verified_at"] = h.verifiedAt.String
		}
		art := controlevent.RowsArtifact{
			DB:    "runtime",
			Table: "tenant_hostnames",
			Op:    "upsert",
			Rows:  []map[string]any{row},
		}
		ref, sum, _, err := c.fleetUploadArtifact(ctx, fmt.Sprintf("rows/tenant_hostnames/%s", h.id), art)
		if err != nil {
			return fmt.Errorf("upload zone hostname %s: %w", h.id, err)
		}
		if _, err := c.fleetQueueEvent(ctx, tx,
			controlevent.TypeHostnameBound, h.tenantID, "", 0, 0, ref, sum); err != nil {
			return fmt.Errorf("queue zone hostname %s: %w", h.id, err)
		}
	}
	return nil
}
