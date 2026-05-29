-- runtime.db initial schema.
--
-- Content-side tables: ops (the executable rules read by the runtime),
-- plus the migration changeset tracker. Tenants and stacks land in
-- subsequent migrations so each concern stays one file.
--
-- See `db/schema/sqlite/auth/` for the matching identity schema.

CREATE TABLE IF NOT EXISTS varvals (
    var TEXT,
    val TEXT,
    UNIQUE(var)
);

-- ops is the runtime read model: each row is one executable rule at a
-- given (stack, scope) with optional mock_req/mock_res payloads. The
-- admin API populates it by materialising the active stack_version's
-- files inside one BEGIN IMMEDIATE; the data plane (dbcache mirror)
-- reads from here on the hot path.
CREATE TABLE IF NOT EXISTS ops (
    stack       TEXT,
    scope       INTEGER,
    name        TEXT NOT NULL DEFAULT '',
    txcl        TEXT,
    mock_req    TEXT,
    mock_res    TEXT,
    tenant_id   TEXT,
    UNIQUE(stack, scope, txcl)
);
CREATE INDEX IF NOT EXISTS ops_stack_scope_index ON ops(stack, scope);
-- Per-name uniqueness within a stage, but only for non-empty names —
-- legacy rows with name='' coexist via the (stack, scope, txcl) UNIQUE.
CREATE UNIQUE INDEX IF NOT EXISTS ops_stack_scope_name_idx
    ON ops(stack, scope, name)
    WHERE name != '';
CREATE INDEX IF NOT EXISTS ops_tenant_idx ON ops(tenant_id);
