-- tenants is the routing topology: one row per (slug, future hostname,
-- future routing knobs). Lives in runtime.db so the data plane can
-- resolve hostname→tenant without opening auth.db. Identity-side
-- references to tenant_id from auth.db cross the DB boundary by
-- convention — un-enforced FKs, looked up via two-step queries.

CREATE TABLE IF NOT EXISTS tenants (
    tenant_id   TEXT PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    name        TEXT,
    created_at  TEXT NOT NULL,
    revoked_at  TEXT
);

-- Seed the default tenant so a fresh chassis works without a separate
-- boot step. First dev-enrollment on an empty chassis claims it
-- (super_admin + membership in auth.db).
INSERT OR IGNORE INTO tenants (tenant_id, slug, name, created_at)
VALUES ('tnt_default', 'default', 'Default',
        strftime('%Y-%m-%dT%H:%M:%fZ','now'));
