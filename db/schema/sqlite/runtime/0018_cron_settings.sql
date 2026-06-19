-- Per-tenant cron timezone.
--
-- The cron head stamps the wall-clock fields on each tick
-- (_txc.cron.hour/minute/dom/dow/month/year + the mod buckets) in UTC by
-- default. A tenant that sets a timezone here gets those fields localized
-- to it, so `WHEN @cron.hour == 9 && @cron.minute == 0` means 09:00 in that
-- zone. No row (or an empty timezone) = UTC.
--
-- Set via `txco cron config set timezone <IANA zone>` and hot-reloaded
-- through dbcache (no restart). The @cron.bucket dedup key stays UTC, so
-- fleet cron dedup is unaffected. `timezone` is an IANA name (e.g.
-- "Asia/Tokyo"), validated at set time.

CREATE TABLE IF NOT EXISTS cron_settings (
    tenant_id   TEXT PRIMARY KEY,
    timezone    TEXT NOT NULL DEFAULT '',
    updated_at  TEXT NOT NULL,
    -- actor_id of the operator who last set it. Cross-DB reference to
    -- auth.db.actors; un-enforced by SQLite.
    updated_by  TEXT
);
