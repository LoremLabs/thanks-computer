-- control_events_outbox is the producer-side transactional outbox for
-- fleet sync. Admin handlers that mutate runtime.db append a row here
-- in the SAME tx as the mutation; a background pump
-- (chassis/controlpublish) drains pending rows by calling
-- feed.Sink.Append and writes back the broker-assigned ControlVersion
-- on success. Crash safety: anything in the outbox WILL be published
-- once the broker is reachable; anything not in the outbox was never
-- accepted.
--
-- See docs (overlay repo): internal docs/todo-fleet-sync-producer.md.

CREATE TABLE control_events_outbox (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    -- event_id is the producer-assigned semantic identity (UUIDv7).
    -- Rides through retries unchanged. Used as Nats-Msg-Id in the
    -- JetStream Sink and as the dedup key in the consumer's
    -- applied_events table.
    event_id                  TEXT NOT NULL UNIQUE,
    event_type                TEXT NOT NULL,
    tenant_id                 TEXT,
    stack_id                  TEXT,
    -- For typed-version mutations: the new version (e.g. the
    -- activated stack_version) and the prior value the producer saw.
    -- base_version is recorded but not enforced in v1; future CAS
    -- checks (P-later) can layer on top without a wire-format change.
    version                   INTEGER,
    base_version              INTEGER,
    artifact_ref              TEXT,
    checksum                  TEXT,        -- 'sha256:<hex>'
    -- payload_json is the canonical Event JSON minus control_version
    -- (broker assigns that on publish). Stored as a blob so schema
    -- evolution of the Event struct doesn't require another column
    -- migration; the decomposed columns above are for diagnostics
    -- and indexed lookup only.
    payload_json              BLOB NOT NULL,
    created_at                TEXT NOT NULL,
    -- Pump bookkeeping; visible to operators for stuck-publish
    -- debugging. attempt_count strictly increases on each failed
    -- attempt; last_error is the short error string from the last
    -- failed Append. NULL when never attempted.
    attempt_count             INTEGER NOT NULL DEFAULT 0,
    last_error                TEXT,
    last_attempt_at           TEXT,
    -- After successful publish:
    published_control_version INTEGER,     -- broker-assigned; NULL ⇒ pending
    published_at              TEXT
);

-- Partial index — pump's hot path is the pending set, which stays
-- small at operator-driven mutation rates. Full-table scans for
-- audit/diagnostics walk the table without the index.
CREATE INDEX idx_outbox_pending
    ON control_events_outbox(id)
    WHERE published_control_version IS NULL;
