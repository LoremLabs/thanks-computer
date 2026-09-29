-- The pull policy of a secret: whether work that was dispatched somewhere
-- else (a workspace) may ASK for the secret itself and be handed it.
--
--   pull = 'none' (default) : no. The secret is used where it is stored — a
--                             rule names it and the chassis materializes it
--                             into that one op — which is every secret's
--                             behavior before this column existed.
--   pull = 'reviewed'       : work on a reviewed node may be handed it.
--   pull = 'any'            : work on any node may be handed it, including
--                             one that runs code nobody has read.
--
-- The policy belongs to the secret and is set by whoever stores it. It is
-- one input to the decision, not the decision: a request also needs a run
-- grant that names the secret and a principal that holds a standing grant
-- to release it (db/schema/*/auth/0006, 0007), and the tenant's `_grant`
-- stack may change the answer either way.
--
-- This migration ships one release BEFORE any code reads or writes the
-- column. Secret rows reach a fleet's nodes as row maps applied to each
-- node's mirror, and a map naming a column the mirror does not have fails
-- to apply — so every mirror must have the column before any node sends it.
--
-- Plain ADD COLUMN with a CHECK, the 0012 and 0024 precedent.

ALTER TABLE tenant_secrets ADD COLUMN pull TEXT NOT NULL DEFAULT 'none'
    CHECK (pull IN ('none','reviewed','any'));
