-- applied_events is the consumer-side semantic-dedup record for
-- fleet sync. Before applying any control event the applier inserts
-- (event_id, control_version, applied_at) here. If the row already
-- exists (INSERT OR IGNORE, RowsAffected==0), this event has been
-- applied before — skip the apply, still advance the cursor + ack
-- the broker message. The applied_events row + cursor advance + any
-- data-row mutations live in the SAME tx, so a partial apply rolls
-- back cleanly.
--
-- Why this is load-bearing: broker-side dedup (JetStream
-- Nats-Msg-Id) is time-bounded. A pump that recovers after the
-- dedup window expires can republish the same event_id and JetStream
-- will assign a fresh stream_sequence. The consumer-side check here
-- recognises the event_id as already-applied regardless of how the
-- broker handled the redelivery.
--
-- See docs (overlay repo): internal docs/todo-fleet-sync-producer.md.

CREATE TABLE applied_events (
    event_id        TEXT PRIMARY KEY,
    control_version INTEGER NOT NULL,
    applied_at      TEXT NOT NULL
);

-- Lookup by control_version supports operational queries ("what
-- events landed at versions X..Y?"). Not hot-path for the applier
-- itself (which checks by event_id PK).
CREATE INDEX idx_applied_control_version
    ON applied_events(control_version);
