-- CNAME support for zone override records.
--
-- 0011 allowlisted Phase-1 record types (NS/A/AAAA/MX/TXT) in a
-- table-level CHECK. A CHECK can't be altered in place in SQLite, so
-- rebuild the table with CNAME added — the 0006 ops rebuild precedent.
-- dns_records is referenced by nothing (it only references dns_zones),
-- so a plain rename/copy/drop is safe. The data plane reads the dbcache
-- mirror, not this table directly, so no online-migration concern.
--
-- CNAME protocol rules (apex forbidden, no coexistence with other data
-- at the same owner, singleton per owner) are enforced at the admin
-- write path, not here — the schema stays a type allowlist.

ALTER TABLE dns_records RENAME TO dns_records_old;

CREATE TABLE dns_records (
    id          TEXT PRIMARY KEY,
    zone_id     TEXT NOT NULL REFERENCES dns_zones(id),
    name        TEXT NOT NULL,
    type        TEXT NOT NULL
                CHECK (type IN ('NS','A','AAAA','MX','TXT','CNAME')),
    ttl         INTEGER,
    rdata       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    created_by  TEXT,
    updated_at  TEXT NOT NULL,
    revoked_at  TEXT
);

INSERT INTO dns_records (id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at, revoked_at)
    SELECT id, zone_id, name, type, ttl, rdata, created_at, created_by, updated_at, revoked_at
      FROM dns_records_old;

DROP TABLE dns_records_old;

-- The partial index followed the old table down; recreate it.
CREATE INDEX IF NOT EXISTS dns_records_active_zone_idx
    ON dns_records(zone_id)
    WHERE revoked_at IS NULL;
