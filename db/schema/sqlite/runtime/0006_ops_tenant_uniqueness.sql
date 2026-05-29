-- Tenant-scope the ops uniqueness keys.
--
-- 0001 keyed ops by (stack, scope, txcl) and a partial unique index on
-- (stack, scope, name) — both tenant-blind. Stack names are therefore a
-- single global namespace: two tenants that both name a stack `web`
-- share rows, and the data plane's tenant-scoped lookup could be
-- defeated by colliding on another tenant's stack name. tenant_id must
-- be part of stack identity so each tenant owns an independent
-- namespace and activation of two same-named stacks can't collide.
--
-- A table-level UNIQUE constraint can't be altered in place in SQLite,
-- so rebuild the table. ops has no foreign keys, so a plain
-- rename/copy/drop is safe. The data plane reads the dbcache mirror,
-- not this file's table directly, so no online-migration concern.

ALTER TABLE ops RENAME TO ops_old;

CREATE TABLE ops (
    stack       TEXT,
    scope       INTEGER,
    name        TEXT NOT NULL DEFAULT '',
    txcl        TEXT,
    mock_req    TEXT,
    mock_res    TEXT,
    tenant_id   TEXT,
    UNIQUE(stack, scope, txcl, tenant_id)
);

INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id)
    SELECT stack, scope, name, txcl, mock_req, mock_res, tenant_id FROM ops_old;

DROP TABLE ops_old;

CREATE INDEX IF NOT EXISTS ops_stack_scope_index ON ops(stack, scope);
-- Per-name uniqueness within a stage stays partial (legacy name='' rows
-- coexist via the txcl key) but is now per-tenant.
CREATE UNIQUE INDEX IF NOT EXISTS ops_stack_scope_name_idx
    ON ops(stack, scope, name, tenant_id)
    WHERE name != '';
CREATE INDEX IF NOT EXISTS ops_tenant_idx ON ops(tenant_id);
