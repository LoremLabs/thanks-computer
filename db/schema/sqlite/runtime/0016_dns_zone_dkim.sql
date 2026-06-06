-- Per-domain DKIM keys, stored ON the dns_zones row so they ride the zone's
-- existing fleet-sync (snapshot dump + dns.zone.upserted control event) to
-- every node. The keypair is generated ONCE on the control plane when the
-- zone is created (CreateZoneTx) — never per-node, so the public key the DNS
-- head publishes and the private key any node signs with are identical.
--
--   dkim_private_pem — PKCS#1 PEM; the signer (txco://sendmail) signs with
--                      d=<origin>, s=<selector>. Sensitive, but rides the same
--                      trusted channels as the rest of runtime state.
--   dkim_public_b64  — base64 PKIX DER; the DNS head publishes
--                      <selector>._domainkey.<origin> TXT "v=DKIM1;k=rsa;p=…".
--
-- All NOT NULL DEFAULT '' so the fleet-sync applier's INSERT OR REPLACE (which
-- reconstructs the row from the artifact map) and pre-existing zones both stay
-- valid; an empty key simply means "not signing yet" (backfill generates one).

ALTER TABLE dns_zones ADD COLUMN dkim_selector    TEXT NOT NULL DEFAULT '';
ALTER TABLE dns_zones ADD COLUMN dkim_private_pem TEXT NOT NULL DEFAULT '';
ALTER TABLE dns_zones ADD COLUMN dkim_public_b64  TEXT NOT NULL DEFAULT '';
