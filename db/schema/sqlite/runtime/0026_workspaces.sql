-- workspaces: the identity table behind `workspace://<name>/<verb>` — the
-- (tenant, stack, name) → provider-side reference mapping, plus the
-- lifecycle bookkeeping the chassis keeps about it (last use, current run,
-- checkpoint, destruction). See chassis/workspace/store.go.
--
-- One row per workspace identity. The chassis `Manager` looks a row up on
-- first use, creates the provider-side environment when there is none (or
-- the row is destroyed), and touches `last_used_at`/`run_id` after every
-- exec. Rows are records, never locks: concurrent execs on one workspace
-- are allowed (the author owns file-level conflicts).
--
-- `tenant` is the tenant SLUG (as tenant_sources stores it) — the processor
-- has the slug in request scope and never needs the id here. Timestamps are
-- TEXT RFC3339 (lexicographic == chronological) so the reaper's idle scan is
-- one dialect-agnostic query. No foreign keys: a workspace outlives nothing
-- and the reaper is the only deleter.
CREATE TABLE IF NOT EXISTS workspaces (
    workspace_id   TEXT PRIMARY KEY,        -- sha256(tenant\0stack\0name) hex
    tenant         TEXT NOT NULL,           -- tenant SLUG
    stack          TEXT NOT NULL,
    name           TEXT NOT NULL,           -- the txcl-visible name (may contain "/")
    provider       TEXT NOT NULL,           -- "local" | "sprites" | …
    provider_ref   TEXT NOT NULL,           -- the provider's own id (a sprite name, a directory)
    runtime        TEXT,
    network        TEXT NOT NULL DEFAULT 'public',
    status         TEXT NOT NULL,           -- created | running | warm | cold | destroyed
    run_id         TEXT,                    -- the current/last run id (see Manager)
    checkpoint_ref TEXT,                    -- the provider's id for the latest checkpoint
    created_at     TEXT NOT NULL,
    last_used_at   TEXT NOT NULL,
    destroyed_at   TEXT
);

-- The identity: one row per (tenant, stack, name). The Manager upserts on it.
CREATE UNIQUE INDEX IF NOT EXISTS workspaces_identity_idx
    ON workspaces (tenant, stack, name);

-- The reaper's idle scan: live rows ordered by last use.
CREATE INDEX IF NOT EXISTS workspaces_last_used_idx
    ON workspaces (status, last_used_at);
