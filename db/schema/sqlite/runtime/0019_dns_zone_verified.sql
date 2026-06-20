-- DNS zone NS-verification (multi-tenant anti-squat).
--
-- Creating a dns_zones row otherwise confers authority for the origin
-- IMMEDIATELY — DKIM signing, verified-sender status (DomainCoveredByZone),
-- inbound mail routing (TenantForMailZone), and authoritative DNS serving —
-- WITHOUT proving the tenant controls the domain. That's fine for dev /
-- single-operator chassis (you trust the operator), but a squatting hole in
-- multi-tenant self-service (a tenant could `zone create stripe.com` they don't
-- own and send DKIM-signed mail as it).
--
-- The opt-in --dns-require-zone-verification flag gates authority on this
-- column: when set, a zone stays PENDING (verified_at NULL) until its NS
-- actually resolves to our --dns-nameservers (`txco dns zone verify`). When the
-- flag is off (default), CreateZoneTx stamps verified_at at creation so behavior
-- is unchanged. Authority reads add `AND verified_at IS NOT NULL`.
--
-- Mirrors tenant_hostnames.verified_at (0005). NULL = unverified/pending.
ALTER TABLE dns_zones ADD COLUMN verified_at TEXT;

-- Grandfather existing zones: they were created super-admin-only under the old
-- gate, so they stay active if an operator later flips the flag on. (New zones
-- created with the flag on start NULL/pending.)
UPDATE dns_zones SET verified_at = created_at WHERE verified_at IS NULL;
