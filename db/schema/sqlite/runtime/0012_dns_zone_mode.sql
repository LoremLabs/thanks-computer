-- Authoritative DNS — Phase 2: zone mode (synthesized pattern vs manual).
--
-- Phase 1 zones were materialized-only: every record lived as a
-- dns_records row. Phase 2 makes the common case a *synthesized pattern*
-- — for a tenant that delegates a subdomain to us (NS -> our chassis),
-- the chassis returns the same computed shape of records (apex SOA/NS/A,
-- optional MX, and per active stack `stack-name.<origin>` A+MX),
-- substituting the origin + the tenant's active stacks. See
-- internal docs/todo-dns-authority.md §6.3-§6.4.
--
--   mode = 'pattern' (default) : synthesize the pattern, then let any
--          dns_records rows OVERRIDE/augment it (same owner+type wins).
--          The common case; a zone needs no dns_records rows at all.
--   mode = 'manual'            : materialized-only — serve exactly the
--          dns_records rows, no synthesis. The Phase-1 behavior, kept as
--          the escape hatch for a hand-managed zone file.
--
-- dns_records remains the override/extra layer for BOTH modes.

ALTER TABLE dns_zones ADD COLUMN mode TEXT NOT NULL DEFAULT 'pattern'
    CHECK (mode IN ('pattern','manual'));
