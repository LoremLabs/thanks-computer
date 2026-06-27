package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// --- fleet resync -----------------------------------------------------------
//
// POST /v1/fleet/resync re-emits ONE tenant's current control-plane state as
// fresh fleet-sync events, so lagging replicas converge. It targets a single
// tenant by design — it re-emits ALL of that tenant's data (its row + active
// hostnames + active stack versions), but never fans out across every tenant.
// A full fleet rebuild is a snapshot bootstrap, not a flood of re-emitted
// events.
//
// It is a NON-DESTRUCTIVE reconcile: it only ever emits upsert row-artifacts
// (tenant.created, hostname.bound) and stack.activated — never deletes/revokes
// — so re-running just re-asserts current state; consumers INSERT-OR-REPLACE and
// no-op where already in sync. The events carry fresh event_ids, so the
// consumer applies them (and reloads its dbcache) rather than dedup-skipping.
//
// Use: heal a replica that missed an event (e.g. a producer bug) for a specific
// tenant.

type fleetResyncRequest struct {
	// TenantSlug, when set, resyncs just that tenant; empty resyncs all.
	TenantSlug string `json:"tenant_slug,omitempty"`
}

type fleetResyncCounts struct {
	TenantCreated        int `json:"tenant_created"`
	HostnameBound        int `json:"hostname_bound"`
	StackActivated       int `json:"stack_activated"`
	DNSZoneUpserted      int `json:"dns_zone_upserted"`
	CronSettingsUpserted int `json:"cron_settings_upserted"`
	SecretChanged        int `json:"secret_changed"`
}

type fleetResyncResponse struct {
	FleetEnabled bool              `json:"fleet_enabled"`
	TenantSlug   string            `json:"tenant_slug,omitempty"`
	Events       fleetResyncCounts `json:"events"`
}

// resyncEvent is one prepared (artifact already uploaded) event awaiting its
// outbox row.
type resyncEvent struct {
	eventType string
	tenantID  string
	stackID   string
	version   int64
	ref       string
	sum       string
}

func (c *Controller) handleFleetResync(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}

	var req fleetResyncRequest
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		// An empty body is fine (resync all); only a malformed one is an error.
		if err := dec.Decode(&req); err != nil && err.Error() != "EOF" {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
			return
		}
	}

	// Fleet sync off ⇒ nothing to do (single-node). Report it plainly rather
	// than silently "succeeding".
	if !c.fleetEnabled() {
		writeJSON(w, http.StatusOK, fleetResyncResponse{FleetEnabled: false})
		return
	}

	// One tenant at a time — by design. `resync` re-emits ALL of the tenant's
	// data, but never fans out across every tenant: a full fleet rebuild is a
	// snapshot bootstrap, not a flood of re-emitted events.
	if req.TenantSlug == "" {
		writeJSONError(w, http.StatusBadRequest, "tenant_slug_required", map[string]any{
			"hint": "resync targets one tenant; pass tenant_slug. For a full fleet rebuild use a snapshot bootstrap.",
		})
		return
	}
	t, err := c.tenants.LookupBySlug(r.Context(), req.TenantSlug)
	if err != nil {
		if err == tenants.ErrNotFound {
			writeJSONError(w, http.StatusNotFound, "tenant_not_found", map[string]any{"slug": req.TenantSlug})
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "lookup_tenant", map[string]any{"err": err.Error()})
		return
	}
	targets := []tenants.Tenant{*t}

	// Phase 1 (no tx): upload every artifact and collect the pending events.
	// Artifact-before-tx keeps the outbox-commit the single acceptance point —
	// an upload failure aborts before any outbox row is written (orphan
	// artifacts are GC-recoverable).
	pending, counts, err := c.buildResyncEvents(r.Context(), targets)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "resync_build", map[string]any{"err": err.Error()})
		return
	}

	// Phase 2 (one tx): append all outbox rows, commit. The pump publishes them
	// asynchronously; consumers apply + advance their cursor.
	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx", map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	for _, e := range pending {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			e.eventType, e.tenantID, e.stackID, e.version, 0, e.ref, e.sum); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue", map[string]any{"err": qerr.Error()})
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	c.pu.Logger.Info("fleet resync queued",
		zap.String("tenant_slug", req.TenantSlug),
		zap.Int("tenant_created", counts.TenantCreated),
		zap.Int("hostname_bound", counts.HostnameBound),
		zap.Int("dns_zone_upserted", counts.DNSZoneUpserted),
		zap.Int("cron_settings_upserted", counts.CronSettingsUpserted),
		zap.Int("secret_changed", counts.SecretChanged),
		zap.Int("stack_activated", counts.StackActivated))

	writeJSON(w, http.StatusOK, fleetResyncResponse{
		FleetEnabled: true,
		TenantSlug:   req.TenantSlug,
		Events:       counts,
	})
}

