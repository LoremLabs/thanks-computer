-- Connections: a principal's identity at an outside service (chassis/connection).
-- Mirrors db/schema/sqlite/auth/0009_connections.sql: one row per
-- (tenant, principal, provider, label) holding only the CURRENT material,
-- sealed with the tenant secrets' envelope; renewal updates it in place.

CREATE TABLE IF NOT EXISTS connections (
    id                 TEXT PRIMARY KEY,             -- con_<hxid>
    tenant_id          TEXT NOT NULL,
    principal_id       TEXT NOT NULL,                -- any principal: user:…, service:…
    provider           TEXT NOT NULL,                -- PROVIDERS/<name>.yaml, by name
    label              TEXT NOT NULL DEFAULT '',     -- several accounts per provider, later
    kind               TEXT NOT NULL,                -- oauth2 | github_app_installation
    subject            TEXT NOT NULL DEFAULT '',     -- the provider's stable id for the identity
    account            TEXT NOT NULL DEFAULT '',     -- its display name
    status             TEXT NOT NULL,                -- active | uncertain | reconnect
    scopes             TEXT NOT NULL DEFAULT '[]',   -- JSON array, as granted
    generation         BIGINT NOT NULL DEFAULT 1,    -- +1 on every write of the material
    expires_at         TEXT,                         -- the current token's; NULL = no expiry
    lifetime_s         BIGINT NOT NULL DEFAULT 0,    -- expires_in when issued
    refresh_expires_at TEXT,
    claim_id           TEXT,                         -- the renewal in progress
    claim_until        TEXT,
    material           TEXT NOT NULL,                -- the sealed envelope (JSON, base64 fields)
    created_by         TEXT NOT NULL,                -- the managing stack
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS connections_identity_idx
    ON connections (tenant_id, principal_id, provider, label);
CREATE INDEX IF NOT EXISTS connections_provider_idx
    ON connections (tenant_id, provider);

-- A sign-in in progress: consumed exactly once, before the code is
-- exchanged, so a replayed state or a racing callback stops here.
CREATE TABLE IF NOT EXISTS connection_flows (
    nonce_hash    TEXT PRIMARY KEY,                  -- sha256 of the state's nonce
    tenant_id     TEXT NOT NULL,
    principal_id  TEXT NOT NULL,
    provider      TEXT NOT NULL,
    label         TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL,
    stack         TEXT NOT NULL,                     -- the managing stack that started it
    redirect_uri  TEXT NOT NULL,
    browser_hash  TEXT NOT NULL,                     -- sha256 of the flow cookie
    initiator     TEXT NOT NULL DEFAULT '',          -- who the stack says is connecting
    expires_at    TEXT NOT NULL,
    consumed_at   TEXT
);

CREATE INDEX IF NOT EXISTS connection_flows_expiry_idx
    ON connection_flows (expires_at);
