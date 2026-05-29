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
	}
	for _, c := range cases {
		if got := Postgres.Rebind(c.in); got != c.want {
			t.Errorf("Postgres.Rebind(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
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
