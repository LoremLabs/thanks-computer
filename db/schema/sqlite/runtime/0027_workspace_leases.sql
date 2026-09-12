-- workspace_leases: one row per granted attachment to a workspace — the
-- interactive terminal behind `workspace://<name>/attach` today; any other
-- long-lived binding (a workspace-local service, a running task) later.
-- See chassis/workspace/lease.go.
--
-- A lease says who holds the workspace, from which WebSocket session on
-- which chassis node, until when (`expires_at`, the attach op's
-- max_duration), and when it last proved it was alive (`last_heartbeat`,
-- refreshed every 60 s while the binding lives). A lease two heartbeats
-- silent is dead: a node that crashed mid-session cannot pin a workspace
-- forever. Rows are released (`released_at`), never deleted, so "who was in
-- this machine, when" stays answerable.
--
-- `tenant` is the tenant SLUG and `stack` the OWNING app stack (never the
-- `_websocket` inlet sub-stack), so `workspace_id` matches the workspaces
-- row. Timestamps are TEXT RFC3339 as in workspaces. No foreign keys: a
-- lease outlives nothing and is only ever released.
CREATE TABLE IF NOT EXISTS workspace_leases (
    lease_id       TEXT PRIMARY KEY,        -- the attachment id
    tenant         TEXT NOT NULL,           -- tenant SLUG
    stack          TEXT NOT NULL,           -- owning app stack
    name           TEXT NOT NULL,           -- the txcl-visible workspace name
    workspace_id   TEXT NOT NULL,           -- sha256(tenant\0stack\0name) hex, = workspaces.workspace_id
    kind           TEXT NOT NULL,           -- "pty" | …
    session_id     TEXT NOT NULL,           -- the WebSocket session holding the socket
    node_id        TEXT NOT NULL,           -- the chassis node holding that socket
    run_id         TEXT,                    -- the workspace run the process lives in
    started_at     TEXT NOT NULL,
    expires_at     TEXT NOT NULL,           -- started_at + max_duration
    last_heartbeat TEXT NOT NULL,
    released_at    TEXT                     -- NULL while the binding lives
);

-- The reaper's question: does anything live hold this workspace?
CREATE INDEX IF NOT EXISTS workspace_leases_workspace_idx
    ON workspace_leases (workspace_id, released_at);

-- Admin listings: a tenant's attachments, newest first.
CREATE INDEX IF NOT EXISTS workspace_leases_tenant_idx
    ON workspace_leases (tenant, started_at);
