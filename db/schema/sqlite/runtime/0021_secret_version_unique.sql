-- Backstop for concurrent secret rotation (C-2b): exactly one version_no per
-- secret. On SQLite this was already guaranteed implicitly by the runtime DB's
-- _txlock=immediate single-writer serialization — no two rotations ever
-- interleaved — so no existing row can violate it; this index just makes the
-- invariant explicit and portable to shared Postgres (where nothing serializes
-- rotations until this index + FOR UPDATE lock do). It supersedes the plain
-- tenant_secret_versions_by_secret_idx (0008) as a lookup index too.
--
-- Do NOT attempt to dedup existing rows — tenant_secret_versions are immutable
-- audit history; a violation must fail loudly at migrate (fail-closed), not be
-- silently repaired.
CREATE UNIQUE INDEX IF NOT EXISTS tenant_secret_versions_secret_ver_uidx
    ON tenant_secret_versions(secret_id, version_no);
