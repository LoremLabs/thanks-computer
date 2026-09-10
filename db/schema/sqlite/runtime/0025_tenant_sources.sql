-- tenant_sources: declared external-source watchers (a remote IMAP mailbox
-- today; other cursor-bearing sources later) plus their runtime poll state.
--
-- Two column groups with DIFFERENT writers, and the split is load-bearing:
--
--   declared — written ONLY by the SOURCES/ store-seed materializer (srcseed)
--     during activation. This is config the tenant committed to OPS; a
--     redeploy re-asserts it.
--
--   runtime  — written ONLY by the source poller: `cursor` (how far it has
--     read the mailbox) and the claim/lease (`status`/`claimed_by`/
--     `claimed_at`/`next_poll_at`) so exactly one fleet node polls a given
--     source at a time. Because the materializer never touches these, a
--     redeploy of the declaration never rewinds the cursor.
--
-- `tenant` is the tenant SLUG (as scheduled_events stores it), so the poller
-- can materialize the mailbox secret (MaterializeForOpSlug) and route the
-- fired run into `<tenant>/_source/0` without a second lookup.
CREATE TABLE IF NOT EXISTS tenant_sources (
    source_id        TEXT PRIMARY KEY,   -- deterministic: hash(tenant, stack, pack, declared_id)
    tenant           TEXT NOT NULL,      -- tenant SLUG
    stack            TEXT NOT NULL,
    pack             TEXT NOT NULL,      -- SOURCES/<pack>.jsonl this row was declared in
    declared_id      TEXT NOT NULL,      -- the "id" field of the pack line
    kind             TEXT NOT NULL,      -- "imap"
    config           TEXT NOT NULL,      -- JSON: {host,port,tls,user,secret,mailbox,on_processed,...}
    enabled          INTEGER NOT NULL DEFAULT 1,
    every_seconds    INTEGER NOT NULL DEFAULT 300,
    declared_version INTEGER NOT NULL DEFAULT 0,
    retired_at       TEXT,               -- soft-retire: set when a redeploy drops the line; cursor preserved

    -- runtime poll state (poller-only) --------------------------------------
    cursor           TEXT,               -- opaque JSON, kind-specific ({"uidvalidity":N,"uid_hwm":M} for imap)
    status           TEXT NOT NULL DEFAULT 'idle',  -- idle | claimed
    claimed_by       TEXT,
    claimed_at       TEXT,
    next_poll_at     TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    last_poll_at     TEXT,
    last_error       TEXT,
    attempts         INTEGER NOT NULL DEFAULT 0,
    created_at       TEXT NOT NULL DEFAULT '1970-01-01T00:00:00Z'
);

-- The natural key: one row per declared source. srcseed upserts on it.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_sources_declared_uq
    ON tenant_sources (tenant, stack, pack, declared_id);

-- The poller's due-scan: enabled, live, unclaimed rows ordered by next_poll_at.
CREATE INDEX IF NOT EXISTS tenant_sources_due_idx
    ON tenant_sources (next_poll_at)
    WHERE enabled = 1 AND retired_at IS NULL AND status = 'idle';
