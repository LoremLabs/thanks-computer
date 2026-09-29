-- Standing grants: what a principal may ever ask the chassis for. One row
-- says one thing — this principal may use these verbs on this resource —
-- with no conditions, roles, inheritance or time limit. See chassis/authn
-- (grants.go).
--
-- This is the stack plane (0005_identity.sql): the rows are written by a
-- tenant's stacks through txco://grant/*, and nothing here references the
-- account plane.
--
-- Kinds built: capability (verb invoke) and secret (verb release). The
-- resource is named, never referenced: a grant may name a secret that is
-- stored later, and outlives one that is deleted and stored again.
--
-- created_by is the base name of the stack that wrote the row, which must be
-- the stack that manages the principal (authn/owner.go).
--
-- A row never changes except to be revoked. Changing a grant's verbs revokes
-- the row and writes another, so the table is its own history.
CREATE TABLE IF NOT EXISTS resource_grants (
    id             TEXT PRIMARY KEY,           -- grt_<hxid>
    tenant_id      TEXT NOT NULL,
    principal_id   TEXT NOT NULL,
    resource_kind  TEXT NOT NULL,              -- capability | secret
    resource_id    TEXT NOT NULL,              -- crm.lookup | CRM_KEY
    verbs          TEXT NOT NULL,              -- JSON array: ["invoke"] | ["release"]
    created_by     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    revoked_at     TEXT
);
-- One live grant per principal and resource. Partial, so a revoked grant can
-- be given again.
CREATE UNIQUE INDEX IF NOT EXISTS resource_grants_live_idx
    ON resource_grants(tenant_id, principal_id, resource_kind, resource_id)
    WHERE revoked_at IS NULL;
-- "Who can reach this?" reads by resource.
CREATE INDEX IF NOT EXISTS resource_grants_resource_idx
    ON resource_grants(tenant_id, resource_kind, resource_id);
