-- Postgres mirror of sqlite/auth/0007. One difference: generation and the
-- two call counters are BIGINT, because a caller may pass its own generation
-- (a lease counter, a timestamp) and SQLite's INTEGER is 64-bit. Timestamps
-- stay RFC3339 TEXT and JSON stays TEXT so the Go scan code is one path (see
-- 0001_init.sql).
--
-- Run grants: what ONE piece of work may ask the chassis for, until when,
-- and within what budget. A stack mints one when it dispatches the work
-- (txco://delegate/mint); every request the work makes reads this row, so
-- revoking it, closing it or spending its budget takes effect on the next
-- request. See chassis/authn (rungrants.go).
--
-- A run grant only narrows. Its allowlist names nothing its principal holds
-- no standing grant for (0006_resource_grants.sql), and a grant narrowed
-- from another (parent_grant) names nothing the parent does not, expires no
-- later, and takes its budget out of the parent's.
--
-- The row is the authority. The token that travels with the work
-- (chassis/rungrant) only identifies the row: it holds no allowlist and no
-- budget, so nothing a holder does to a token widens what it may ask for.
--
-- minted_by is the base name of the minting stack and decides who may
-- revoke or close the row. stack is the minting rule's app stack: the
-- workspace the run is sent to is one of that stack's, and a secret the run
-- asks for is resolved as that stack would resolve it.
CREATE TABLE IF NOT EXISTS run_grants (
    id            TEXT PRIMARY KEY,           -- rgr_<hxid>
    tenant_id     TEXT NOT NULL,
    principal_id  TEXT NOT NULL,              -- whom the work acts for
    minted_by     TEXT NOT NULL,
    stack         TEXT NOT NULL,
    -- The work. Minting the same run again raises the generation and closes
    -- the older rows, so a stale copy of the work is refused.
    run           TEXT NOT NULL,
    generation    BIGINT NOT NULL,
    -- Where the work runs. workspace_id is the workspaces row's id
    -- (chassis/workspace.ID); both are '' for work that runs nowhere.
    workspace_id  TEXT NOT NULL DEFAULT '',
    workspace     TEXT NOT NULL DEFAULT '',
    node_class    TEXT NOT NULL DEFAULT 'unreviewed',  -- reviewed | unreviewed
    -- Names the run's own directory where grants are presented as files.
    -- Random, and never written to an envelope or a token: knowing a grant's
    -- id must not be enough to find its files.
    file_key      TEXT NOT NULL,
    allowlist     TEXT NOT NULL,              -- JSON array: ["crm.lookup","secret:DB_DSN"]
    budget_calls  BIGINT NOT NULL,
    spent_calls   BIGINT NOT NULL DEFAULT 0,
    parent_grant  TEXT,                       -- the grant this one was narrowed from
    depth         INTEGER NOT NULL DEFAULT 0,
    trace_id      TEXT NOT NULL DEFAULT '',   -- the request that minted it
    issued_at     TEXT NOT NULL,
    expires_at    TEXT NOT NULL,
    closed_at     TEXT,                       -- the work ended, or a newer generation replaced it
    close_reason  TEXT NOT NULL DEFAULT '',
    revoked_at    TEXT,
    UNIQUE (tenant_id, minted_by, run, generation),
    UNIQUE (file_key)
);
CREATE INDEX IF NOT EXISTS run_grants_principal_idx ON run_grants(tenant_id, principal_id);
CREATE INDEX IF NOT EXISTS run_grants_parent_idx ON run_grants(tenant_id, parent_grant);
CREATE INDEX IF NOT EXISTS run_grants_workspace_idx ON run_grants(tenant_id, workspace_id);

-- run_heads holds one row per run: its newest generation. Minting writes
-- this row FIRST, so two mints of the same run take turns — the second waits
-- for the first to commit, then sees everything it wrote. Without it, two
-- concurrent mints could each miss the other's row and both stay live.
CREATE TABLE IF NOT EXISTS run_heads (
    tenant_id   TEXT NOT NULL,
    minted_by   TEXT NOT NULL,
    run         TEXT NOT NULL,
    generation  BIGINT NOT NULL,
    grant_id    TEXT NOT NULL,              -- the run_grants row of that generation
    PRIMARY KEY (tenant_id, minted_by, run)
);
