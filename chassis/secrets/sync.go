package secrets

import (
	"context"
	"database/sql"
	"encoding/base64"
	"time"
)

// Syncer publishes secret-row changes to the fleet so every node's local
// SQLite copy converges. It is the producer seam: the chassis admin layer
// implements it (uploading RowsArtifacts to the artifact store + queuing
// outbox events), so the secrets package stays free of the control-plane
// wire types. A nil Syncer (the default) means single-node — the Store's
// write paths skip all of this.
//
// The two-part shape honours the fleet-sync contract — the artifact is
// uploaded BEFORE the write tx, and the outbox row is appended INSIDE it,
// atomically with the secret write (so an accepted mutation can never fail
// to propagate):
//   - the method does the pre-tx artifact upload and returns an in-tx
//     closure;
//   - the Store invokes that closure inside its write tx, before commit.
//
// Replicated ciphertext only decrypts on other nodes when the fleet shares
// one master key (TXCO_SECRET_MASTER_KEY_B64); see master_key.go.
type Syncer interface {
	// PublishSecretUpsert handles a create / rotate / description change.
	// versionRow is nil for a parent-only change (description); for a
	// create/rotate it is the new tenant_secret_versions row and MUST be
	// published before parentRow so a consumer never sees a parent pointing
	// at an absent version. parentRow is the full tenant_secrets row.
	PublishSecretUpsert(ctx context.Context, tenantID string, versionRow, parentRow map[string]any) (inTx func(*sql.Tx) error, err error)
	// PublishSecretRevoke handles a revoke: the full tenant_secrets row with
	// revoked_at set (the consumer's INSERT OR REPLACE flips it inactive).
	PublishSecretRevoke(ctx context.Context, tenantID string, parentRow map[string]any) (inTx func(*sql.Tx) error, err error)
}

// SetSyncer installs (or clears, with nil) the fleet syncer. Called once
// during admin wiring; nil = single-node, so every write path skips the
// producer work entirely.
func (s *Store) SetSyncer(sy Syncer) { s.syncer = sy }

// ResyncRow is one active secret's full fleet-sync payload: the parent
// tenant_secrets row and its active tenant_secret_versions row, as the
// {column: value} maps a RowsArtifact carries (blob columns base64-wrapped).
// Cleartext is never produced — the encrypted blobs travel as-is. Used by
// the admin resync path to re-emit current state to lagging/new nodes.
type ResyncRow struct {
	SecretID   string
	VersionID  string
	ParentRow  map[string]any
	VersionRow map[string]any
}

// publishUpsert runs the syncer's pre-tx work for an upsert and returns the
// in-tx outbox closure (nil, nil when no syncer is installed).
func (s *Store) publishUpsert(ctx context.Context, tenantID string, versionRow, parentRow map[string]any) (func(*sql.Tx) error, error) {
	if s.syncer == nil {
		return nil, nil
	}
	return s.syncer.PublishSecretUpsert(ctx, tenantID, versionRow, parentRow)
}

// publishRevoke runs the syncer's pre-tx work for a revoke.
func (s *Store) publishRevoke(ctx context.Context, tenantID string, parentRow map[string]any) (func(*sql.Tx) error, error) {
	if s.syncer == nil {
		return nil, nil
	}
	return s.syncer.PublishSecretRevoke(ctx, tenantID, parentRow)
}

// b64col wraps raw bytes for JSON transport of a BLOB column. The consumer's
// controlapply.coerce decodes {"$b64":…} back to []byte before binding, so
// the bytes round-trip into the BLOB column unchanged (a plain base64 string
// would bind as TEXT and corrupt the ciphertext).
func b64col(b []byte) map[string]any {
	return map[string]any{"$b64": base64.StdEncoding.EncodeToString(b)}
}

// parentRowMap projects a tenant_secrets row onto the full RowsArtifact column
// map (INSERT OR REPLACE needs every NOT-NULL column; nullable columns are
// omitted when empty so the consumer writes SQL NULL, not ""). Mirrors the
// tenant_secrets schema (migration 0008).
func parentRowMap(m *SecretMetadata) map[string]any {
	row := map[string]any{
		"secret_id":   m.SecretID,
		"tenant_id":   m.TenantID,
		"name":        m.Name,
		"created_at":  m.CreatedAt.UTC().Format(time.RFC3339),
		"key_version": m.KeyVersion,
	}
	if m.Stack != nil && *m.Stack != "" {
		row["stack"] = *m.Stack
	}
	if m.Description != "" {
		row["description"] = m.Description
	}
	if m.CreatedBy != "" {
		row["created_by"] = m.CreatedBy
	}
	if m.LastRotatedAt != nil {
		row["last_rotated_at"] = m.LastRotatedAt.UTC().Format(time.RFC3339)
	}
	if m.RevokedAt != nil {
		row["revoked_at"] = m.RevokedAt.UTC().Format(time.RFC3339)
	}
	return row
}

// versionRowMap projects a tenant_secret_versions row. The four blob columns
// travel base64-wrapped; see b64col.
func versionRowMap(versionID, secretID string, versionNo int, es *EncryptedSecret, createdAt time.Time) map[string]any {
	return map[string]any{
		"version_id":  versionID,
		"secret_id":   secretID,
		"version_no":  versionNo,
		"nonce":       b64col(es.Nonce),
		"ciphertext":  b64col(es.Ciphertext),
		"wrapped_dek": b64col(es.WrappedDEK),
		"dek_nonce":   b64col(es.DEKNonce),
		"created_at":  createdAt.UTC().Format(time.RFC3339),
	}
}
