-- Maps an OIDC (issuer, subject) to the tenant created on that identity's
-- first cloud enrollment (POST /auth/oauth/enroll). Lives in auth.db beside
-- actors / actor_keys / actor_memberships. tenant_id references the runtime.db
-- tenants(tenant_id) row cross-DB; the FK is un-enforced (different database).
--
-- The (issuer, subject) primary key makes re-enrollment idempotent: the same
-- identity always resolves to the same tenant, regardless of which (possibly
-- new) key it presents.

CREATE TABLE IF NOT EXISTS oidc_subjects (
    issuer     TEXT NOT NULL,
    subject    TEXT NOT NULL,
    tenant_id  TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (issuer, subject)
);
CREATE INDEX IF NOT EXISTS oidc_subjects_tenant_idx ON oidc_subjects(tenant_id);
