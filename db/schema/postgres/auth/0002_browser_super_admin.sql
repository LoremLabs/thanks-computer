-- Postgres mirror of sqlite/auth/0002. Carry the actor's super-admin
-- status onto browser bootstraps + sessions so RequireSuperAdmin gates
-- cookie-authed requests on the real flag rather than treating every
-- browser session as an operator. INTEGER 0/1 to match actors.super_admin
-- (scanned into an int, not a bool). Existing rows default 0.

ALTER TABLE browser_bootstrap ADD COLUMN IF NOT EXISTS super_admin INTEGER NOT NULL DEFAULT 0;
ALTER TABLE browser_sessions ADD COLUMN IF NOT EXISTS super_admin INTEGER NOT NULL DEFAULT 0;
