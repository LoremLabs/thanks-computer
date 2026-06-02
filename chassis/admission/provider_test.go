package admission

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
)

// ddl mirrors the 0014 migration (tenants + tenant_runtime_state) closely
// enough for the provider's JOIN.
const ddl = `
CREATE TABLE tenants (
    tenant_id  TEXT PRIMARY KEY,
    slug       TEXT NOT NULL UNIQUE,
    name       TEXT,
    created_at TEXT NOT NULL,
    revoked_at TEXT
);
CREATE TABLE tenant_runtime_state (
    tenant_id   TEXT PRIMARY KEY,
    enabled     INTEGER NOT NULL DEFAULT 1,
    suspended   INTEGER NOT NULL DEFAULT 0,
    deny_status INTEGER NOT NULL DEFAULT 403,
    deny_reason TEXT    NOT NULL DEFAULT '',
    rate_limit_rps    INTEGER NOT NULL DEFAULT 0,
    rate_burst        INTEGER NOT NULL DEFAULT 0,
    concurrency_limit INTEGER NOT NULL DEFAULT 0,
    updated_at  TEXT    NOT NULL DEFAULT ''
);`

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedTenant(t *testing.T, db *sql.DB, id, slug string, revoked bool) {
	t.Helper()
	var revokedAt any
	if revoked {
		revokedAt = "2026-01-01T00:00:00Z"
	}
	if _, err := db.Exec(
		`INSERT INTO tenants (tenant_id, slug, name, created_at, revoked_at) VALUES (?,?,?,?,?)`,
		id, slug, slug, "2026-01-01T00:00:00Z", revokedAt); err != nil {
		t.Fatal(err)
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

func TestProviderDecide(t *testing.T) {
	db := newDB(t)
	seedTenant(t, db, "tnt_default", "default", false)
	seedTenant(t, db, "tnt_acme", "acme", false)  // suspended -> 402
	seedTenant(t, db, "tnt_off", "off", false)    // disabled  -> default 403
	seedTenant(t, db, "tnt_rev", "revoked", true) // revoked + row -> JOIN excludes -> admit

	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, suspended, deny_status, deny_reason)
	                 VALUES ('tnt_acme', 1, 402, 'payment_required')`)
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, enabled) VALUES ('tnt_off', 0)`)
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, suspended) VALUES ('tnt_rev', 1)`)

	p := NewSQLiteProvider(zap.NewNop())
	if err := p.Rebuild(db); err != nil {
		t.Fatalf("rebuild: %v", err)
	}

	cases := []struct {
		tenant string
		admit  bool
		status int
		reason string
	}{
		{"unknown", true, 0, ""},
		{"_sys", true, 0, ""},
		{"default", true, 0, ""}, // no row -> admit (default-tenant safety)
		{"acme", false, 402, "payment_required"},
		{"off", false, 403, ""},
		{"revoked", true, 0, ""}, // revoked tenant excluded by JOIN -> admit
	}
	for _, c := range cases {
		d := p.Decide(c.tenant)
		if d.Admit != c.admit {
			t.Errorf("%s: admit=%v want %v", c.tenant, d.Admit, c.admit)
			continue
		}
		if !c.admit {
			if d.Status != c.status {
				t.Errorf("%s: status=%d want %d", c.tenant, d.Status, c.status)
			}
			if d.Reason != c.reason {
				t.Errorf("%s: reason=%q want %q", c.tenant, d.Reason, c.reason)
			}
		}
	}
}

func TestProviderRebuildKeepsPreviousOnError(t *testing.T) {
	db := newDB(t)
	seedTenant(t, db, "tnt_acme", "acme", false)
	mustExec(t, db, `INSERT INTO tenant_runtime_state (tenant_id, suspended) VALUES ('tnt_acme', 1)`)

	p := NewSQLiteProvider(zap.NewNop())
	if err := p.Rebuild(db); err != nil {
		t.Fatal(err)
	}
	if p.Decide("acme").Admit {
		t.Fatal("acme should be denied after first rebuild")
	}
	// Drop the table -> Rebuild errors -> previous snapshot retained.
	mustExec(t, db, `DROP TABLE tenant_runtime_state`)
	if err := p.Rebuild(db); err == nil {
		t.Fatal("expected rebuild error on missing table")
	}
	if p.Decide("acme").Admit {
		t.Error("previous snapshot must be retained on rebuild error (acme still denied)")
	}
}

func TestProviderEmptyAdmitsAll(t *testing.T) {
	p := NewSQLiteProvider(zap.NewNop())
	if !p.Decide("anyone").Admit {
		t.Error("a provider with no snapshot must admit all")
	}
}

func TestNopProviderAdmits(t *testing.T) {
	if !(NopProvider{}).Decide("anyone").Admit {
		t.Error("NopProvider must admit all")
	}
}
