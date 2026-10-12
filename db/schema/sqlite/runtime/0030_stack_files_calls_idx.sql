-- Schema parity for the Postgres pgmirror stack_files index (see service
-- overlay pgruntime/schema/0033), rebuilt under a new name with CALLS/.
--
-- The pgmirror reload query copies the active versions' rows under six
-- path prefixes now: FILES/ (static assets), DATASETS/, OUTLETS/,
-- SANDBOXES/, CAPS/ and CALLS/ (the declaration sources; CALLS/ is what
-- `WITH entry` on a call:// resolves through, chassis/server/call_decls.go).
-- A partial index is usable only when its predicate covers the query's, so
-- 0029's five-prefix index stopped applying once the query gained CALLS/.
-- This one carries the same six prefixes as the query and replaces it.
--
-- It does no work on the open-core SQLite runtime; it exists to keep the two
-- schema trees in lockstep. SQLite has no INCLUDE, so the covered columns
-- (path, content_hash) fold into the key.
CREATE INDEX IF NOT EXISTS stack_files_decls6_idx
    ON stack_files (version_id, path, content_hash)
    WHERE path LIKE 'FILES/%' OR path LIKE 'DATASETS/%' OR path LIKE 'OUTLETS/%' OR path LIKE 'SANDBOXES/%' OR path LIKE 'CAPS/%' OR path LIKE 'CALLS/%';

DROP INDEX IF EXISTS stack_files_decls_idx;
