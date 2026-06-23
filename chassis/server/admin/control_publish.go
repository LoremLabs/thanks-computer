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
	"strings"

	"golang.org/x/sync/errgroup"

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
		SELECT sf.path, sf.content, sf.content_hash
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
	// Drain the rows first: we must not hold the query open while fanning out
	// the concurrent CAS Puts below. Artifact order stays fixed by ORDER BY path.
	var out []controlevent.StackArtifactFile
	type casPut struct{ path, hash, content string }
	var puts []casPut
	for rows.Next() {
		var path, content, hash string
		if err := rows.Scan(&path, &content, &hash); err != nil {
			rows.Close()
			return nil, err
		}
		// FILES/** static assets travel as a fingerprint, not bytes: ensure
		// the bytes are in the shared CAS, then ship only ContentHash so
		// data-plane nodes resolve them lazily (and never inline them).
		// Rule/fixture files stay inline. Single-node deployments never call
		// this (gated on FeedSink != nop), so the file/disk CAS is fine there.
		if strings.HasPrefix(path, "FILES/") {
			if hash == "" {
				hash = sha256Hex(content)
			}
			if c.fcas != nil {
				puts = append(puts, casPut{path: path, hash: hash, content: content})
			}
			out = append(out, controlevent.StackArtifactFile{Path: path, ContentHash: hash})
			continue
		}
		out = append(out, controlevent.StackArtifactFile{Path: path, Content: content})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// Push FILES bytes into the shared CAS CONCURRENTLY (bounded), skipping
	// content already present — the same pattern + limit as materialiseFiles.
	// This producer path runs on FLEET nodes, where the in-tx materialiseFiles
	// is gated off; doing these Puts one-at-a-time in the row loop above ran
	// ~Nfiles × put-latency (≈69s for 790 files) and 502'd the activate.
	if len(puts) > 0 {
		// Skip files whose hash is already active for this stack: the current
		// active version materialised them into the shared CAS, so they need NO
		// CAS round-trip at all — not even an Exists HEAD. (The active version's
		// bytes being in the CAS is load-bearing: the live stack couldn't serve
		// otherwise.) This makes a re-push that touched a few files cost a few
		// CAS ops, not Nfiles. The remaining new/changed hashes still go through
		// Exists-then-Put — they may exist from an OLDER version or a concurrent
		// push, so the Exists guard stays for those.
		prior := c.priorActiveFileHashes(ctx, tenantID, stackName)
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(materialiseConcurrency)
		for _, p := range puts {
			if _, alreadyActive := prior[p.hash]; alreadyActive {
				continue
			}
			g.Go(func() (err error) {
				// A worker panic would crash the whole chassis (the HTTP
				// handler's recover can't reach another goroutine); convert it
				// to an error so activation fails cleanly.
				defer func() {
					if r := recover(); r != nil {
						err = fmt.Errorf("filecas put %s: panic: %v", p.path, r)
					}
				}()
				if ok, _ := c.fcas.Exists(gctx, p.hash); ok {
					return nil
				}
				if perr := c.fcas.Put(gctx, p.hash, []byte(p.content)); perr != nil {
					return fmt.Errorf("filecas put %s: %w", p.path, perr)
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// priorActiveFileHashes returns the set of FILES/* content hashes that the
// stack's CURRENT active version already materialised into the shared CAS.
// readStackFilesForArtifact runs BEFORE the activation tx flips active_version,
// so this is the prior version's set — and a new version's file whose hash is
// in it is provably already in the CAS, so it needs no CAS round-trip at all.
// Best-effort: an empty/nil set just routes everything through Exists-then-Put.
func (c *Controller) priorActiveFileHashes(
	ctx context.Context, tenantID, stackName string,
) map[string]struct{} {
	rows, err := c.pu.RuntimeDB.QueryContext(ctx, `
		SELECT sf.content_hash
		  FROM stack_files sf
		  JOIN stacks s ON s.active_version = sf.version_id
		 WHERE s.tenant_id = ? AND s.name = ?
		   AND sf.path LIKE 'FILES/%' AND sf.content_hash <> ''`,
		tenantID, stackName)
	if err != nil {
		return nil
	}
	defer rows.Close()
	set := make(map[string]struct{})
	for rows.Next() {
		var h string
		if rows.Scan(&h) == nil && h != "" {
			set[h] = struct{}{}
		}
	}
	return set
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
		// Per-host DKIM (0017) — NOT NULL DEFAULT '', so always carried; a later
		// upsert must not blank a structured host's key on data-plane nodes.
		"dkim_selector":    h.DKIMSelector,
		"dkim_private_pem": h.DKIMPrivatePEM,
		"dkim_public_b64":  h.DKIMPublicB64,
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
