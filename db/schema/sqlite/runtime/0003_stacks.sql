-- Versioned opstack control plane.
--
-- Each `stacks` row is one (tenant × stack name) pair. Each
-- `stack_versions` row is an immutable snapshot identified by a
-- per-stack `version_number` (1, 2, 3 …) shown in API/UI/CLI; the
-- autoincrement `version_id` is used only for joins. The single source
-- of truth for "currently live" is `stacks.active_version`.
--
-- `stack_files` holds each version's full file set. The runtime still
-- reads from the `ops` table; activation materialises the selected
-- version's files into `ops` inside one BEGIN IMMEDIATE transaction.

CREATE TABLE IF NOT EXISTS stacks (
    stack_id        TEXT PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    name            TEXT NOT NULL,
    active_version  INTEGER,
    created_at      TEXT NOT NULL,
    UNIQUE(tenant_id, name),
    FOREIGN KEY (tenant_id) REFERENCES tenants(tenant_id)
);
CREATE INDEX IF NOT EXISTS stacks_tenant_idx ON stacks(tenant_id);

-- version_id is INTEGER PRIMARY KEY (not AUTOINCREMENT). Plain integer
-- PKs already auto-increment; AUTOINCREMENT only adds the "monotonic
-- across deletes" guarantee, which we don't need (version_number is
-- the user-facing counter and is set explicitly). Skipping AUTOINCREMENT
-- avoids forcing the sqlite_sequence system table into existence — the
-- dbcache mirror's sqlite3dump trips on it when no AUTOINCREMENT row
-- has ever been inserted on a fresh chassis.
CREATE TABLE IF NOT EXISTS stack_versions (
    version_id        INTEGER PRIMARY KEY,
    stack_id          TEXT NOT NULL REFERENCES stacks(stack_id),
    version_number    INTEGER NOT NULL,
    parent_version_id INTEGER REFERENCES stack_versions(version_id),
    status            TEXT NOT NULL DEFAULT 'draft'
                      CHECK (status IN ('draft','superseded','revoked')),
    created_by        TEXT NOT NULL,
    created_at        TEXT NOT NULL,
    activated_at      TEXT,
    manifest_hash     TEXT NOT NULL DEFAULT '',
    UNIQUE(stack_id, version_number)
);
CREATE INDEX IF NOT EXISTS stack_versions_by_stack_idx
    ON stack_versions(stack_id, version_number DESC);

CREATE TABLE IF NOT EXISTS stack_files (
    version_id    INTEGER NOT NULL REFERENCES stack_versions(version_id),
    path          TEXT NOT NULL,
    content       TEXT NOT NULL,
    content_hash  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (version_id, path)
);
CREATE INDEX IF NOT EXISTS stack_files_version_idx ON stack_files(version_id);
