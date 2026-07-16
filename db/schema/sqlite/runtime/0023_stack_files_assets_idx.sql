-- Schema parity for the Postgres pgmirror stack_files index (see service
-- overlay pgruntime/schema/0026).
--
-- The pgmirror reload query — JOIN stacks.active_version + WHERE path LIKE
-- 'FILES/%' OR path LIKE 'DATASETS/%' — runs only against a Postgres runtime
-- store, so this index does no work on the open-core SQLite runtime; it exists
-- to keep the two schema trees in lockstep (as 0022's content-bytea change is
-- Postgres-only in the other direction).
--
-- SQLite has no INCLUDE, so the covered columns (path, content_hash) fold into
-- the key. Partial index matches the established dns_records_active_zone_idx
-- precedent (0022).
CREATE INDEX IF NOT EXISTS stack_files_assets_idx
    ON stack_files (version_id, path, content_hash)
    WHERE path LIKE 'FILES/%' OR path LIKE 'DATASETS/%';
