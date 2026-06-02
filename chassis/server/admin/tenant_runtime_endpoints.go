package admin

// Operator controls for per-tenant request admission (the 402/403 gate).
// `txco admin tenant suspend/resume` writes one tenant_runtime_state row
// through here, then reloads the dbcache mirror so the admission provider
// picks it up on the next request — the same write→reload pattern the
// hostname endpoints use. The billing system drives the SAME table via
// entitlement.updated fleet events; this is the manual operator surface.
//
// Routes sit under the tenant-scoped subrouter (/v1/tenants/{tenant}/...)
// so resolveTenantMiddleware has already resolved the slug → ac.TenantID
// (and 404'd an unknown tenant). The action itself is chassis-wide, so the
// handler additionally gates on super_admin.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
)

type setRuntimeStateRequest struct {
	DenyStatus int    `json:"deny_status,omitempty"` // 402 | 403; default 402 on suspend
	DenyReason string `json:"deny_reason,omitempty"` // surfaced as x-txc-deny-reason
}

type tenantRuntimeStateRecord struct {
	TenantID   string `json:"tenant_id"`
	Slug       string `json:"slug"`
	Enabled    bool   `json:"enabled"`
	Suspended  bool   `json:"suspended"`
	DenyStatus int    `json:"deny_status"`
	DenyReason string `json:"deny_reason,omitempty"`
}

func (c *Controller) handleSuspendTenant(w http.ResponseWriter, r *http.Request) {
	c.setTenantRuntimeState(w, r, true)
}

func (c *Controller) handleResumeTenant(w http.ResponseWriter, r *http.Request) {
	c.setTenantRuntimeState(w, r, false)
}

// setTenantRuntimeState upserts the tenant's tenant_runtime_state row
// (suspended on/off) and reloads the dbcache so the admission provider
// sees it next request. On a fleet deployment it also queues an
// entitlement.updated event so replicas converge.
func (c *Controller) setTenantRuntimeState(w http.ResponseWriter, r *http.Request, suspend bool) {
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}

	// Resume returns to admit-shaped defaults; suspend defaults to 402 and
	// accepts overrides from the (optional) body.
	denyStatus, denyReason := 403, ""
	if suspend {
		denyStatus, denyReason = 402, "payment_required"
		var req setRuntimeStateRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body",
				map[string]any{"err": err.Error()})
			return
		}
		if req.DenyStatus != 0 {
			denyStatus = req.DenyStatus
		}
		if req.DenyReason != "" {
			denyReason = req.DenyReason
		}
		if denyStatus < 100 || denyStatus > 599 {
			writeJSONError(w, http.StatusBadRequest, "invalid_deny_status",
				map[string]any{"deny_status": denyStatus, "hint": "must be a 1xx–5xx HTTP status (typically 402 or 403)"})
			return
		}
	}

	row := tenantRuntimeRow(ac.TenantID, suspend, denyStatus, denyReason)

	// Fleet-sync producer: upload the artifact BEFORE the tx so an orphaned
	// upload (commit fails) is GC-recoverable. Single-node skips this.
	var fleetRef, fleetSum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadEntitlementUpsert(r.Context(), ac.TenantID, row)
		if ferr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload",
				map[string]any{"err": ferr.Error()})
			return
		}
		fleetRef, fleetSum = ref, sum
	}

	tx, err := c.pu.RuntimeDB.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "begin_tx",
			map[string]any{"err": err.Error()})
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Phase 1 owns only the admission columns; the reserved
	// rate/concurrency columns take their defaults here. When Phase 2
	// lands, suspend/resume must read-modify-write to preserve those (or
	// the billing producer owns the full row via its own events).
	if _, err := tx.ExecContext(r.Context(),
		`INSERT OR REPLACE INTO tenant_runtime_state
		   (tenant_id, enabled, suspended, deny_status, deny_reason, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		row["tenant_id"], row["enabled"], row["suspended"],
		row["deny_status"], row["deny_reason"], row["updated_at"],
	); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "write_runtime_state",
			map[string]any{"err": err.Error()})
		return
	}

	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeEntitlementUpdated, ac.TenantID, "", 0, 0,
			fleetRef, fleetSum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue",
				map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit",
			map[string]any{"err": err.Error()})
		return
	}
	committed = true

	// Synchronously refresh the dbcache so the admission provider picks up
	// the new row on the next request. Matches the hostname/activate flow.
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after tenant runtime-state write failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}

	writeJSON(w, http.StatusOK, tenantRuntimeStateRecord{
		TenantID:   ac.TenantID,
		Slug:       ac.TenantSlug,
		Enabled:    true,
		Suspended:  suspend,
		DenyStatus: denyStatus,
		DenyReason: denyReason,
	})
}

// tenantRuntimeRow builds the admit-shaped row with the suspend knob set.
func tenantRuntimeRow(tenantID string, suspend bool, denyStatus int, denyReason string) map[string]any {
	suspended := 0
	if suspend {
		suspended = 1
	}
	return map[string]any{
		"tenant_id":   tenantID,
		"enabled":     1,
		"suspended":   suspended,
		"deny_status": denyStatus,
		"deny_reason": denyReason,
		"updated_at":  time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// fleetUploadEntitlementUpsert mirrors fleetUploadHostnameUpsert: a
// RowsArtifact targeting runtime.tenant_runtime_state, keyed by tenant_id
// so retries overwrite the same artifact.
func (c *Controller) fleetUploadEntitlementUpsert(ctx context.Context, tenantID string, row map[string]any) (string, string, error) {
	art := controlevent.RowsArtifact{
		DB:    "runtime",
		Table: "tenant_runtime_state",
		Op:    "upsert",
		Rows:  []map[string]any{row},
	}
	key := fmt.Sprintf("rows/tenant_runtime_state/%s", tenantID)
	ref, sum, _, err := c.fleetUploadArtifact(ctx, key, art)
	if err != nil {
		return "", "", err
	}
	return ref, sum, nil
}
