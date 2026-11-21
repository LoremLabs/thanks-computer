-- attach: a workspace held by a machine that dialed in with `txco attach`
-- (chassis/attached). Two tables beside `workspaces`, in the same runtime DB,
-- because the same rule holds: identity is runtime state, and it lives where
-- the authoritative writes go.
--
-- The `workspaces` row is what says a name is attached (provider =
-- 'attached', provider_ref = the credential's id). These tables say who may
-- hold it. Which connection holds it NOW is not here at all: that is a claim
-- in the KV, liveness only.
--
-- Timestamps are TEXT RFC3339, as in `workspaces`.

-- A one-use code, made by the stack that owns the name. Only its hash is
-- kept. Spent by a conditional UPDATE that must touch exactly one row.
CREATE TABLE IF NOT EXISTS attach_codes (
    code_hash    TEXT PRIMARY KEY,          -- sha256(code) hex
    workspace_id TEXT NOT NULL,             -- workspace.ID(tenant, stack, name)
    tenant       TEXT NOT NULL,             -- tenant SLUG
    stack        TEXT NOT NULL,
    name         TEXT NOT NULL,
    label        TEXT,                      -- shown to the owner before they accept
    created_at   TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    consumed_at  TEXT
);

-- A credential: one key, enrolled for exactly one workspace. Not an actor,
-- and no membership: it can hold that workspace and do nothing else.
CREATE TABLE IF NOT EXISTS attach_credentials (
    credential_id TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL,
    tenant        TEXT NOT NULL,
    stack         TEXT NOT NULL,
    name          TEXT NOT NULL,
    public_key    TEXT NOT NULL,            -- base64 of the 32-byte Ed25519 key
    fingerprint   TEXT NOT NULL,            -- SHA256:… as ssh prints it
    label         TEXT,
    os            TEXT,
    root          TEXT,                     -- the folder, as the machine named it
    machine       TEXT,
    created_at    TEXT NOT NULL,
    revoked_at    TEXT
);

-- One live credential for a name at a time: a new enrollment revokes the
-- old one in the same transaction, and this index is what makes that a
-- guarantee rather than a habit.
CREATE UNIQUE INDEX IF NOT EXISTS attach_credentials_live_idx
    ON attach_credentials (workspace_id) WHERE revoked_at IS NULL;
