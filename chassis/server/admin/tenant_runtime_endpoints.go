package admin

// Operator controls for per-tenant request admission (suspend/resume + the
// node-local rate/concurrency limits). Each verb reads the tenant's current
// tenant_runtime_state row, mutates only its own columns, and writes the FULL
// row back — both locally and as the fleet artifact — so suspend never
// clobbers limits and `limits` never clobbers suspend (the consumer applier
// does a full-row INSERT OR REPLACE). Then it reloads the dbcache mirror so
// the admission provider picks the change up on the next request. The billing
// system drives the SAME table via entitlement.updated fleet events; this is
// the manual operator surface.
//
// Routes sit under the tenant-scoped subrouter (/v1/tenants/{tenant}/...) so
// resolveTenantMiddleware has already resolved the slug → ac.TenantID (and
// 404'd an unknown tenant). The actions are chassis-wide, so each handler
// additionally gates on super_admin.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

// setTenantLimitsRequest patches only the fields the operator provided —
// nil means "leave as-is", so `--rps 50` alone doesn't clear concurrency.
type setTenantLimitsRequest struct {
	RPS         *float64 `json:"rps,omitempty"`
	Burst       *int     `json:"burst,omitempty"`
	Concurrency *int     `json:"concurrency,omitempty"`
}

type tenantRuntimeStateRecord struct {
	TenantID         string  `json:"tenant_id"`
	Slug             string  `json:"slug"`
	Enabled          bool    `json:"enabled"`
	Suspended        bool    `json:"suspended"`
	DenyStatus       int     `json:"deny_status"`
	DenyReason       string  `json:"deny_reason,omitempty"`
	RateLimitRPS     float64 `json:"rate_limit_rps"`
	RateBurst        int     `json:"rate_burst"`
	ConcurrencyLimit int     `json:"concurrency_limit"`
}

// runtimeRow is the full tenant_runtime_state row (sans tenant_id/updated_at),
// loaded read-modify-write so each verb preserves the columns it doesn't own.
type runtimeRow struct {
	Enabled          int
	Suspended        int
	DenyStatus       int
	DenyReason       string
	RateLimitRPS     float64
	RateBurst        int
	ConcurrencyLimit int
}

func defaultRuntimeRow() runtimeRow {
	return runtimeRow{Enabled: 1, Suspended: 0, DenyStatus: 403}
}

func (rr runtimeRow) toMap(tenantID string) map[string]any {
	return map[string]any{
		"tenant_id":         tenantID,
		"enabled":           rr.Enabled,
		"suspended":         rr.Suspended,
		"deny_status":       rr.DenyStatus,
		"deny_reason":       rr.DenyReason,
		"rate_limit_rps":    rr.RateLimitRPS,
		"rate_burst":        rr.RateBurst,
		"concurrency_limit": rr.ConcurrencyLimit,
		"updated_at":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
}

func (c *Controller) loadRuntimeRow(ctx context.Context, tenantID string) (runtimeRow, error) {
	rr := defaultRuntimeRow()
	err := c.pu.RuntimeDB.QueryRowContext(ctx,
		`SELECT enabled, suspended, deny_status, deny_reason, rate_limit_rps, rate_burst, concurrency_limit
		   FROM tenant_runtime_state WHERE tenant_id = ?`, tenantID).Scan(
		&rr.Enabled, &rr.Suspended, &rr.DenyStatus, &rr.DenyReason,
		&rr.RateLimitRPS, &rr.RateBurst, &rr.ConcurrencyLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return defaultRuntimeRow(), nil
	}
	if err != nil {
		return rr, err
	}
	return rr, nil
}

func (c *Controller) handleSuspendTenant(w http.ResponseWriter, r *http.Request) {
	ac := c.runtimeStateAuth(w, r)
	if ac == nil {
		return
	}
	denyStatus, denyReason := 402, "payment_required"
	var req setRuntimeStateRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
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
			map[string]any{"deny_status": denyStatus, "hint": "must be a 1xx-5xx HTTP status (typically 402 or 403)"})
		return
	}
	rr, err := c.loadRuntimeRow(r.Context(), ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load_runtime_state", map[string]any{"err": err.Error()})
		return
	}
	rr.Enabled, rr.Suspended, rr.DenyStatus, rr.DenyReason = 1, 1, denyStatus, denyReason
	c.writeRuntimeRow(w, r, ac, rr)
}

