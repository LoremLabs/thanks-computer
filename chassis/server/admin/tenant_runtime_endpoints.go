package admin

// Operator controls for per-tenant request admission (suspend/resume + the
// node-local rate/concurrency limits). Each verb reads the tenant's current
// tenant_runtime_state row, mutates only its own columns, and writes the FULL
// row back — both locally and as the fleet artifact — so suspend never
// clobbers limits and `limits` never clobbers suspend (the consumer applier
// does a full-row INSERT OR REPLACE). Then it reloads the dbcache mirror so
// the admission provider picks the change up on the next request.
//
// Column ownership: admission denies on `enabled==0 || suspended==1` (two
// independent columns). The OPERATOR verbs here drive `enabled` (suspend ->
// enabled=0, resume -> enabled=1). The `suspended` column is the PROGRAMMATIC
// gate driven by background services via entitlement.updated fleet events (e.g.
// the credit reconciler), reached in-process through applyRuntimeRow. Splitting
// the two across columns lets an operator disable and a programmatic gate
// coexist without clobbering each other. The single shared (deny_status,
// deny_reason) pair is only cleared back to default when BOTH columns are open
// (clearDenyIfOpen), so re-enabling a tenant the gate still holds down keeps
// the gate's reason surfaced.
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

// clearDenyIfOpen resets the shared (deny_status, deny_reason) pair to the
// default only when the row is fully open — neither column denying. When the
// other denier is still active (an operator disable via enabled, or a
// programmatic gate via suspended), the reason is left in place so it keeps
// surfacing. Used by operator resume (it owns enabled) and gate release (it
// owns suspended); each clears its own column first, then calls this.
func (rr *runtimeRow) clearDenyIfOpen() {
	if rr.Enabled == 1 && rr.Suspended == 0 {
		rr.DenyStatus, rr.DenyReason = 403, ""
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
	// Operator disable drives the `enabled` column; leave `suspended` (the
	// programmatic gate) untouched so a credit gate isn't clobbered.
	rr.Enabled, rr.DenyStatus, rr.DenyReason = 0, denyStatus, denyReason
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
	// Operator re-enable drives the `enabled` column; leave `suspended` (the
	// programmatic gate) untouched. Clear the shared deny reason only if the
	// row is now fully open (no still-active programmatic gate).
	rr.Enabled = 1
	rr.clearDenyIfOpen()
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

// runtimeWriteError carries the per-step error code so the HTTP wrapper can
// surface the same machine code it did before applyRuntimeRow was extracted.
type runtimeWriteError struct {
	code string
	err  error
}

func (e *runtimeWriteError) Error() string { return e.err.Error() }
func (e *runtimeWriteError) Unwrap() error { return e.err }

// applyRuntimeRow upserts the full row (local + fleet artifact), queues the
// entitlement.updated event, commits, and reloads the dbcache mirror. The
// fleet artifact carries the same full row, so a replica's INSERT OR REPLACE
// never drops a column this write didn't touch. This is the one true write
// path: the HTTP operator verbs (via writeRuntimeRow) and the in-process
// programmatic gate (SetGate) both route through here, so neither can emit a
// partial row.
func (c *Controller) applyRuntimeRow(ctx context.Context, tenantID string, rr runtimeRow) error {
	row := rr.toMap(tenantID)

	// Fleet-sync producer: upload the artifact BEFORE the tx so an orphaned
	// upload (commit fails) is GC-recoverable. Single-node skips this.
	var fleetRef, fleetSum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadEntitlementUpsert(ctx, tenantID, row)
		if ferr != nil {
			return &runtimeWriteError{"fleet_upload", ferr}
		}
		fleetRef, fleetSum = ref, sum
	}

	tx, err := c.pu.RuntimeDB.BeginTx(ctx, nil)
	if err != nil {
		return &runtimeWriteError{"begin_tx", err}
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO tenant_runtime_state
		   (tenant_id, enabled, suspended, deny_status, deny_reason,
		    rate_limit_rps, rate_burst, concurrency_limit, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row["tenant_id"], row["enabled"], row["suspended"], row["deny_status"], row["deny_reason"],
		row["rate_limit_rps"], row["rate_burst"], row["concurrency_limit"], row["updated_at"],
	); err != nil {
		return &runtimeWriteError{"write_runtime_state", err}
	}

	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(ctx, tx,
			controlevent.TypeEntitlementUpdated, tenantID, "", 0, 0, fleetRef, fleetSum,
		); qerr != nil {
			return &runtimeWriteError{"fleet_queue", qerr}
		}
	}

	if err := tx.Commit(); err != nil {
		return &runtimeWriteError{"commit", err}
	}
	committed = true

	// Synchronously refresh the dbcache so the admission provider picks up
	// the change on the next request. Matches the hostname/activate flow.
	if err := c.pu.Dbc.Reload(); err != nil {
		c.pu.Logger.Warn("dbcache reload after tenant runtime-state write failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}
	return nil
}

// writeRuntimeRow is the HTTP wrapper around applyRuntimeRow: it applies the
// row and renders the resulting record (or the per-step error code).
func (c *Controller) writeRuntimeRow(w http.ResponseWriter, r *http.Request, ac *auth.Context, rr runtimeRow) {
	if err := c.applyRuntimeRow(r.Context(), ac.TenantID, rr); err != nil {
		code := "write_runtime_state"
		var we *runtimeWriteError
		if errors.As(err, &we) {
			code = we.code
		}
		writeJSONError(w, http.StatusInternalServerError, code, map[string]any{"err": err.Error()})
		return
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
