-- Hostname → tenant routing layer.
--
-- Each row binds one (hostname → tenant_id, stack) at a moment in
-- time. Surrogate id PK + partial unique index on hostname (only when
-- active) lets the same hostname pass through multiple owners over
-- time without losing the audit history: revoking a row releases the
-- hostname for re-assignment, but the prior row stays for
-- "who held foo.local last quarter?" debugging.
--
-- created_by stores the actor_id of the admin who claimed the
-- hostname. The reference is into auth.db.actors and is cross-DB by
-- convention — SQLite cannot enforce FKs across files without ATTACH,
-- which we don't use on the hot path. The handler reads
-- auth.FromContext(r).ActorID and stamps it here.
--
-- Lookups go through the dbcache mirror; see
-- chassis/server/ingress/db_resolver.go.

CREATE TABLE IF NOT EXISTS tenant_hostnames (
    id          TEXT PRIMARY KEY,
    hostname    TEXT NOT NULL,
    tenant_id   TEXT NOT NULL REFERENCES tenants(tenant_id),
    stack       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    created_by  TEXT,
    revoked_at  TEXT
);

-- Only one ACTIVE row per hostname; revoked rows preserve audit
-- history. The resolver's WHERE revoked_at IS NULL clause matches the
-- index, so the lookup is a covered point read.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_hostnames_active_hostname_idx
    ON tenant_hostnames(hostname)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS tenant_hostnames_tenant_idx
    ON tenant_hostnames(tenant_id);
