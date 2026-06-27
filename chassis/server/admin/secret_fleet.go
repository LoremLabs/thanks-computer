package admin

// Producer-side fleet-sync for the per-tenant secret store. The admin
// Controller implements secrets.Syncer (registered on the Store via
// SetSyncer during wiring); the secrets package calls these from its write
// paths so a create/rotate/revoke fans out to every fleet node.
//
// Shape mirrors dns_fleet.go's fleetPublishZone: build a RowsArtifact,
// fleetUploadArtifact (pre-tx, network) + fleetQueueEvent (in-tx outbox).
// The artifacts are id-keyed so a publish retry overwrites identically
// (same bytes, same checksum) — same idiom as dns_zones. The version row
// is version_id-keyed (immutable per rotation); the parent is
// secret_id-keyed (rewritten on rotate/describe/revoke).
//
// All gated on fleetEnabled(): single-node chassis return a nil in-tx
// closure and the Store skips every byte of this.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

// PublishSecretUpsert implements secrets.Syncer for create / rotate /
// description changes. versionRow is nil for a parent-only change. The
// version event is queued BEFORE the parent so a consumer never briefly
// sees a tenant_secrets row pointing at an absent version.
func (c *Controller) PublishSecretUpsert(
	ctx context.Context, tenantID string, versionRow, parentRow map[string]any,
) (func(*sql.Tx) error, error) {
	if !c.fleetEnabled() {
		return nil, nil
	}

	type queued struct{ ref, sum string }
	var events []queued

	if versionRow != nil {
		vid, _ := versionRow["version_id"].(string)
		art := controlevent.RowsArtifact{
			DB: "runtime", Table: "tenant_secret_versions", Op: "upsert",
			Rows: []map[string]any{versionRow},
		}
		ref, sum, _, err := c.fleetUploadArtifact(ctx,
			fmt.Sprintf("rows/tenant_secret_versions/%s", vid), art)
		if err != nil {
			return nil, fmt.Errorf("upload secret version %s: %w", vid, err)
		}
		events = append(events, queued{ref, sum})
	}

	sid, _ := parentRow["secret_id"].(string)
	pArt := controlevent.RowsArtifact{
		DB: "runtime", Table: "tenant_secrets", Op: "upsert",
		Rows: []map[string]any{parentRow},
	}
	pRef, pSum, _, err := c.fleetUploadArtifact(ctx,
		fmt.Sprintf("rows/tenant_secrets/%s", sid), pArt)
	if err != nil {
		return nil, fmt.Errorf("upload secret parent %s: %w", sid, err)
	}
	events = append(events, queued{pRef, pSum})

	return func(tx *sql.Tx) error {
		for _, e := range events {
			if _, err := c.fleetQueueEvent(ctx, tx,
				controlevent.TypeSecretChanged, tenantID, "", 0, 0, e.ref, e.sum); err != nil {
				return fmt.Errorf("queue secret event: %w", err)
			}
		}
		return nil
	}, nil
}

// PublishSecretRevoke implements secrets.Syncer for a revoke: one
// tenant_secrets upsert carrying revoked_at (the consumer's INSERT OR
// REPLACE flips it inactive), as a TypeSecretRevoked event.
func (c *Controller) PublishSecretRevoke(
	ctx context.Context, tenantID string, parentRow map[string]any,
) (func(*sql.Tx) error, error) {
	if !c.fleetEnabled() {
		return nil, nil
	}
	sid, _ := parentRow["secret_id"].(string)
	art := controlevent.RowsArtifact{
		DB: "runtime", Table: "tenant_secrets", Op: "upsert",
		Rows: []map[string]any{parentRow},
	}
	ref, sum, _, err := c.fleetUploadArtifact(ctx,
		fmt.Sprintf("rows/tenant_secrets/%s", sid), art)
	if err != nil {
		return nil, fmt.Errorf("upload secret revoke %s: %w", sid, err)
	}
	return func(tx *sql.Tx) error {
		if _, err := c.fleetQueueEvent(ctx, tx,
			controlevent.TypeSecretRevoked, tenantID, "", 0, 0, ref, sum); err != nil {
			return fmt.Errorf("queue secret revoke: %w", err)
		}
		return nil
	}, nil
}
