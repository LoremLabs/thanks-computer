package admin

// Per-tenant cron timezone (`txco cron config show|set`). The cron head
// stamps @cron.* in UTC by default; a tenant that sets a timezone here gets
// those wall-clock fields localized (so `WHEN @cron.hour == 9` means 09:00
// local). Tenant-scoped (under /v1/tenants/{t}) and gated on the same
// opstack capability that authoring a `_cron` stack needs — the timezone is
// how that stack's schedule is interpreted. Writes upsert the cron_settings
// row and reload the dbcache mirror so the cron head picks it up with no
// restart. The @cron.bucket dedup key stays UTC, so fleet cron is
// unaffected.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/auth/policy"
	"github.com/loremlabs/thanks-computer/chassis/auth/signature"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

type cronConfigDTO struct {
	// Timezone is the IANA name (e.g. "Asia/Tokyo"); empty means UTC (default).
	Timezone string `json:"timezone"`
	// Configured is true when a non-empty timezone is set for the tenant.
	Configured bool `json:"configured"`
}

type putCronConfigRequest struct {
	Timezone string `json:"timezone"`
}

// handleGetCronConfig returns the URL tenant's cron timezone (empty = UTC).
func (c *Controller) handleGetCronConfig(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:read"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}
	tz, configured, err := tenants.LoadCronTimezone(r.Context(), c.pu.RuntimeDB, ac.TenantID, c.pu.RuntimeDialect)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "load_cron_config", map[string]any{"err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cronConfigDTO{Timezone: tz, Configured: configured})
}

// handlePutCronConfig sets the URL tenant's cron timezone. An empty timezone
// clears it (back to UTC). The IANA zone is validated here so a bad name
// fails the set, not every cron tick. Reloads the mirror so the cron head
// picks the change up with no restart.
func (c *Controller) handlePutCronConfig(w http.ResponseWriter, r *http.Request) {
	if err := policy.RequireCapability(r.Context(), "opstack:*:update"); err != nil {
		auth.WriteForbidden(w, signature.ErrCapabilityDenied)
		return
	}
	ac := auth.FromContext(r.Context())
	if ac == nil || ac.TenantID == "" {
		writeJSONError(w, http.StatusInternalServerError, "tenant_id_missing", nil)
		return
	}

	var req putCronConfigRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body", map[string]any{"err": err.Error()})
		return
	}
	tz := strings.TrimSpace(req.Timezone)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_timezone", map[string]any{
				"timezone": tz,
				"err":      err.Error(),
				"hint":     `use an IANA zone name like "Asia/Tokyo" (case-sensitive)`,
			})
			return
		}
	}

	updatedBy := ""
	if ac != nil {
		updatedBy = ac.ActorID
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	// Fleet-sync producer: upload the row artifact BEFORE the tx (the outbox
	// commit is the single acceptance point; an orphaned artifact is
	// GC-recoverable). Single-node deployments (nop sink) skip this entirely.
	var fleetRef, fleetSum string
	if c.fleetEnabled() {
		ref, sum, ferr := c.fleetUploadCronSettingsUpsert(r.Context(), ac.TenantID, tz, updatedAt, updatedBy)
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
	if err := tenants.PutCronTimezoneTx(r.Context(), tx, ac.TenantID, tz, updatedAt, updatedBy, c.pu.RuntimeDialect); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "put_cron_config", map[string]any{"err": err.Error()})
		return
	}
	// Queue the fleet event in the SAME tx as the mutation.
	if c.fleetEnabled() {
		if _, qerr := c.fleetQueueEvent(r.Context(), tx,
			controlevent.TypeCronSettingsUpserted, ac.TenantID, "", 0, 0, fleetRef, fleetSum); qerr != nil {
			writeJSONError(w, http.StatusInternalServerError, "fleet_queue", map[string]any{"err": qerr.Error()})
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "commit", map[string]any{"err": err.Error()})
		return
	}
	committed = true
	if err := c.pu.Dbc.ReloadAfterWrite(); err != nil {
		c.pu.Logger.Warn("dbcache reload after cron config set failed; FS watcher will retry",
			zap.String("err", err.Error()))
	}
	writeJSON(w, http.StatusOK, cronConfigDTO{Timezone: tz, Configured: tz != ""})
}

// cronSettingsToRow projects a cron_settings row onto the JSON-row shape the
// consumer applier upserts (RowsArtifact). Mirrors the column set of the
// cron_settings table (migration 0018); updated_by is omitted when empty so
// the consumer's INSERT OR REPLACE leaves NULL rather than writing "".
func cronSettingsToRow(tenantID, tz, updatedAt, updatedBy string) map[string]any {
	row := map[string]any{
		"tenant_id":  tenantID,
		"timezone":   tz,
		"updated_at": updatedAt,
	}
	if updatedBy != "" {
		row["updated_by"] = updatedBy
	}
	return row
}

// fleetUploadCronSettingsUpsert uploads a cron_settings RowsArtifact (op=upsert)
// keyed by tenant id, mirroring fleetUploadHostnameUpsert. Returns (ref, sum).
func (c *Controller) fleetUploadCronSettingsUpsert(ctx context.Context, tenantID, tz, updatedAt, updatedBy string) (string, string, error) {
	art := controlevent.RowsArtifact{
		DB:    "runtime",
		Table: "cron_settings",
		Op:    "upsert",
		Rows:  []map[string]any{cronSettingsToRow(tenantID, tz, updatedAt, updatedBy)},
	}
	ref, sum, _, err := c.fleetUploadArtifact(ctx, "rows/cron_settings/"+tenantID, art)
	if err != nil {
		return "", "", err
	}
	return ref, sum, nil
}
