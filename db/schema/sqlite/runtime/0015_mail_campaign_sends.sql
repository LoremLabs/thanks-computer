-- mail_campaign_sends backs the txco://sendmail "campaign" guard: a given
-- campaign is delivered to a recipient AT MOST ONCE per tenant. The op
-- claims a row before sending and only proceeds when the INSERT actually
-- created it (claim-first / release-on-failure):
--
--   INSERT ... ON CONFLICT DO NOTHING  -- 0 rows => already sent => skip
--   <send>                             -- success => UPDATE status='sent'
--                                      -- failure => DELETE the claim (retryable)
--
-- Tenant scoping is in the primary key (tenant_id first), so campaign
-- "welcome" for tenant A is independent of tenant B. recipient is stored
-- normalized (lowercased, trimmed). Unlike tenant_runtime_state this is a
-- writable, op-owned table (written on the send path via the real runtime
-- *sql.DB, not the dbcache snapshot) — not read on the request hot path.
--
-- Semantics are at-most-once, biased to NOT-send: a crash between the claim
-- and the send (or its compensating delete) leaves a stale status='claimed'
-- row, and a retry skips that recipient. For a campaign guard that is the
-- correct bias. A stale-claim reaper (TTL on 'claimed') is a later add.

CREATE TABLE IF NOT EXISTS mail_campaign_sends (
    tenant_id   TEXT NOT NULL,
    campaign    TEXT NOT NULL,
    recipient   TEXT NOT NULL,                       -- normalized: lowercased, trimmed
    status      TEXT NOT NULL DEFAULT 'claimed',     -- claimed | sent
    message_id  TEXT NOT NULL DEFAULT '',
    sent_at     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, campaign, recipient)
);
