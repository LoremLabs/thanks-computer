-- Hostname verification layer.
--
-- Without verification, anyone with `hostname:*:write` in their tenant
-- can claim arbitrary hostnames (google.com, a competitor's domain,
-- etc.) — the 409 conflict only fires if some other tenant on the
-- same chassis got there first. Verification closes that gap by
-- requiring the operator to demonstrate control over either:
--
--   * the DNS zone (via a TXT record at _txco-verify.<hostname>), or
--   * the HTTP serving for the hostname (via a token served at
--     /.well-known/txco-verify/<token>, fetched by the chassis).
--
-- See chassis/tenants/verifier.go for the two verification flows.
-- Routing behavior toward unverified rows is config-gated by
-- --require-hostname-verification (permissive default in dev,
-- strict in production).

-- Add verified_at to tenant_hostnames so the resolver's JOIN can
-- filter cheaply on the hot path without a per-request challenge
-- lookup. Verification writes this column AND a verified_at on the
-- challenge row that proved it.
ALTER TABLE tenant_hostnames ADD COLUMN verified_at TEXT;

-- One row per verification attempt's worth of state. Same
-- audit-history shape as tenant_hostnames: surrogate id PK + partial
-- unique index keyed on the active subset only. Verified, revoked,
-- and expired rows all stay in the table so an operator can audit
-- "who tried what, when?" by hostname.
CREATE TABLE IF NOT EXISTS tenant_hostname_challenges (
    id            TEXT PRIMARY KEY,
    hostname_id   TEXT NOT NULL REFERENCES tenant_hostnames(id),
    method        TEXT NOT NULL
                  CHECK (method IN ('dns-txt','http-01')),
    -- 160-bit URL-safe token. UNIQUE because the public
    -- /.well-known/txco-verify/<token> handler does a point lookup on
    -- this column alone — we don't want token collisions across
    -- hostnames to make the lookup ambiguous.
    token         TEXT NOT NULL UNIQUE,
    created_at    TEXT NOT NULL,
    -- actor_id of whoever issued the challenge. Cross-DB reference
    -- to auth.db.actors; un-enforced by SQLite. See
    -- feedback_audit_actor_id.md.
    created_by    TEXT,
    expires_at    TEXT NOT NULL,
    -- Most recent verification attempt. NULL until /verify runs.
    attempted_at  TEXT,
    -- Truncated error from the last failed attempt; NULL on success.
    last_error    TEXT,
    -- Stamped once when verification succeeds; never overwritten.
    verified_at   TEXT,
    -- Soft-revoke. Issuing a new challenge for the same (hostname,
    -- method) revokes the prior row. Verified rows are NOT revoked —
    -- their verified_at IS NOT NULL takes them out of the active
    -- partial index naturally.
    revoked_at    TEXT
);

-- "One active challenge per (hostname, method)." Active = not
-- verified AND not revoked. The application layer relies on this
-- being the safety net: it does revoke-then-insert inside one
-- BEGIN IMMEDIATE transaction, but a racing insert would fail here
-- rather than producing two competing live challenges.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_hostname_challenges_active_idx
    ON tenant_hostname_challenges(hostname_id, method)
    WHERE verified_at IS NULL AND revoked_at IS NULL;

-- Covering index for listing all challenges for a hostname (history
-- mode in the admin API).
CREATE INDEX IF NOT EXISTS tenant_hostname_challenges_hostname_idx
    ON tenant_hostname_challenges(hostname_id);