// buildResyncEvents uploads the artifacts for every target tenant's current
// state (tenant row, active hostnames, active stack versions) and returns the
// pending events to enqueue. No DB writes here beyond reads.
func (c *Controller) buildResyncEvents(ctx context.Context, targets []tenants.Tenant) ([]resyncEvent, fleetResyncCounts, error) {
	var pending []resyncEvent
	var counts fleetResyncCounts

	for _, t := range targets {
		// tenant.created
		tArt := controlevent.RowsArtifact{
			DB:    "runtime",
			Table: "tenants",
			Op:    "upsert",
			Rows:  []map[string]any{tenantToRow(t)},
		}
		ref, sum, _, err := c.fleetUploadArtifact(ctx, fmt.Sprintf("rows/tenants/%s", t.TenantID), tArt)
		if err != nil {
			return nil, counts, fmt.Errorf("tenant %s: %w", t.TenantID, err)
		}
		pending = append(pending, resyncEvent{eventType: controlevent.TypeTenantCreated, tenantID: t.TenantID, ref: ref, sum: sum})
		counts.TenantCreated++

		// hostname.bound for each active hostname
		hns, err := c.tenants.ListHostnames(ctx, t.TenantID, false)
		if err != nil {
			return nil, counts, fmt.Errorf("tenant %s hostnames: %w", t.TenantID, err)
		}
		for _, h := range hns {
			hRef, hSum, herr := c.fleetUploadHostnameUpsert(ctx, h)
			if herr != nil {
				return nil, counts, fmt.Errorf("hostname %s: %w", h.ID, herr)
			}
			pending = append(pending, resyncEvent{eventType: controlevent.TypeHostnameBound, tenantID: t.TenantID, ref: hRef, sum: hSum})
			counts.HostnameBound++
		}

		// dns.zone.upserted for each active delegated zone — so a node brought
		// up via resync holds the zone state (re-derives routing hosts + can
		// serve the zone), not just the hostname rows.
		zones, zerr := c.tenants.ListZones(ctx, t.TenantID, false)
		if zerr != nil {
			return nil, counts, fmt.Errorf("tenant %s zones: %w", t.TenantID, zerr)
		}
		for _, z := range zones {
			zArt := controlevent.RowsArtifact{
				DB:    "runtime",
				Table: "dns_zones",
				Op:    "upsert",
				Rows:  []map[string]any{zoneToRow(z)},
			}
			zRef, zSum, _, zuerr := c.fleetUploadArtifact(ctx, fmt.Sprintf("rows/dns_zones/%s", z.ID), zArt)
			if zuerr != nil {
				return nil, counts, fmt.Errorf("zone %s: %w", z.ID, zuerr)
			}
			pending = append(pending, resyncEvent{eventType: controlevent.TypeDNSZoneUpserted, tenantID: t.TenantID, ref: zRef, sum: zSum})
			counts.DNSZoneUpserted++
		}

		// cron.settings.upserted — the tenant's cron timezone, so a resynced
		// node localizes @cron.* consistently. Re-published verbatim (incl. its
		// updated_at) so the row matches what the admin node holds.
		if cs, ok, cerr := tenants.LoadCronSettings(ctx, c.pu.RuntimeDB, t.TenantID); cerr != nil {
			return nil, counts, fmt.Errorf("tenant %s cron settings: %w", t.TenantID, cerr)
		} else if ok {
			cRef, cSum, cuerr := c.fleetUploadCronSettingsUpsert(ctx, t.TenantID, cs.Timezone, cs.UpdatedAt, cs.UpdatedBy)
			if cuerr != nil {
				return nil, counts, fmt.Errorf("tenant %s cron settings upload: %w", t.TenantID, cuerr)
			}
			pending = append(pending, resyncEvent{eventType: controlevent.TypeCronSettingsUpserted, tenantID: t.TenantID, ref: cRef, sum: cSum})
			counts.CronSettingsUpserted++
		}

		// secret.changed for each ACTIVE secret — the parent row + its active
		// version row (encrypted blobs travel as-is). Decryptable on the
		// receiving node only when the fleet shares one master key
		// (TXCO_SECRET_MASTER_KEY_B64); this is the backfill/recovery path for
		// new or lagging nodes. Version row queued before parent (see Syncer).
		if c.pu.Secrets != nil {
			resyncRows, serr := c.pu.Secrets.Store().ListForResync(ctx, t.TenantID)
			if serr != nil {
				return nil, counts, fmt.Errorf("tenant %s secrets: %w", t.TenantID, serr)
			}
			for _, rr := range resyncRows {
				vArt := controlevent.RowsArtifact{
					DB: "runtime", Table: "tenant_secret_versions", Op: "upsert",
					Rows: []map[string]any{rr.VersionRow},
				}
				vRef, vSum, _, verr := c.fleetUploadArtifact(ctx,
					fmt.Sprintf("rows/tenant_secret_versions/%s", rr.VersionID), vArt)
				if verr != nil {
					return nil, counts, fmt.Errorf("secret version %s: %w", rr.VersionID, verr)
				}
				pending = append(pending, resyncEvent{eventType: controlevent.TypeSecretChanged, tenantID: t.TenantID, ref: vRef, sum: vSum})
				counts.SecretChanged++

				pArt := controlevent.RowsArtifact{
					DB: "runtime", Table: "tenant_secrets", Op: "upsert",
					Rows: []map[string]any{rr.ParentRow},
				}
				pRef, pSum, _, perr := c.fleetUploadArtifact(ctx,
					fmt.Sprintf("rows/tenant_secrets/%s", rr.SecretID), pArt)
				if perr != nil {
					return nil, counts, fmt.Errorf("secret parent %s: %w", rr.SecretID, perr)
				}
				pending = append(pending, resyncEvent{eventType: controlevent.TypeSecretChanged, tenantID: t.TenantID, ref: pRef, sum: pSum})
				counts.SecretChanged++
			}
		}

		// stack.activated for each stack with an active version. active_version
		// holds a version_id, so JOIN to recover its version_number (the value
		// the artifact + event carry), mirroring handleListStacks.
		rows, err := c.pu.RuntimeDB.QueryContext(ctx,
			`SELECT s.stack_id, s.name, sv.version_number
			   FROM stacks s
			   JOIN stack_versions sv ON sv.version_id = s.active_version
			  WHERE s.tenant_id = ?`, t.TenantID)
		if err != nil {
			return nil, counts, fmt.Errorf("tenant %s stacks: %w", t.TenantID, err)
		}
		type activeStack struct {
			stackID string
			name    string
			version int64
		}
		var stacks []activeStack
		for rows.Next() {
			var s activeStack
			if err := rows.Scan(&s.stackID, &s.name, &s.version); err != nil {
				_ = rows.Close()
				return nil, counts, fmt.Errorf("tenant %s scan stack: %w", t.TenantID, err)
			}
			stacks = append(stacks, s)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, counts, err
		}
		_ = rows.Close()

		for _, s := range stacks {
			files, ferr := c.readStackFilesForArtifact(ctx, t.TenantID, s.name, s.version)
			if ferr != nil {
				return nil, counts, fmt.Errorf("stack %s/%s files: %w", t.TenantID, s.name, ferr)
			}
			sArt := controlevent.StackActivatedArtifact{
				TenantID: t.TenantID,
				Stack:    s.name,
				Version:  s.version,
				Files:    files,
			}
			sRef, sSum, _, serr := c.fleetUploadArtifact(ctx,
				fmt.Sprintf("stacks/%s/%s/%d", t.TenantID, s.name, s.version), sArt)
			if serr != nil {
				return nil, counts, fmt.Errorf("stack %s/%s: %w", t.TenantID, s.name, serr)
			}
			pending = append(pending, resyncEvent{
				eventType: controlevent.TypeStackActivated,
				tenantID:  t.TenantID,
				stackID:   s.stackID,
				version:   s.version,
				ref:       sRef,
				sum:       sSum,
			})
			counts.StackActivated++
		}
	}
	return pending, counts, nil
}
