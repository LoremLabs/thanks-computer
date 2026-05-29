-- Tenant-scoped secret store.
--
-- Each tenant_secrets row binds one (tenant_id, stack, name) →
-- secret identity at a moment in time. Values are NEVER stored as
-- plaintext: each tenant_secret_versions row carries an AES-256-GCM
-- ciphertext encrypted under a per-secret DEK (Data Encryption Key),
-- and the DEK itself is wrapped (AES-256-GCM) by a host-local master
-- key. Both layers carry AAD bound to row identity so a stolen blob
-- cannot be moved between tenants, secrets, or versions without GCM
-- verification failing. The master key lives outside the DB
-- (operator-owned host file); the DB alone is useless to a thief.
--
-- See internal docs/todo-secret-store.md for the full design.
--
-- name is UPPER_SNAKE by convention and **immutable** after create
-- (rename = create-new + revoke-old). description can be updated.
-- value can be rotated (which writes a new tenant_secret_versions
-- row with version_no = previous + 1). Reads always target the
-- latest non-revoked version.
--
-- stack is NULLABLE. NULL = tenant-wide (the default in v1 admin
-- UI); non-NULL = scoped to that one stack. The resolver picks the
-- narrower scope first and falls back to tenant-wide.
--
-- created_by stores the actor_id of the admin who created the
-- secret. The reference is into auth.db.actors and is cross-DB by
-- convention (same pattern as tenant_hostnames).
--
-- key_version records which master-key generation wrapped this
-- secret's DEK. v1 ships a single MK version (== 1); multi-version
-- overlap is forward-compatible for online MK rotation.

CREATE TABLE IF NOT EXISTS tenant_secrets (
    secret_id        TEXT PRIMARY KEY,            -- 'sec_' + hxid
    tenant_id        TEXT NOT NULL,
    stack            TEXT,                        -- NULL = tenant-wide
    name             TEXT NOT NULL,               -- UPPER_SNAKE, immutable
    description      TEXT,
    created_at       TEXT NOT NULL,               -- RFC3339
    created_by       TEXT,                        -- actor_id (cross-DB)
    revoked_at       TEXT,                        -- soft-delete; NULL = active
    last_rotated_at  TEXT,
    key_version      INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE IF NOT EXISTS tenant_secret_versions (
    version_id   TEXT PRIMARY KEY,                -- 'sec_v_' + hxid
    secret_id    TEXT NOT NULL REFERENCES tenant_secrets(secret_id),
    version_no   INTEGER NOT NULL,
    nonce        BLOB NOT NULL,                   -- 12-byte AES-GCM nonce (outer)
    ciphertext   BLOB NOT NULL,                   -- ciphertext ‖ GCM tag
    wrapped_dek  BLOB NOT NULL,                   -- DEK encrypted with MK
    dek_nonce    BLOB NOT NULL,                   -- 12-byte nonce (inner / wrap)
    created_at   TEXT NOT NULL,
    revoked_at   TEXT
);

-- Active-uniqueness on (tenant_id, stack, name). NULL stack
-- participates in uniqueness via COALESCE(stack, ''): the empty
-- string can never collide with a real stack name (stack names are
-- non-empty identifiers), so '' stably represents the tenant-wide
-- bucket inside the index. Net effect: one tenant-wide row AND one
-- row per (stack, name) coexist for the same name, but a second
-- tenant-wide row with the same name is rejected. Revoked rows
-- preserve audit history; the partial-index WHERE excludes them
-- so the resolver lookup is a covered point read.
--
-- (Without COALESCE, ANSI-SQL NULL != NULL would treat every
-- tenant-wide row as distinct from every other — silently allowing
-- duplicate names per tenant. Expression indexes are supported in
-- both SQLite and Postgres with identical syntax.)
CREATE UNIQUE INDEX IF NOT EXISTS tenant_secrets_active_name_idx
    ON tenant_secrets (tenant_id, COALESCE(stack, ''), name)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS tenant_secrets_tenant_idx
    ON tenant_secrets (tenant_id);

CREATE INDEX IF NOT EXISTS tenant_secret_versions_by_secret_idx
    ON tenant_secret_versions (secret_id, version_no);
