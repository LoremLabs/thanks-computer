-- Authoritative DNS — Phase 1: materialized zones + records.
--
-- The `dns` personality (chassis/server/personality/dns) answers
-- authoritatively for zones explicitly delegated to this chassis. Phase
-- 1 serves *materialized* records straight from these tables (no
-- synthesis, no DNS-01, no DNSSEC — those are later phases). A request
-- whose name falls under no active zone here is REFUSED; the server is
-- never a recursive/open resolver.
--
-- Routing/authority gate (Phase 1): the chassis answers for any active
-- dns_zones row — operator-trusted, the same posture as a YAML ingress
-- route. The verified-claim delegation gate (proving control of the
-- parent before we answer) is a Phase 2 concern; see
-- internal docs/todo-dns-authority.md §6.5.
--
-- Same audit-history shape as tenant_hostnames (0004/0005): surrogate
-- TEXT PK + created_at/created_by/updated_at/revoked_at, soft-revoke via
-- revoked_at, and a partial unique index keyed on the active subset.

-- One delegated zone. The SOA is *synthesized* from these columns at
-- serve time (it is never stored as a dns_records row); `serial` is
-- computed per-zone as the uint32 epoch-seconds of MAX(updated_at) over
-- the zone row + its active records (see the dns package), so a serial
-- advances only when zone content actually changes.
CREATE TABLE IF NOT EXISTS dns_zones (
    id          TEXT PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(tenant_id),
    -- Canonical apex FQDN, lowercased + trailing-dot-stripped (the
    -- CanonicalizeHost form), e.g. "ops.example.com". The server appends
    -- the trailing dot when matching wire queries.
    origin      TEXT NOT NULL,
    -- SOA fields. mname = primary nameserver; rname = responsible party
    -- in DNS email form (dots for '@'), e.g. "hostmaster.txco.io".
    mname       TEXT NOT NULL,
    rname       TEXT NOT NULL,
    refresh     INTEGER NOT NULL DEFAULT 7200,
    retry       INTEGER NOT NULL DEFAULT 3600,
    expire      INTEGER NOT NULL DEFAULT 1209600,
    -- SOA minimum doubles as the negative-cache (NXDOMAIN/NODATA) TTL.
    minimum     INTEGER NOT NULL DEFAULT 90,
    -- TTL applied to records whose own ttl is NULL.
    default_ttl INTEGER NOT NULL DEFAULT 60,
    created_at  TEXT NOT NULL,
    -- actor_id of whoever created the zone. Cross-DB reference to
    -- auth.db.actors; un-enforced by SQLite. See feedback_audit_actor_id.md.
    created_by  TEXT,
    -- Load-bearing for the serial: bumped on every mutation, left
    -- untouched on a no-op write so a no-op reload never churns the
    -- serial (which would trigger needless zone transfers).
    updated_at  TEXT NOT NULL,
    -- Soft-revoke; NULL = active. Revoked zones stay for audit.
    revoked_at  TEXT
);

-- "One active zone per origin." Active = not revoked. A second tenant
-- (or a re-add) cannot claim an origin already actively served.
CREATE UNIQUE INDEX IF NOT EXISTS dns_zones_active_origin_idx
    ON dns_zones(origin)
    WHERE revoked_at IS NULL;

-- Resource records within a zone. `rdata` is stored in DNS
-- presentation format (what `dig` prints), so the server can parse it
-- with dns.NewRR and the render path can emit it verbatim:
--   A     "192.0.2.10"
--   AAAA  "2001:db8::1"
--   MX    "10 mail.example.com."
--   TXT   "v=spf1 include:_spf.txco.io -all"
--   NS    "ns1.txco.io."
-- Phase 1 record types only; SOA is synthesized (never a row here).
CREATE TABLE IF NOT EXISTS dns_records (
    id          TEXT PRIMARY KEY,
    zone_id     TEXT NOT NULL REFERENCES dns_zones(id),
    -- Relative label within the zone, or '@' for the apex. e.g.
    -- '@', 'www', 'mail', '_dmarc'.
    name        TEXT NOT NULL,
    type        TEXT NOT NULL
                CHECK (type IN ('NS','A','AAAA','MX','TXT')),
    -- NULL inherits the zone's default_ttl.
    ttl         INTEGER,
    rdata       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    created_by  TEXT,
    -- See dns_zones.updated_at — same serial-input role.
    updated_at  TEXT NOT NULL,
    revoked_at  TEXT
);

-- The serve-path read is "all active records for a zone"; this covers
-- it without scanning revoked history.
CREATE INDEX IF NOT EXISTS dns_records_active_zone_idx
    ON dns_records(zone_id)
    WHERE revoked_at IS NULL;
