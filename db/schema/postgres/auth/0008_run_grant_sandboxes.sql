-- Sandboxes on a run grant: what the work may OPEN, by name, and what each
-- sandbox hands the program (chassis/sandbox). A JSON object of
--   {"github": {"GH_TOKEN": "secret:GITHUB_PAT"}, ...}
-- snapshotted at mint from the minting stack's SANDBOXES/<name>.yaml, so a
-- later edit to the file changes new mints, not live grants. The allowlist
-- column is derived from it (every secret the sandboxes name) and stays the
-- authority for what a request may be handed; this column says under which
-- variable names.
ALTER TABLE run_grants ADD COLUMN IF NOT EXISTS sandboxes TEXT NOT NULL DEFAULT '{}';
