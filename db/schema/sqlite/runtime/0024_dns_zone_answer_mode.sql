-- Authoritative DNS — stack-answered zones (the `_dns` inlet, lane 2).
--
-- `mode` (0012) says how a zone's records are COMPOSED: 'pattern'
-- synthesis vs 'manual' rows. `answer_mode` says WHO answers a query:
--
--   answer_mode = 'snapshot' (default) : the prebuilt zone snapshot, as
--                                        before — no change in behavior.
--   answer_mode = 'stack'              : the tenant's `_dns` stack. On an
--                                        answer-cache miss the head dispatches
--                                        the query with the snapshot's answer
--                                        pre-stamped as `@dns.proposed`; the
--                                        stack's `@dns.res` goes on the wire
--                                        and is cached by its TTL.
--
-- Orthogonal to `mode`: the proposal is whatever `mode` composes, so a
-- pass-through stack (`EMIT @dns.res = @dns.proposed`) answers a stack zone
-- byte-identically to its snapshot mode.
--
-- `stack_fallback` is what a stack zone answers when the stack does NOT —
-- deadline, error, over the dispatch limit, invalid or absent `@dns.res`:
--
--   stack_fallback = 'proposal' (default) : the snapshot answer, so a broken
--                                           or slow stack degrades to today's
--                                           behavior rather than to darkness.
--   stack_fallback = 'servfail'           : SERVFAIL (resolvers retry, never
--                                           negative-cache), for zones whose
--                                           truth lives only in the stack.
--
-- Both are plain ADD COLUMN with a CHECK, the 0012 precedent. Data-plane
-- nodes read them from the dbcache mirror (BuildSnapshot); the fleet row
-- upsert carries them (admin dns_fleet.go zoneToRow).

ALTER TABLE dns_zones ADD COLUMN answer_mode TEXT NOT NULL DEFAULT 'snapshot'
    CHECK (answer_mode IN ('snapshot','stack'));

ALTER TABLE dns_zones ADD COLUMN stack_fallback TEXT NOT NULL DEFAULT 'proposal'
    CHECK (stack_fallback IN ('proposal','servfail'));
