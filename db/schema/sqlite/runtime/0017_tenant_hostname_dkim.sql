-- Per-host DKIM keys for chassis-minted structured hosts
-- (`<stack>-<rand>.<structured-suffix>`). Each host signs outbound mail with
-- `d=<host>, s=<selector>` using its OWN key, and the DNS head publishes the
-- matching `txco._domainkey.<host>` (+ a per-host `_dmarc.<host>`). Per-host
-- keys isolate sending reputation — one bad stack can't poison the shared
-- suffix — which is a core reason the platform self-hosts its nameserver.
--
-- Generated once at mint (tenants.EnsureSystemHostnameTx) and fleet-synced on
-- the tenant_hostnames row, so every node signs + publishes identically.
-- NOT NULL DEFAULT '' so the fleet applier's INSERT OR REPLACE and pre-0017
-- rows stay valid; an empty key just means "not signing yet" (a one-time
-- backfill mints keys for older structured hosts). Only system-minted
-- structured hosts carry a key; verified custom domains use their dns_zones
-- key (0016) instead.

ALTER TABLE tenant_hostnames ADD COLUMN dkim_selector    TEXT NOT NULL DEFAULT '';
ALTER TABLE tenant_hostnames ADD COLUMN dkim_private_pem TEXT NOT NULL DEFAULT '';
ALTER TABLE tenant_hostnames ADD COLUMN dkim_public_b64  TEXT NOT NULL DEFAULT '';
