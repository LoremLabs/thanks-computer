package admin

// Producer-side fleet-sync helpers. These are called from existing
// admin mutation handlers (handleActivateStack today; P5 hooks the
// others) to upload an event artifact + queue an outbox row in the
// same SQLite tx as the mutation.
//
// Wire contract (see fleet-sync-contract.md and the overlay-repo
// design doc todo-fleet-sync-producer.md):
//
//  1. The handler computes the artifact JSON.
//  2. fleetUploadArtifact uploads it to astore BEFORE opening the
//     tx. Returns (artifact_ref, checksum). Orphan artifacts (upload
//     succeeds, tx never commits) are tolerated; the artifact
//     sweeper (P5) GCs them. Accepted DB mutations without an
//     artifact are NOT tolerated — so the ordering matters.
//  3. The handler opens its tx, performs its mutation, calls
//     fleetQueueEvent inside the tx (writes the outbox row), and
//     commits.
//  4. The background pump (chassis/controlpublish) picks it up.
//
// All of this is gated on FeedSink != nop: single-node deployments
// skip every byte of producer-side work.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/controlpublish"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// fleetEnabled reports whether the producer pipeline should run for
// this chassis. False ⇒ skip artifact upload + outbox write entirely.
func (c *Controller) fleetEnabled() bool {
	s := c.pu.Conf.FeedSink
	return s != "" && s != "nop" && c.astore != nil
}

// fleetUploadArtifact serializes payload to JSON and uploads it to
// the artifact store. Returns (artifact_ref, checksum, jsonBytes).
// The bytes are returned so the caller can store them in the outbox
// payload_json column without re-marshalling.
func (c *Controller) fleetUploadArtifact(ctx context.Context, key string, payload any) (string, string, []byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", "", nil, fmt.Errorf("marshal artifact: %w", err)
	}
	// Empty manifest blob: the artifact store API takes one (used by
	// some backends for sidecar metadata); we don't need it here.
	if err := c.astore.Put(ctx, key, data, []byte(`{}`)); err != nil {
		return "", "", nil, fmt.Errorf("put artifact %q: %w", key, err)
	}
	return key, "sha256:" + sha256Hex(string(data)), data, nil
}

// readStackFilesForArtifact loads (path, content) for the named
// stack's target version. Read outside the activation tx — stack
// versions are immutable per the contract, so the file contents are
// race-free. An empty result is not an error here; the artifact
// will just have an empty Files slice (the applier upserts zero
// rows, which is OK if the same chassis later receives a non-empty
// version event for the same version_number — but in practice we
// only emit when materialiseStackVersion has files to work with).
func (c *Controller) readStackFilesForArtifact(
	ctx context.Context, tenantID, stackName string, versionNumber int64,
) ([]controlevent.StackArtifactFile, error) {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx, `
		SELECT sf.path, sf.content
		  FROM stack_files sf
		  JOIN stack_versions sv ON sf.version_id = sv.version_id
		  JOIN stacks s          ON sv.stack_id = s.stack_id
		 WHERE s.tenant_id = ?
		   AND s.name = ?
		   AND sv.version_number = ?
		 ORDER BY sf.path`,
		tenantID, stackName, versionNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []controlevent.StackArtifactFile
	for rows.Next() {
		var f controlevent.StackArtifactFile
		if err := rows.Scan(&f.Path, &f.Content); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// currentActiveVersionNumber returns the active_version of the named
// stack as the producer's view of base_version. Best-effort: 0 on
// any error or unset value. Not authoritative for CAS (recorded only).
func (c *Controller) currentActiveVersionNumber(
	ctx context.Context, tenantID, stackName string,
) int64 {
	var av sql.NullInt64
	_ = c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT active_version FROM stacks WHERE tenant_id = ? AND name = ?`,
		tenantID, stackName).Scan(&av)
	if av.Valid {
		return av.Int64
	}
	return 0
}

// fleetQueueEvent writes the outbox row in the same tx as the
// mutation. event_id is the producer-assigned UUID — generated here
// because it has to be both the outbox row key AND part of the
// canonical Event JSON stamped in payload_json. The Sink later uses
// the same event_id as its idempotent-publish key (Nats-Msg-Id on
// JetStream).
func (c *Controller) fleetQueueEvent(
	ctx context.Context, tx *sql.Tx,
	eventType, tenantID, stackID string,
	version, baseVersion int64,
	artifactRef, checksum string,
) (string, error) {
	eventID := "evt_" + hxid.NewTimeSort().String()
	ev := controlevent.Event{
		EventID:     eventID,
		Type:        eventType,
		TenantID:    tenantID,
		StackID:     stackID,
		Version:     version,
		BaseVersion: baseVersion,
		ArtifactRef: artifactRef,
		Checksum:    checksum,
		// ControlVersion stays 0 — broker assigns it at publish time.
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}
	if err := controlpublish.AppendOutbox(ctx, tx,
		eventID, eventType, tenantID, stackID, version, baseVersion,
		artifactRef, checksum, payload); err != nil {
		return "", fmt.Errorf("append outbox: %w", err)
	}
	return eventID, nil
}

// fleetUploadHostnameUpsert builds a RowsArtifact targeting
// runtime.tenant_hostnames with op=upsert, marshals the row from a
// Hostname struct, and uploads via fleetUploadArtifact. The artifact
// key includes the hostname row id so retries are idempotent at the
// store layer (same bytes overwrite the same key). Returns
// (artifact_ref, checksum).
func (c *Controller) fleetUploadHostnameUpsert(
	ctx context.Context, h tenants.Hostname,
) (string, string, error) {
	row := hostnameToRow(h)
	art := controlevent.RowsArtifact{
		DB:    "runtime",
		Table: "tenant_hostnames",
		Op:    "upsert",
		Rows:  []map[string]any{row},
	}
	key := fmt.Sprintf("rows/tenant_hostnames/%s", h.ID)
	ref, sum, _, err := c.fleetUploadArtifact(ctx, key, art)
	if err != nil {
		return "", "", err
	}
	return ref, sum, nil
}

// hostnameToRow projects a Hostname onto the JSON-row shape the
// consumer applier uses for RowsArtifact upserts. Mirrors the
// column set of the tenant_hostnames table (migrations 0004 + 0005).
// Optional timestamp fields are omitted when zero so the consumer's
// INSERT OR REPLACE doesn't write empty strings where NULLs belong.
func hostnameToRow(h tenants.Hostname) map[string]any {
	row := map[string]any{
		"id":         h.ID,
		"hostname":   h.Hostname,
		"tenant_id":  h.TenantID,
		"stack":      h.Stack,
		"created_at": h.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if h.CreatedBy != "" {
		row["created_by"] = h.CreatedBy
	}
	if h.RevokedAt != nil {
		row["revoked_at"] = h.RevokedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if h.VerifiedAt != nil {
		row["verified_at"] = h.VerifiedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	return row
}
