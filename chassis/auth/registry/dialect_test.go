package registry

import (
	"errors"
	"testing"
)

func TestDialectForDSN(t *testing.T) {
	cases := map[string]Dialect{
		"file:./chassis/data/db/auth-prod.db": SQLite,
		"file:$db-root-dir/auth-$env.db":      SQLite,
		"":                                    SQLite,
		"auth-prod.db":                        SQLite,
		"postgres://u:p@host:5432/db?sslmode=disable": Postgres,
		"postgresql://u:p@host/db":                    Postgres,
		"POSTGRES://U:P@HOST/DB":                      Postgres, // scheme is case-insensitive
		"  postgres://u@h/d  ":                        Postgres, // trimmed
	}
	for dsn, want := range cases {
		if got := DialectForDSN(dsn); got != want {
			t.Errorf("DialectForDSN(%q) = %T, want %T", dsn, got, want)
		}
	}
}

func TestSQLiteRebindIsIdentity(t *testing.T) {
	q := `SELECT a FROM t WHERE x = ? AND y = ? -- '?' stays`
	if got := SQLite.Rebind(q); got != q {
		t.Errorf("SQLite.Rebind mutated the query:\n got %q\nwant %q", got, q)
	}
}

func TestPostgresRebind(t *testing.T) {
	cases := []struct{ in, want string }{
		{`SELECT 1`, `SELECT 1`},
		{`WHERE a = ?`, `WHERE a = $1`},
		{`INSERT INTO t (a,b,c) VALUES (?, ?, ?)`, `INSERT INTO t (a,b,c) VALUES ($1, $2, $3)`},
		{
			`UPDATE t SET c = ? WHERE k = ? AND consumed_at IS NULL AND exp > ?`,
			`UPDATE t SET c = $1 WHERE k = $2 AND consumed_at IS NULL AND exp > $3`,
		},
		// A literal ? inside a single-quoted string must NOT be rebound;
		// a real placeholder after it still numbers correctly.
		{`SELECT '? not a param' , x WHERE y = ?`, `SELECT '? not a param' , x WHERE y = $1`},
		// Escaped '' inside a string literal.
		{`SELECT 'it''s ? fine' WHERE z = ?`, `SELECT 'it''s ? fine' WHERE z = $1`},
		// 10+ params get multi-digit indices.
		{
			`VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			`VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		},
		// Runtime-port statement shapes — lock the $N numbering.
		// tenant_runtime_state ON CONFLICT upsert (9 value placeholders; the
		// DO UPDATE SET clause is excluded.*-only, no placeholders).
		{
			`INSERT INTO tenant_runtime_state (tenant_id, enabled, suspended, deny_status, deny_reason, rate_limit_rps, rate_burst, concurrency_limit, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(tenant_id) DO UPDATE SET enabled = excluded.enabled`,
			`INSERT INTO tenant_runtime_state (tenant_id, enabled, suspended, deny_status, deny_reason, rate_limit_rps, rate_burst, concurrency_limit, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) ON CONFLICT(tenant_id) DO UPDATE SET enabled = excluded.enabled`,
		},
		// INSERT … RETURNING (the dialect-conditional draft-version path).
		{
			`INSERT INTO stack_versions (stack_id, version_number, parent_version_id, status, created_by, created_at, manifest_hash) VALUES (?, ?, ?, 'draft', ?, ?, '') RETURNING version_id`,
			`INSERT INTO stack_versions (stack_id, version_number, parent_version_id, status, created_by, created_at, manifest_hash) VALUES ($1, $2, $3, 'draft', $4, $5, '') RETURNING version_id`,
		},
		// cron_settings ON CONFLICT DO UPDATE (excluded.* body).
		{
			`INSERT INTO cron_settings (tenant_id, timezone, updated_at, updated_by) VALUES (?, ?, ?, ?) ON CONFLICT(tenant_id) DO UPDATE SET timezone = excluded.timezone, updated_at = excluded.updated_at`,
			`INSERT INTO cron_settings (tenant_id, timezone, updated_at, updated_by) VALUES ($1, $2, $3, $4) ON CONFLICT(tenant_id) DO UPDATE SET timezone = excluded.timezone, updated_at = excluded.updated_at`,
		},
		// A ? inside a -- line comment is not a placeholder; the real one after is.
		{
			"SELECT a -- keep ? here\nFROM t WHERE id = ?",
			"SELECT a -- keep ? here\nFROM t WHERE id = $1",
		},
		// A ? inside a /* block comment */ is not a placeholder.
		{
			`SELECT a /* ? not a param */ FROM t WHERE id = ?`,
			`SELECT a /* ? not a param */ FROM t WHERE id = $1`,
		},
		// A ? inside a "quoted identifier" is not a placeholder ("" escape too).
		{
			`SELECT "we?rd", "a""b?" FROM t WHERE id = ?`,
			`SELECT "we?rd", "a""b?" FROM t WHERE id = $1`,
		},
		// Postgres JSON existence operators ?| and ?& are left intact.
		{
			`SELECT meta ?| array['a','b'], meta ?& array['c'] FROM t WHERE id = ?`,
			`SELECT meta ?| array['a','b'], meta ?& array['c'] FROM t WHERE id = $1`,
		},
		// A " inside a '…' string literal does not open an identifier.
		{
			`SELECT 'a"b ? c' WHERE id = ?`,
			`SELECT 'a"b ? c' WHERE id = $1`,
		},
	}
	for _, c := range cases {
		if got := Postgres.Rebind(c.in); got != c.want {
			t.Errorf("Postgres.Rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

// TestPostgresRebindBareJSONOperatorLimitation documents (does not endorse) the
// known gap: the bare single-char JSON key-exists operator `?` is textually
// indistinguishable from a placeholder and IS rebound. Runtime queries must use
// jsonb_exists(col, 'key') instead. If this ever needs to change, a real SQL
// scanner is the fix — not a bigger regex.
func TestPostgresRebindBareJSONOperatorLimitation(t *testing.T) {
	got := Postgres.Rebind(`SELECT meta ? 'key' FROM t WHERE id = ?`)
	// The `?` operator is (wrongly, but knowably) treated as a placeholder,
	// so the trailing real placeholder becomes $2. This asserts the CURRENT
	// behavior so a future fix flips this test deliberately.
	want := `SELECT meta $1 'key' FROM t WHERE id = $2`
	if got != want {
		t.Errorf("bare-? limitation drifted:\n got %q\nwant %q", got, want)
	}
}

// fakePGErr implements the duck-typed interface{ SQLState() string } the
// way lib/pq's *pq.Error and pgx's *pgconn.PgError do, so the dialect
// can detect a unique violation without core importing either driver.
type fakePGErr struct{ code string }

func (e *fakePGErr) Error() string    { return "pg error " + e.code }
func (e *fakePGErr) SQLState() string { return e.code }

func TestIsUniqueViolation(t *testing.T) {
	// SQLite: historical, column-specific error-string match.
	sqliteUnique := errors.New("UNIQUE constraint failed: actor_keys.public_key")
	if !SQLite.IsUniqueViolation(sqliteUnique) {
		t.Error("SQLite should detect its UNIQUE constraint string")
	}
	if SQLite.IsUniqueViolation(errors.New("UNIQUE constraint failed: actors.actor_id")) {
		t.Error("SQLite must stay column-specific (actor_keys.public_key only)")
	}
	if SQLite.IsUniqueViolation(nil) || SQLite.IsUniqueViolation(errors.New("boom")) {
		t.Error("SQLite false positives")
	}

	// Postgres: SQLSTATE 23505 via the duck-typed interface (wrapped too).
	if !Postgres.IsUniqueViolation(&fakePGErr{"23505"}) {
		t.Error("Postgres should detect SQLSTATE 23505")
	}
	if !Postgres.IsUniqueViolation(errors.Join(errors.New("ctx"), &fakePGErr{"23505"})) {
		t.Error("Postgres should detect a wrapped 23505 (errors.As)")
	}
	if Postgres.IsUniqueViolation(&fakePGErr{"23503"}) { // foreign_key_violation
		t.Error("Postgres must not treat 23503 as a unique violation")
	}
	if Postgres.IsUniqueViolation(nil) {
		t.Error("Postgres nil false positive")
	}
}

func TestIsUniqueViolationGeneric(t *testing.T) {
	// SQLite: any table/column counts, including partial-unique-index msgs
	// (the messages the runtime stores actually see for hostnames/secrets/zones).
	for _, msg := range []string{
		"UNIQUE constraint failed: actor_keys.public_key",
		"UNIQUE constraint failed: tenant_hostnames.hostname",
		"UNIQUE constraint failed: index 'tenant_secrets_active_name_idx'",
		"UNIQUE constraint failed: dns_zones.zone",
	} {
		if !SQLite.IsUniqueViolationGeneric(errors.New(msg)) {
			t.Errorf("SQLite generic should match %q", msg)
		}
	}
	// ...but not other constraint failures or nil/unrelated errors.
	for _, e := range []error{
		nil,
		errors.New("NOT NULL constraint failed: tenants.slug"),
		errors.New("FOREIGN KEY constraint failed"),
		errors.New("boom"),
	} {
		if SQLite.IsUniqueViolationGeneric(e) {
			t.Errorf("SQLite generic false positive on %v", e)
		}
	}

	// Postgres: any 23505 (bare + wrapped), not 23503, not nil.
	if !Postgres.IsUniqueViolationGeneric(&fakePGErr{"23505"}) {
		t.Error("Postgres generic should detect SQLSTATE 23505")
	}
	if !Postgres.IsUniqueViolationGeneric(errors.Join(errors.New("ctx"), &fakePGErr{"23505"})) {
		t.Error("Postgres generic should detect a wrapped 23505 (errors.As)")
	}
	if Postgres.IsUniqueViolationGeneric(&fakePGErr{"23503"}) {
		t.Error("Postgres generic must not treat 23503 as a unique violation")
	}
	if Postgres.IsUniqueViolationGeneric(nil) {
		t.Error("Postgres generic nil false positive")
	}

	// Invariant: the NARROW SQLite matcher must stay column-specific so adding
	// the generic method can't broaden auth's ErrKeyAlreadyEnrolled mapping.
	if SQLite.IsUniqueViolation(errors.New("UNIQUE constraint failed: tenant_hostnames.hostname")) {
		t.Error("narrow SQLite.IsUniqueViolation must stay actor_keys.public_key-only (auth byte-identity)")
	}
}
