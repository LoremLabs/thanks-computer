-- Authoritative DNS — Phase 2.1: chassis-global synthesis settings.
--
-- The synthesized pattern (chassis/server/personality/dns) needs the
-- deployment's infrastructure facts: our nameserver hostnames (what
-- customers delegate to), our edge IP(s) (the A/AAAA target), and our
-- mail exchanger. These were boot-only `--dns-*` flags; this table makes
-- them operator-settable at runtime via `txco dns config set` and
-- hot-reloaded through dbcache — no restart, and visible via
-- `txco dns config show`.
--
-- Singleton: a chassis has one synthesis-infra config. The boot flags
-- remain the fallback/seed when no row exists (first run). A future
-- per-zone override layer (columns on dns_zones) can shadow these for a
-- zone that needs a different edge/region — see
-- internal docs/todo-dns-authority.md §9.
--
-- List fields (nameservers, edge_ips) are comma-separated, matching the
-- `--dns-*` flag convention.

CREATE TABLE IF NOT EXISTS dns_settings (
    -- Enforce a single row: every write targets singleton = 1.
    singleton   INTEGER PRIMARY KEY CHECK (singleton = 1),
    nameservers TEXT NOT NULL DEFAULT '',
    edge_ips    TEXT NOT NULL DEFAULT '',
    mx_host     TEXT NOT NULL DEFAULT '',
    mx_priority INTEGER NOT NULL DEFAULT 10,
    synth_ttl   INTEGER NOT NULL DEFAULT 300,
    updated_at  TEXT NOT NULL,
    -- actor_id of the operator who last set it. Cross-DB reference to
    -- auth.db.actors; un-enforced by SQLite. See feedback_audit_actor_id.md.
    updated_by  TEXT
);