func (c *Controller) handleResumeTenant(w http.ResponseWriter, r *http.Request) {
	ac := c.runtimeStateAuth(w, r)
	if ac == nil {
		return
	}
	rr, err := c.loadRuntimeRow(r.Context(), ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load_runtime_state", map[string]any{"err": err.Error()})
		return
	}
	rr.Enabled, rr.Suspended, rr.DenyStatus, rr.DenyReason = 1, 0, 403, ""
	c.writeRuntimeRow(w, r, ac, rr)
}

func (c *Controller) handleSetTenantLimits(w http.ResponseWriter, r *http.Request) {
	ac := c.runtimeStateAuth(w, r)
	if ac == nil {
		return
	}
	var req setTenantLimitsRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	rr, err := c.loadRuntimeRow(r.Context(), ac.TenantID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load_runtime_state", map[string]any{"err": err.Error()})
		return
	}
	// Patch only what was provided (nil => leave as-is).
	if req.RPS != nil {
		if *req.RPS < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_rps", map[string]any{"rps": *req.RPS})
			return
		}
		rr.RateLimitRPS = *req.RPS
	}
	if req.Burst != nil {
		rr.RateBurst = *req.Burst
	}
	if req.Concurrency != nil {
		if *req.Concurrency < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_concurrency", map[string]any{"concurrency": *req.Concurrency})
			return
		}
		rr.ConcurrencyLimit = *req.Concurrency
	}
	// Burst defaults to ceil(2*rps) (min 1) when a rate is set but burst is
	// unset/invalid — a more useful protection shape than burst==rps.
	if rr.RateLimitRPS > 0 && rr.RateBurst < 1 {
		rr.RateBurst = int(math.Ceil(2 * rr.RateLimitRPS))
		if rr.RateBurst < 1 {
			rr.RateBurst = 1
		}
	}
	c.writeRuntimeRow(w, r, ac, rr)
}

// runtimeStateAuth enforces super_admin and returns the resolved auth
// context (with a populated TenantID), or writes the error and returns nil.
func (c *Controller) runtimeStateAuth(w http.ResponseWriter, r *http.Request) *auth.Context {
	if err := policy.RequireSuperAdmin(r.Context()); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return nil
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return nil
	}
	return ac
}

// writeRuntimeRow upserts the full row (local + fleet), reloads the dbcache,
// and responds. The fleet artifact carries the same full row, so a replica's
// INSERT OR REPLACE never drops a column this verb didn't touch.
func (c *Controller) writeRuntimeRow(w http.ResponseWriter, r *http.Request, ac *auth.Context, rr runtimeRow) {
	row := rr.toMap(ac.TenantID)

	// Fleet-sync producer: upload the artifact BEFORE the tx so an orphaned
	// upload (commit fails) is GC-recoverable. Single-node skips this.
	var fleetRef, fleetSum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadEntitlementUpsert(r.Context(), ac.TenantID, row)
		if ferr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_upload", map[string]any{"err": ferr.Error()})
			return
		}
		fleetRef, fleetSum = ref, sum
	}

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

	if _, err := tx.ExecContext(r.Context(),
		`INSERT OR REPLACE INTO tenant_runtime_state
		   (tenant_id, enabled, suspended, deny_status, deny_reason,
		    rate_limit_rps, rate_burst, concurrency_limit, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row["tenant_id"], row["enabled"], row["suspended"], row["deny_status"], row["deny_reason"],
		row["rate_limit_rps"], row["rate_burst"], row["concurrency_limit"], row["updated_at"],
	); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "write_runtime_state", map[string]any{"err": err.Error()})
		return
	}

	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeEntitlementUpdated, ac.TenantID, "", 0, 0, fleetRef, fleetSum,
		); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue", map[string]any{"err": qerr.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true

	// Synchronously refresh the dbcache so the admission provider picks up
	// the change on the next request. Matches the hostname/activate flow.
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after tenant runtime-state write failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}

	writeJSON(w, http.StatusOK, tenantRuntimeStateRecord{
		TenantID:         ac.TenantID,
		Slug:             ac.TenantSlug,
		Enabled:          rr.Enabled != 0,
		Suspended:        rr.Suspended != 0,
		DenyStatus:       rr.DenyStatus,
		DenyReason:       rr.DenyReason,
		RateLimitRPS:     rr.RateLimitRPS,
		RateBurst:        rr.RateBurst,
		ConcurrencyLimit: rr.ConcurrencyLimit,
	})
}

// fleetUploadEntitlementUpsert mirrors fleetUploadHostnameUpsert: a
// RowsArtifact targeting runtime.tenant_runtime_state, keyed by tenant_id so
// retries overwrite the same artifact.
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
