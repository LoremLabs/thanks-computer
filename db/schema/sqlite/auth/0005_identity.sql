-- Stack-plane identity: the users, principal bindings and credentials a
-- TENANT's stacks manage (txco://user/*, txco://credential/*) and that the
-- protocol heads will authenticate against. See chassis/authn.
--
-- This is a different plane from the tables above it in this database.
-- actors / actor_keys / actor_memberships / oidc_subjects are the ACCOUNT
-- plane: who may administer a tenant. Nothing here references them and
-- nothing there references these — the tenant is the boundary between the
-- two, and no row crosses it.
--
-- Conventions match the rest of auth.db: ids are opaque TEXT, timestamps are
-- RFC3339 TEXT, booleans are INTEGER 0/1, JSON is TEXT. tenant_id references
-- runtime.db tenants(tenant_id) cross-DB, un-enforced.
--
-- created_by is the BASE NAME of the stack that wrote the row (`web` for
-- both `web` and its `web/canary` slot), stamped by the chassis from the
-- dispatching rule — never from the envelope. A principal's rows all come
-- from one stack, and only that stack may change them.

-- users holds durable state about a HUMAN: a stable id, a display name, and
-- whether they are active. Other principals (a pony, a service) have
-- bindings and credentials but no row here.
CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,           -- usr_<hxid>
    tenant_id     TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    status        TEXT NOT NULL DEFAULT 'active',  -- active | disabled
    created_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS users_tenant_idx ON users(tenant_id);

-- principal_bindings maps an external identifier to a principal. It keys on
-- principal_id, not a user id, because a pony's mailbox address binds to the
-- pony. Kinds built: email (issuer NULL) and oidc. Reserved, not built: did,
-- atproto_handle. Nothing here is OIDC-specific.
CREATE TABLE IF NOT EXISTS principal_bindings (
    id            TEXT PRIMARY KEY,           -- bnd_<hxid>
    tenant_id     TEXT NOT NULL,
    principal_id  TEXT NOT NULL,              -- user:usr_… | pony:… | service:…
    kind          TEXT NOT NULL,
    issuer        TEXT,                       -- NULL for email
    subject       TEXT NOT NULL,
    verified      INTEGER NOT NULL DEFAULT 0,
    metadata      TEXT,
    created_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    revoked_at    TEXT
);
-- One live binding per identifier in a tenant. An expression index because
-- issuer is NULL for email and both engines treat NULLs as distinct in a
-- plain UNIQUE; partial so a revoked identifier can be bound again.
CREATE UNIQUE INDEX IF NOT EXISTS principal_bindings_subject_idx
    ON principal_bindings(tenant_id, kind, COALESCE(issuer, ''), subject)
    WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS principal_bindings_principal_idx
    ON principal_bindings(tenant_id, principal_id);

-- credentials records ONLY the principal a credential authenticates as.
-- Possession and delegation are not modeled. short_id is embedded in the
-- issued password, so a login is one lookup and one argon2 verify; it is
-- unique within its principal (revoked rows included, so an old password can
-- never come to name a new credential) and it is not a secret.
CREATE TABLE IF NOT EXISTS credentials (
    id            TEXT PRIMARY KEY,           -- crd_<hxid>
    tenant_id     TEXT NOT NULL,
    principal_id  TEXT NOT NULL,
    short_id      TEXT NOT NULL,
    kind          TEXT NOT NULL,              -- app_password
    secret_hash   TEXT NOT NULL,              -- PHC argon2id (chassis/apppass)
    scopes        TEXT NOT NULL,              -- JSON array of domain:instance:action
    label         TEXT NOT NULL DEFAULT '',
    created_by    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_used_at  TEXT,
    revoked_at    TEXT,
    UNIQUE (tenant_id, principal_id, short_id)
);
