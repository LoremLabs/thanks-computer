-- auth.db initial schema — Postgres dialect.
--
-- Byte-for-byte mirror of db/schema/sqlite/auth/0001_init.sql, with only
-- the unavoidable dialect differences:
--
--   * BLOB           -> BYTEA            (public_key)
--   * No FOREIGN KEY clauses. SQLite declares them but does NOT enforce
--     them (foreign_keys pragma is off, and they're cross-file anyway).
--     Postgres WOULD enforce declared FKs, which would change behaviour
--     (e.g. fleet-applied auth rows have no guaranteed parent-first
--     order). We deliberately keep the same effective semantics:
--     un-enforced. Identity integrity is the registry's job.
--
-- Everything else is intentionally identical so the Go scan/marshal
-- code is unchanged: timestamps are RFC3339 TEXT, JSON is TEXT,
-- super_admin is INTEGER 0/1 (scanned into an int, not a bool), and the
-- partial unique index uses the same WHERE clause (native in Postgres).

CREATE TABLE IF NOT EXISTS varvals (
    var TEXT,
    val TEXT,
    UNIQUE(var)
);

CREATE TABLE IF NOT EXISTS actors (
    actor_id    TEXT PRIMARY KEY,
    label       TEXT,
    kind        TEXT,
    subject     TEXT,
    tenant      TEXT,
    stack       TEXT,
    super_admin INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    revoked_at  TEXT,
    meta        TEXT
);

CREATE TABLE IF NOT EXISTS actor_keys (
    key_id      TEXT PRIMARY KEY,
    actor_id    TEXT NOT NULL,
    public_key  BYTEA NOT NULL,
    algorithm   TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    revoked_at  TEXT,
    meta        TEXT
);
CREATE INDEX IF NOT EXISTS actor_keys_actor_idx ON actor_keys(actor_id);
-- One principal per non-revoked public key. Revoked rows can collide
-- (kept for forensics).
CREATE UNIQUE INDEX IF NOT EXISTS actor_keys_public_key_idx
    ON actor_keys(public_key)
    WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS actor_memberships (
    actor_id          TEXT NOT NULL,
    tenant_id         TEXT NOT NULL,
    capabilities_json TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    revoked_at        TEXT,
    PRIMARY KEY (actor_id, tenant_id)
);
CREATE INDEX IF NOT EXISTS actor_memberships_tenant_idx
    ON actor_memberships(tenant_id);

CREATE TABLE IF NOT EXISTS actor_invitations (
    invitation_id TEXT PRIMARY KEY,
    token_hash    TEXT NOT NULL UNIQUE,
    label         TEXT,
    kind          TEXT,
    tenant_id     TEXT,
    capabilities  TEXT NOT NULL,
    created_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    consumed_at   TEXT,
    consumed_by   TEXT,
    revoked_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_actor_invitations_live
    ON actor_invitations(token_hash)
    WHERE consumed_at IS NULL AND revoked_at IS NULL;

-- Browser auth: bootstrap tokens exchange for HttpOnly session cookies.
-- See chassis/auth/registry/browser.go and internal docs/todo-admin-ui-browser-auth.md.

CREATE TABLE IF NOT EXISTS browser_bootstrap (
    token_hash         TEXT PRIMARY KEY,
    actor_id           TEXT NOT NULL,
    tenant_id          TEXT NOT NULL,
    capabilities_json  TEXT NOT NULL,
    label              TEXT,
    created_at         TEXT NOT NULL,
    expires_at         TEXT NOT NULL,
    consumed_at        TEXT,
    consumed_ip        TEXT
);
CREATE INDEX IF NOT EXISTS browser_bootstrap_expires_idx
    ON browser_bootstrap(expires_at);
CREATE INDEX IF NOT EXISTS browser_bootstrap_actor_idx
    ON browser_bootstrap(actor_id);

CREATE TABLE IF NOT EXISTS browser_sessions (
    session_id         TEXT PRIMARY KEY,
    actor_id           TEXT NOT NULL,
    tenant_id          TEXT NOT NULL,
    capabilities_json  TEXT NOT NULL,
    ua                 TEXT,
    ip                 TEXT,
    created_at         TEXT NOT NULL,
    expires_at         TEXT NOT NULL,
    revoked_at         TEXT,
    revoked_by         TEXT,
    last_seen_at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS browser_sessions_actor_idx
    ON browser_sessions(actor_id);
CREATE INDEX IF NOT EXISTS browser_sessions_tenant_idx
    ON browser_sessions(tenant_id);
CREATE INDEX IF NOT EXISTS browser_sessions_expires_idx
    ON browser_sessions(expires_at);
