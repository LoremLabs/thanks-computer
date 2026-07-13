package admin

// Inline schema fragments used by every Controller test scaffold. Kept
// in one place so adding a table to the production schema requires one
// edit here instead of N copies of the same DDL.
//
// In production these tables span two files (auth.db vs runtime.db); in
// tests we apply both fragments to a single in-memory DB so the
// Controller's `pu.RuntimeDB == pu.AuthDB` path holds.

const runtimeSchemaSQL = `
CREATE TABLE ops (
	stack TEXT, scope INTEGER, name TEXT NOT NULL DEFAULT '',
	txcl TEXT, mock_req TEXT, mock_res TEXT,
	tenant_id TEXT,
	UNIQUE(stack, scope, txcl)
);
CREATE UNIQUE INDEX ops_stack_scope_name_idx
	ON ops (stack, scope, name) WHERE name != '';
CREATE TABLE tenants (
	tenant_id  TEXT PRIMARY KEY,
	slug       TEXT NOT NULL UNIQUE,
	name       TEXT,
	created_at TEXT NOT NULL,
	revoked_at TEXT
);
INSERT INTO tenants (tenant_id, slug, created_at)
	VALUES ('tnt_default', 'default', '2026-01-01T00:00:00Z');
CREATE TABLE stacks (
	stack_id        TEXT PRIMARY KEY,
	tenant_id       TEXT NOT NULL,
	name            TEXT NOT NULL,
	active_version  INTEGER,
	created_at      TEXT NOT NULL,
	mint_hostname   INTEGER NOT NULL DEFAULT 1,
	UNIQUE(tenant_id, name)
);
CREATE TABLE stack_versions (
	version_id        INTEGER PRIMARY KEY,
	stack_id          TEXT NOT NULL REFERENCES stacks(stack_id),
	version_number    INTEGER NOT NULL,
	parent_version_id INTEGER REFERENCES stack_versions(version_id),
	status            TEXT NOT NULL DEFAULT 'draft'
	                  CHECK (status IN ('draft','superseded','revoked')),
	created_by        TEXT NOT NULL,
	created_at        TEXT NOT NULL,
	activated_at      TEXT,
	manifest_hash     TEXT NOT NULL DEFAULT '',
	UNIQUE(stack_id, version_number)
);
CREATE TABLE stack_files (
	version_id    INTEGER NOT NULL REFERENCES stack_versions(version_id),
	path          TEXT NOT NULL,
	content       TEXT NOT NULL,
	content_hash  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (version_id, path)
);
CREATE TABLE tenant_hostnames (
	id          TEXT PRIMARY KEY,
	hostname    TEXT NOT NULL,
	tenant_id   TEXT NOT NULL REFERENCES tenants(tenant_id),
	stack       TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	created_by  TEXT,
	revoked_at  TEXT,
	verified_at TEXT,
	dkim_selector    TEXT NOT NULL DEFAULT '',
	dkim_private_pem TEXT NOT NULL DEFAULT '',
	dkim_public_b64  TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX tenant_hostnames_active_hostname_idx
    ON tenant_hostnames(hostname)
    WHERE revoked_at IS NULL;
CREATE INDEX tenant_hostnames_tenant_idx
    ON tenant_hostnames(tenant_id);
CREATE TABLE tenant_hostname_challenges (
	id            TEXT PRIMARY KEY,
	hostname_id   TEXT NOT NULL REFERENCES tenant_hostnames(id),
	method        TEXT NOT NULL CHECK (method IN ('dns-txt','http-01')),
	token         TEXT NOT NULL UNIQUE,
	created_at    TEXT NOT NULL,
	created_by    TEXT,
	expires_at    TEXT NOT NULL,
	attempted_at  TEXT,
	last_error    TEXT,
	verified_at   TEXT,
	revoked_at    TEXT
);
CREATE UNIQUE INDEX tenant_hostname_challenges_active_idx
    ON tenant_hostname_challenges(hostname_id, method)
    WHERE verified_at IS NULL AND revoked_at IS NULL;

CREATE TABLE tenant_secrets (
    secret_id        TEXT PRIMARY KEY,
    tenant_id        TEXT NOT NULL,
    stack            TEXT,
    name             TEXT NOT NULL,
    description      TEXT,
    created_at       TEXT NOT NULL,
    created_by       TEXT,
    revoked_at       TEXT,
    last_rotated_at  TEXT,
    key_version      INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE tenant_secret_versions (
    version_id   TEXT PRIMARY KEY,
    secret_id    TEXT NOT NULL,
    version_no   INTEGER NOT NULL,
    nonce        BLOB NOT NULL,
    ciphertext   BLOB NOT NULL,
    wrapped_dek  BLOB NOT NULL,
    dek_nonce    BLOB NOT NULL,
    created_at   TEXT NOT NULL,
    revoked_at   TEXT
);
CREATE UNIQUE INDEX tenant_secrets_active_name_idx
    ON tenant_secrets (tenant_id, COALESCE(stack, ''), name)
    WHERE revoked_at IS NULL;
CREATE TABLE control_events_outbox (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id                  TEXT NOT NULL UNIQUE,
    event_type                TEXT NOT NULL,
    tenant_id                 TEXT,
    stack_id                  TEXT,
    version                   INTEGER,
    base_version              INTEGER,
    artifact_ref              TEXT,
    checksum                  TEXT,
    payload_json              BLOB NOT NULL,
    created_at                TEXT NOT NULL,
    attempt_count             INTEGER NOT NULL DEFAULT 0,
    last_error                TEXT,
    last_attempt_at           TEXT,
    published_control_version INTEGER,
    published_at              TEXT
);
`

