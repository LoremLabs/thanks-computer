package tenants

// Per-tenant cron timezone (cron_settings). The cron head stamps the
// wall-clock cron fields in UTC by default; a tenant with a timezone set
// here gets them localized. Empty/absent = UTC. See
// db/schema/sqlite/runtime/0018_cron_settings.sql and the cron head
// (chassis/server/personality/cron). rowQueryer is shared with dns_store.go.

import (
	"context"
	"database/sql"
	"errors"
)

// LoadCronTimezone returns the tenant's configured cron timezone (an IANA
// name like "Asia/Tokyo"). ok=false when no row exists or the timezone is
// empty — callers then treat the tenant as UTC. Works on the live runtime
// DB or the dbcache snapshot.
func LoadCronTimezone(ctx context.Context, q rowQueryer, tenantID string) (tz string, ok bool, err error) {
	err = q.QueryRowContext(ctx,
		`SELECT timezone FROM cron_settings WHERE tenant_id = ?`, tenantID).Scan(&tz)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return tz, tz != "", nil
}

// CronSettingsRow is a tenant's full cron_settings row — used by the fleet
// resync producer to re-publish the current value verbatim.
type CronSettingsRow struct {
	Timezone  string
	UpdatedAt string
	UpdatedBy string
}

// LoadCronSettings returns the full cron_settings row for a tenant. ok=false
// when no row exists (the tenant never configured a timezone). Unlike
// LoadCronTimezone, a present-but-empty timezone still returns ok=true — an
// explicitly-cleared row is real state worth re-asserting on resync.
func LoadCronSettings(ctx context.Context, q rowQueryer, tenantID string) (CronSettingsRow, bool, error) {
	var r CronSettingsRow
	err := q.QueryRowContext(ctx,
		`SELECT timezone, updated_at, COALESCE(updated_by, '')
		   FROM cron_settings WHERE tenant_id = ?`, tenantID).
		Scan(&r.Timezone, &r.UpdatedAt, &r.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return CronSettingsRow{}, false, nil
	}
	if err != nil {
		return CronSettingsRow{}, false, err
	}
	return r, true, nil
}

// PutCronTimezoneTx upserts the tenant's cron timezone. An empty tz clears it
// (the head falls back to UTC). The caller supplies updatedAt (RFC3339 UTC) so
// the row matches the fleet-sync artifact uploaded before the tx, and validates
// the zone name (the admin endpoint does, via time.LoadLocation) so a bad zone
// fails the set rather than every tick.
func PutCronTimezoneTx(ctx context.Context, tx *sql.Tx, tenantID, tz, updatedAt, updatedBy string) error {
	if tenantID == "" {
		return errors.New("tenants: PutCronTimezoneTx requires tenant_id")
	}
	var byArg any
	if updatedBy != "" {
		byArg = updatedBy
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO cron_settings (tenant_id, timezone, updated_at, updated_by)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(tenant_id) DO UPDATE SET
		     timezone   = excluded.timezone,
		     updated_at = excluded.updated_at,
		     updated_by = excluded.updated_by`,
		tenantID, tz, updatedAt, byArg)
	return err
}
