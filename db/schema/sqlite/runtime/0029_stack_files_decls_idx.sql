-- Schema parity for the Postgres pgmirror stack_files index (see service
-- overlay pgruntime/schema/0032), rebuilt under a new name.
--
-- The pgmirror reload query copies the active versions' rows under five
-- path prefixes: FILES/ (static assets), DATASETS/, OUTLETS/, SANDBOXES/
-- and CAPS/ (the declaration sources). 0023's stack_files_assets_idx was
-- written when the query named two of them; a partial index is usable only
-- when its predicate covers the query's, so it stopped applying once the
-- query grew. This one carries the same five prefixes as the query and
-- replaces it.
--
-- It does no work on the open-core SQLite runtime; it exists to keep the two
-- schema trees in lockstep. SQLite has no INCLUDE, so the covered columns
-- (path, content_hash) fold into the key.
CREATE INDEX IF NOT EXISTS stack_files_decls_idx
    ON stack_files (version_id, path, content_hash)
    WHERE path LIKE 'FILES/%' OR path LIKE 'DATASETS/%' OR path LIKE 'OUTLETS/%' OR path LIKE 'SANDBOXES/%' OR path LIKE 'CAPS/%';

DROP INDEX IF EXISTS stack_files_assets_idx;
