package registry

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Dialect isolates the handful of SQL behaviours that differ between the
// in-tree default (SQLite) and a shared Postgres auth store used by an
// HA control plane. The registry SQL is otherwise identical: timestamps
// are RFC3339 TEXT, JSON is TEXT, booleans are INTEGER 0/1, and upserts
// use the `INSERT … ON CONFLICT(...) DO UPDATE SET … excluded.…` form
// that both engines accept — so nothing here touches Go scan/marshal.
//
// Only three things vary:
//
//   - Placeholders: SQLite binds `?`, Postgres binds `$1, $2, …`.
//   - Unique-violation detection: SQLite carries it in the error string,
//     Postgres in SQLSTATE 23505. We never import a driver here — the
//     Postgres case duck-types `interface{ SQLState() string }` so core
//     keeps SQLite as its only compiled driver.
//   - The upfront write lock: SQLite uses `BEGIN IMMEDIATE` to fail fast
//     instead of deadlocking mid-transaction; Postgres uses a
//     SERIALIZABLE transaction. Correctness of the single-winner
//     consume paths comes from the conditional `UPDATE … WHERE
//     consumed_at IS NULL` (rows-affected) guard, which is
//     dialect-neutral — this is only the lock strategy.
type Dialect interface {
	// Rebind rewrites `?` placeholders for the target engine. SQLite
	// returns the query unchanged; Postgres returns `$1, $2, …`.
	Rebind(query string) string

	// IsUniqueViolation reports whether err is the actor_keys public-key
	// uniqueness conflict (the only UNIQUE the registry distinguishes —
	// it surfaces as ErrKeyAlreadyEnrolled).
	IsUniqueViolation(err error) bool

	// BeginImmediate starts the transaction used by the atomic consume
	// paths (invitation / bootstrap redemption) with an upfront write
	// lock appropriate to the engine.
	BeginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error)
}

// SQLite is the default dialect — identity placeholders, error-string
// unique detection, and the historical `BEGIN IMMEDIATE` behaviour.
// Behaviour is byte-for-byte what the registry did before the seam.
var SQLite Dialect = sqliteDialect{}

// Postgres is selected when the auth DSN is a postgres:// URL. The
// driver itself is registered out of tree (overlay blank-import), never
// compiled into core.
var Postgres Dialect = postgresDialect{}

// DialectForDSN picks the dialect from an auth DSN. Anything that isn't a
// recognised Postgres URL stays on SQLite (the safe default).
func DialectForDSN(dsn string) Dialect {
	s := strings.ToLower(strings.TrimSpace(dsn))
	if strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://") {
		return Postgres
	}
	return SQLite
}

// --- SQLite -----------------------------------------------------------

type sqliteDialect struct{}

func (sqliteDialect) Rebind(q string) string { return q }

// isUniqueConstraintErr matched the SQLite message historically; keep it
// exactly (column-specific so an unrelated UNIQUE doesn't masquerade as
// ErrKeyAlreadyEnrolled).
func (sqliteDialect) IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") &&
		strings.Contains(msg, "actor_keys.public_key")
}

func (sqliteDialect) BeginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Best-effort upfront write lock. Tolerate "within a transaction"
	// (database/sql already opened one) — historical behaviour.
	if _, err := tx.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if !strings.Contains(err.Error(), "within a transaction") {
			_ = tx.Rollback()
			return nil, err
		}
	}
	return tx, nil
}

// --- Postgres ---------------------------------------------------------

type postgresDialect struct{}

func (postgresDialect) Rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	inStr := false // inside a '...' string literal — don't rebind ? there
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case c == '\'':
			// Handle the '' escape inside a string literal.
			b.WriteByte(c)
			if inStr && i+1 < len(q) && q[i+1] == '\'' {
				b.WriteByte(q[i+1])
				i++
				continue
			}
			inStr = !inStr
		case c == '?' && !inStr:
			n++
			b.WriteByte('$')
			b.WriteString(itoa(n))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// sqlStater is implemented by both lib/pq (*pq.Error) and pgx
// (*pgconn.PgError) without us importing either.
type sqlStater interface{ SQLState() string }

func (postgresDialect) IsUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var s sqlStater
	if errors.As(err, &s) {
		return s.SQLState() == "23505" // unique_violation
	}
	// Fallback: some drivers only stringify it.
	return strings.Contains(err.Error(), "SQLSTATE 23505") ||
		strings.Contains(err.Error(), "unique constraint")
}

func (postgresDialect) BeginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
}

// itoa is a tiny strconv.Itoa to avoid the import churn for a 1-2 digit
// placeholder index (queries never have hundreds of params).
func itoa(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	var buf [4]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
