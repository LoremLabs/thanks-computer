-- Carry the actor's super-admin status onto browser bootstraps +
-- sessions. Without it, a cookie-authed request is indistinguishable
-- from a real operator at RequireSuperAdmin (which treated any non-signed
-- source as super-admin), so any tenant member who minted a browser
-- session passed operator-only gates (tenant create, dns config, ...).
--
-- verifyCookie now sets auth.Context.SuperAdmin from this column, and
-- RequireSuperAdmin gates on the real flag. INTEGER 0/1 mirrors
-- actors.super_admin. Existing rows default 0 (not super-admin) — the
-- safe direction: they simply stop passing operator gates.

ALTER TABLE browser_bootstrap ADD COLUMN super_admin INTEGER NOT NULL DEFAULT 0;
ALTER TABLE browser_sessions ADD COLUMN super_admin INTEGER NOT NULL DEFAULT 0;
