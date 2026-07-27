-- Postgres mirror of sqlite/auth/0004. Force a fresh identity-provider
-- login after an admin-UI sign-out: sign-out stamps this column, the
-- browser-bootstrap endpoint refuses while it's set, and a fresh OIDC
-- enrollment clears it. Without it, sign-out revokes the browser session
-- while the CLI's long-lived ed25519 key mints a new one immediately.
--
-- RFC3339 TEXT to match actors.revoked_at (timestamps are TEXT
-- throughout this schema — see the header of 0001_init.sql). Nullable;
-- existing rows default NULL = "no re-auth required".

ALTER TABLE actors ADD COLUMN IF NOT EXISTS reauth_required_at TEXT;
