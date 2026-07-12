-- tenant_runtime_state carries per-tenant operational admission state:
-- whether a tenant may enter its stack, and (Phase 2) its node-local
-- rate/concurrency limits. It is read on the request hot path from an
-- in-memory snapshot (chassis/admission), rebuilt on every dbcache
-- reload — never queried per request.
--
-- Written by an operator/admin tool or by the fleet-sync applier
-- (entitlement.updated -> RowsArtifact upsert). NOTE: that apply path
-- uses INSERT OR REPLACE, which resets any column ABSENT from the row to
-- its DEFAULT. The defaults below are therefore deliberately ADMIT-SHAPED
-- (enabled=1, suspended=0, deny_status=403) so a partial upsert can only
-- fail OPEN (admit), never accidentally block a tenant the operator did
-- not intend to touch. A tenant with no row at all is admitted (the
-- in-memory provider's identity default). The default/_sys tenants get
-- no row on a fresh chassis, so they are admitted out of the box.

CREATE TABLE IF NOT EXISTS tenant_runtime_state (
    tenant_id          TEXT PRIMARY KEY REFERENCES tenants(tenant_id),
    enabled            INTEGER NOT NULL DEFAULT 1,   -- 0 => deny
    suspended          INTEGER NOT NULL DEFAULT 0,   -- 1 => deny
    deny_status        INTEGER NOT NULL DEFAULT 403, -- HTTP status when denied (402 | 403)
    deny_reason        TEXT    NOT NULL DEFAULT '',  -- short machine token (response header)
    -- Phase-2 reserved (unused in Phase 1; present now so Phase 2 needs
    -- no migration and the fleet-sync row shape is stable from day one):
    -- rate_limit_rps is REAL: the limiter rate is a float64 (x/time/rate),
    -- so sub-1-rps limits (e.g. --rate 50/m => 0.833) store faithfully.
    -- Postgres mirror declares this DOUBLE PRECISION for the same reason;
    -- SQLite's INTEGER affinity already tolerated fractional, PG does not.
    rate_limit_rps     REAL    NOT NULL DEFAULT 0,   -- 0 => unlimited
    rate_burst         INTEGER NOT NULL DEFAULT 0,   -- token-bucket size (int)
    concurrency_limit  INTEGER NOT NULL DEFAULT 0,   -- 0 => unlimited
    updated_at         TEXT    NOT NULL DEFAULT ''
);
