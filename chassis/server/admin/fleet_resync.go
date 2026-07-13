package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/controlpublish"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
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
	DNSRecordUpserted    int `json:"dns_record_upserted"`
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

// resyncJob is one event whose artifact upload (and any heavy read feeding
// it, like a stack's file set) has been DEFERRED so the jobs can run
// concurrently. Enumeration stays sequential and ordered; only the
// upload closures fan out.
type resyncJob struct {
	eventType string
	tenantID  string
	stackID   string
	version   int64
	upload    func(ctx context.Context) (ref, sum string, err error)
}

// resyncUploadParallelism bounds concurrent artifact uploads (and stack
// file reads) during a resync — enough to hide the per-object R2 + DB
// latency that made a sequential driplit-scale resync take minutes,
// without monopolizing the runtime pool.
const resyncUploadParallelism = 8

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
	// Chunked multi-row appends: one statement per ~60 events instead of
	// one per event (a driplit-scale resync queues thousands — per-row
	// appends held this tx open for minutes on a shared Postgres).
	// Event ids are minted in pending order, so ordering contracts
	// (secret version row before its parent) survive the batch.
	outRows := make([]controlpublish.OutboxRow, 0, len(pending))
	for _, e := range pending {
		eventID := "evt_" + hxid.NewTimeSort().String()
		payload, merr := json.Marshal(controlevent.Event{
			EventID:     eventID,
			Type:        e.eventType,
			TenantID:    e.tenantID,
			StackID:     e.stackID,
			Version:     e.version,
			ArtifactRef: e.ref,
			Checksum:    e.sum,
		})
		if merr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue", map[string]any{"err": merr.Error()})
			return
		}
		outRows = append(outRows, controlpublish.OutboxRow{
			EventID: eventID, EventType: e.eventType,
			TenantID: e.tenantID, StackID: e.stackID,
			Version:     e.version,
			ArtifactRef: e.ref, Checksum: e.sum,
			PayloadJSON: payload,
		})
	}
	if err := controlpublish.AppendOutboxBatch(r.Context(), tx, outRows, c.pu.RuntimeDialect); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "fleet_queue", map[string]any{"err": err.Error()})
		return
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
		zap.Int("dns_record_upserted", counts.DNSRecordUpserted),
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
// pending events to enqueue. No DB writes here beyond reads. Enumeration is
// sequential (cheap SQL, deterministic event order); the artifact uploads —
// and the per-stack file reads that feed them — run CONCURRENTLY afterwards
// (resyncUploadParallelism), because sequential per-object round trips made
// a publications-scale resync take minutes.
func (c *Controller) buildResyncEvents(ctx context.Context, targets []tenants.Tenant) ([]resyncEvent, fleetResyncCounts, error) {
	var jobs []resyncJob
	var counts fleetResyncCounts

	for _, t := range targets {
		// tenant.created
		t := t
		jobs = append(jobs, resyncJob{
			eventType: controlevent.TypeTenantCreated, tenantID: t.TenantID,
			upload: func(ctx context.Context) (string, string, error) {
				tArt := controlevent.RowsArtifact{
					DB:    "runtime",
					Table: "tenants",
					Op:    "upsert",
					Rows:  []map[string]any{tenantToRow(t)},
				}
				ref, sum, _, err := c.fleetUploadArtifact(ctx, fmt.Sprintf("rows/tenants/%s", t.TenantID), tArt)
				if err != nil {
					return "", "", fmt.Errorf("tenant %s: %w", t.TenantID, err)
				}
				return ref, sum, nil
			},
		})
		counts.TenantCreated++

		// hostname.bound for each active hostname
		hns, err := c.tenants.ListHostnames(ctx, t.TenantID, false)
		if err != nil {
			return nil, counts, fmt.Errorf("tenant %s hostnames: %w", t.TenantID, err)
		}
		for _, h := range hns {
			h := h
			jobs = append(jobs, resyncJob{
				eventType: controlevent.TypeHostnameBound, tenantID: t.TenantID,
				upload: func(ctx context.Context) (string, string, error) {
					ref, sum, err := c.fleetUploadHostnameUpsert(ctx, h)
					if err != nil {
						return "", "", fmt.Errorf("hostname %s: %w", h.ID, err)
					}
					return ref, sum, nil
				},
			})
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
			z := z
			jobs = append(jobs, resyncJob{
				eventType: controlevent.TypeDNSZoneUpserted, tenantID: t.TenantID,
				upload: func(ctx context.Context) (string, string, error) {
					zArt := controlevent.RowsArtifact{
						DB:    "runtime",
						Table: "dns_zones",
						Op:    "upsert",
						Rows:  []map[string]any{zoneToRow(z)},
					}
					ref, sum, _, err := c.fleetUploadArtifact(ctx, fmt.Sprintf("rows/dns_zones/%s", z.ID), zArt)
					if err != nil {
						return "", "", fmt.Errorf("zone %s: %w", z.ID, err)
					}
					return ref, sum, nil
				},
			})
			counts.DNSZoneUpserted++

			// dns.record.upserted for each active override record of the
			// zone — without these a resynced node serves the zone minus
			// its records (dns_records rows travel only by event or
			// snapshot re-bootstrap; see fleetPublishRecord).
			recs, rerr := c.tenants.ListRecords(ctx, z.ID)
			if rerr != nil {
				return nil, counts, fmt.Errorf("zone %s records: %w", z.ID, rerr)
			}
			for _, rec := range recs {
				rec := rec
				jobs = append(jobs, resyncJob{
					eventType: controlevent.TypeDNSRecordUpserted, tenantID: t.TenantID,
					upload: func(ctx context.Context) (string, string, error) {
						rArt := controlevent.RowsArtifact{
							DB:    "runtime",
							Table: "dns_records",
							Op:    "upsert",
							Rows:  []map[string]any{recordToRow(rec)},
						}
						ref, sum, _, err := c.fleetUploadArtifact(ctx, fmt.Sprintf("rows/dns_records/%s", rec.ID), rArt)
						if err != nil {
							return "", "", fmt.Errorf("record %s: %w", rec.ID, err)
						}
						return ref, sum, nil
					},
				})
				counts.DNSRecordUpserted++
			}
		}

		// cron.settings.upserted — the tenant's cron timezone, so a resynced
		// node localizes @cron.* consistently. Re-published verbatim (incl. its
		// updated_at) so the row matches what the admin node holds.
		if cs, ok, cerr := tenants.LoadCronSettings(ctx, c.pu.RuntimeDB, t.TenantID, c.pu.RuntimeDialect); cerr != nil {
			return nil, counts, fmt.Errorf("tenant %s cron settings: %w", t.TenantID, cerr)
		} else if ok {
			cs := cs
			jobs = append(jobs, resyncJob{
				eventType: controlevent.TypeCronSettingsUpserted, tenantID: t.TenantID,
				upload: func(ctx context.Context) (string, string, error) {
					ref, sum, err := c.fleetUploadCronSettingsUpsert(ctx, t.TenantID, cs.Timezone, cs.UpdatedAt, cs.UpdatedBy)
					if err != nil {
						return "", "", fmt.Errorf("tenant %s cron settings upload: %w", t.TenantID, err)
					}
					return ref, sum, nil
				},
			})
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
				rr := rr
				jobs = append(jobs, resyncJob{
					eventType: controlevent.TypeSecretChanged, tenantID: t.TenantID,
					upload: func(ctx context.Context) (string, string, error) {
						vArt := controlevent.RowsArtifact{
							DB: "runtime", Table: "tenant_secret_versions", Op: "upsert",
							Rows: []map[string]any{rr.VersionRow},
						}
						ref, sum, _, err := c.fleetUploadArtifact(ctx,
							fmt.Sprintf("rows/tenant_secret_versions/%s", rr.VersionID), vArt)
						if err != nil {
							return "", "", fmt.Errorf("secret version %s: %w", rr.VersionID, err)
						}
						return ref, sum, nil
					},
				})
				counts.SecretChanged++

				jobs = append(jobs, resyncJob{
					eventType: controlevent.TypeSecretChanged, tenantID: t.TenantID,
					upload: func(ctx context.Context) (string, string, error) {
						pArt := controlevent.RowsArtifact{
							DB: "runtime", Table: "tenant_secrets", Op: "upsert",
							Rows: []map[string]any{rr.ParentRow},
						}
						ref, sum, _, err := c.fleetUploadArtifact(ctx,
							fmt.Sprintf("rows/tenant_secrets/%s", rr.SecretID), pArt)
						if err != nil {
							return "", "", fmt.Errorf("secret parent %s: %w", rr.SecretID, err)
						}
						return ref, sum, nil
					},
				})
				counts.SecretChanged++
			}
		}

		// stack.activated for each stack with an active version. active_version
		// holds a version_id, so JOIN to recover its version_number (the value
		// the artifact + event carry), mirroring handleListStacks.
		rows, err := c.pu.RuntimeDB.QueryContext(ctx,
			c.rb(`SELECT s.stack_id, s.name, sv.version_number
			   FROM stacks s
			   JOIN stack_versions sv ON sv.version_id = s.active_version
			  WHERE s.tenant_id = ?`), t.TenantID)
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
			s := s
			jobs = append(jobs, resyncJob{
				eventType: controlevent.TypeStackActivated,
				tenantID:  t.TenantID,
				stackID:   s.stackID,
				version:   s.version,
				upload: func(ctx context.Context) (string, string, error) {
					// The heavy per-stack file read rides the concurrent
					// phase too — sequentially it alone was ~1 round trip
					// per stack.
					files, err := c.readStackFilesForArtifact(ctx, t.TenantID, s.name, s.version)
					if err != nil {
						return "", "", fmt.Errorf("stack %s/%s files: %w", t.TenantID, s.name, err)
					}
					sArt := controlevent.StackActivatedArtifact{
						TenantID: t.TenantID,
						Stack:    s.name,
						Version:  s.version,
						Files:    files,
					}
					ref, sum, _, uerr := c.fleetUploadArtifact(ctx,
						fmt.Sprintf("stacks/%s/%s/%d", t.TenantID, s.name, s.version), sArt)
					if uerr != nil {
						return "", "", fmt.Errorf("stack %s/%s: %w", t.TenantID, s.name, uerr)
					}
					return ref, sum, nil
				},
			})
			counts.StackActivated++
		}
	}

	// Concurrent upload phase: results land at their job's index, so the
	// pending slice keeps enumeration order exactly.
	pending := make([]resyncEvent, len(jobs))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(resyncUploadParallelism)
	for i, j := range jobs {
		i, j := i, j
		g.Go(func() error {
			ref, sum, err := j.upload(gctx)
			if err != nil {
				return err
			}
			pending[i] = resyncEvent{
				eventType: j.eventType, tenantID: j.tenantID,
				stackID: j.stackID, version: j.version,
				ref: ref, sum: sum,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, counts, err
	}
	return pending, counts, nil
}