const authSchemaSQL = `
CREATE TABLE actors (
	actor_id    TEXT PRIMARY KEY, label TEXT, kind TEXT,
	subject     TEXT, tenant TEXT, stack TEXT,
	super_admin INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL, revoked_at TEXT, meta TEXT
);
CREATE TABLE actor_keys (
	key_id     TEXT PRIMARY KEY, actor_id TEXT NOT NULL,
	public_key BLOB NOT NULL, algorithm TEXT NOT NULL,
	created_at TEXT NOT NULL, revoked_at TEXT, meta TEXT
);
CREATE TABLE actor_invitations (
	invitation_id TEXT PRIMARY KEY,
	token_hash    TEXT NOT NULL UNIQUE,
	label         TEXT, kind TEXT,
	capabilities  TEXT NOT NULL,
	created_by    TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	expires_at    TEXT NOT NULL,
	consumed_at   TEXT, consumed_by TEXT, revoked_at TEXT,
	tenant_id     TEXT
);
CREATE TABLE actor_memberships (
	actor_id          TEXT NOT NULL,
	tenant_id         TEXT NOT NULL,
	capabilities_json TEXT NOT NULL,
	created_at        TEXT NOT NULL,
	revoked_at        TEXT,
	PRIMARY KEY (actor_id, tenant_id)
);
CREATE TABLE browser_bootstrap (
	token_hash         TEXT PRIMARY KEY,
	actor_id           TEXT NOT NULL,
	tenant_id          TEXT NOT NULL,
	capabilities_json  TEXT NOT NULL,
	super_admin        INTEGER NOT NULL DEFAULT 0,
	label              TEXT,
	created_at         TEXT NOT NULL,
	expires_at         TEXT NOT NULL,
	consumed_at        TEXT,
	consumed_ip        TEXT
);
CREATE TABLE browser_sessions (
	session_id         TEXT PRIMARY KEY,
	actor_id           TEXT NOT NULL,
	tenant_id          TEXT NOT NULL,
	capabilities_json  TEXT NOT NULL,
	super_admin        INTEGER NOT NULL DEFAULT 0,
	ua                 TEXT,
	ip                 TEXT,
	created_at         TEXT NOT NULL,
	expires_at         TEXT NOT NULL,
	revoked_at         TEXT,
	revoked_by         TEXT,
	last_seen_at       TEXT NOT NULL
);
CREATE TABLE dns_zones (
	id          TEXT PRIMARY KEY,
	tenant_id   TEXT NOT NULL,
	origin      TEXT NOT NULL,
	mname       TEXT NOT NULL,
	rname       TEXT NOT NULL,
	refresh     INTEGER NOT NULL DEFAULT 7200,
	retry       INTEGER NOT NULL DEFAULT 3600,
	expire      INTEGER NOT NULL DEFAULT 1209600,
	minimum     INTEGER NOT NULL DEFAULT 300,
	default_ttl INTEGER NOT NULL DEFAULT 300,
	mode        TEXT NOT NULL DEFAULT 'pattern',
	created_at  TEXT NOT NULL,
	created_by  TEXT,
	updated_at  TEXT NOT NULL,
	revoked_at  TEXT,
	verified_at TEXT,
	dkim_selector    TEXT NOT NULL DEFAULT '',
	dkim_private_pem TEXT NOT NULL DEFAULT '',
	dkim_public_b64  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE dns_records (
	id          TEXT PRIMARY KEY,
	zone_id     TEXT NOT NULL REFERENCES dns_zones(id),
	name        TEXT NOT NULL,
	type        TEXT NOT NULL
	            CHECK (type IN ('NS','A','AAAA','MX','TXT')),
	ttl         INTEGER,
	rdata       TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	created_by  TEXT,
	updated_at  TEXT NOT NULL,
	revoked_at  TEXT
);
CREATE TABLE cron_settings (
	tenant_id  TEXT PRIMARY KEY,
	timezone   TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	updated_by TEXT
);
CREATE TABLE oidc_subjects (
	issuer     TEXT NOT NULL,
	subject    TEXT NOT NULL,
	tenant_id  TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (issuer, subject)
);
`
