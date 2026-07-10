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
	// it surfaces as ErrKeyAlreadyEnrolled). Deliberately column-specific
	// on SQLite so an unrelated UNIQUE can't masquerade as that error.
	IsUniqueViolation(err error) bool

	// IsUniqueViolationGeneric reports whether err is *any* unique-constraint
	// violation, regardless of table/column. This is the general check the
	// runtime stores need on Postgres — a duplicate hostname, secret, or DNS
	// zone must map to its typed error on either engine. On Postgres the
	// SQLSTATE (23505) already carries no column identity, so the narrow and
	// generic checks coincide there; the distinction only matters for SQLite's
	// string-based detection.
	IsUniqueViolationGeneric(err error) bool

	// BeginImmediate starts the transaction used by the atomic consume
	// paths (invitation / bootstrap redemption) with an upfront write
	// lock appropriate to the engine.
	BeginImmediate(ctx context.Context, db *sql.DB) (*sql.Tx, error)

	// BeginWrite opens the transaction used by the runtime control-plane
	// write path. SQLite returns a plain tx (its connection-level
	// _txlock=immediate already takes the whole-DB RESERVED write lock, so
	// the whole path serializes). Postgres returns an explicit READ COMMITTED
	// tx so that a LockClause() `FOR UPDATE` BLOCKS on contention rather than
	// aborting — under an accidental SERIALIZABLE session a lock wait would
	// surface as a 40001 serialization failure instead. Distinct from
	// BeginImmediate (which is SERIALIZABLE on Postgres, for the auth registry).
	BeginWrite(ctx context.Context, db *sql.DB) (*sql.Tx, error)

	// LockClause returns the row-lock suffix appended to a SELECT to take a
	// pessimistic lock on the matched rows. Postgres returns " FOR UPDATE";
	// SQLite returns "" (empty — the whole-DB RESERVED lock from BeginWrite
	// already serializes, so no per-row lock exists or is needed). Empty on
	// SQLite means the query text is unchanged.
	LockClause() string
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

// IsUniqueViolationGeneric matches any SQLite unique-constraint failure,
// whatever table/column — the canonical message prefix covers plain UNIQUE
// columns and partial unique indexes alike.
func (sqliteDialect) IsUniqueViolationGeneric(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
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

// BeginWrite: plain tx. The runtime DB is opened with _txlock=immediate, so
// this already takes SQLite's whole-DB RESERVED write lock up front — the
// historical behaviour every runtime write path relied on before this seam.
func (sqliteDialect) BeginWrite(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return db.BeginTx(ctx, nil)
}

// LockClause: empty — SQLite has no row lock (nor `FOR UPDATE`); the whole-DB
// RESERVED lock serializes writers, so appending "" leaves the SQL unchanged.
func (sqliteDialect) LockClause() string { return "" }

// --- Postgres ---------------------------------------------------------

type postgresDialect struct{}

// Rebind rewrites `?` placeholders to `$1, $2, …`, skipping `?` that appear in
// contexts where they are not placeholders: single-quoted string literals (a
// doubled single-quote escapes), double-quoted identifiers (a doubled
// double-quote escapes), -- line comments, and block comments. Two-character JSON
// operators `?|` and `?&` are also left intact.
//
// Known limitation: the *bare* single-char JSON operator `?` (jsonb key-exists)
// is textually indistinguishable from a placeholder and WILL be rebound. Don't
// use it in runtime queries — use the function form `jsonb_exists(col, 'key')`
// (or parameterize) instead. Documented + covered in dialect_test.go.
func (postgresDialect) Rebind(q string) string {
	var b strings.Builder
	b.Grow(len(q) + 8)
	n := 0
	var inStr, inIdent, inLine, inBlock bool
	for i := 0; i < len(q); i++ {
		c := q[i]
		switch {
		case inLine:
			b.WriteByte(c)
			if c == '\n' {
				inLine = false
			}
		case inBlock:
			b.WriteByte(c)
			if c == '*' && i+1 < len(q) && q[i+1] == '/' {
				b.WriteByte('/')
				i++
				inBlock = false
			}
		case inStr:
			b.WriteByte(c)
			if c == '\'' {
				if i+1 < len(q) && q[i+1] == '\'' { // '' escape
					b.WriteByte('\'')
					i++
				} else {
					inStr = false
				}
			}
		case inIdent:
			b.WriteByte(c)
			if c == '"' {
				if i+1 < len(q) && q[i+1] == '"' { // "" escape
					b.WriteByte('"')
					i++
				} else {
					inIdent = false
				}
			}
		case c == '\'':
			b.WriteByte(c)
			inStr = true
		case c == '"':
			b.WriteByte(c)
			inIdent = true
		case c == '-' && i+1 < len(q) && q[i+1] == '-':
			b.WriteByte('-')
			b.WriteByte('-')
			i++
			inLine = true
		case c == '/' && i+1 < len(q) && q[i+1] == '*':
			b.WriteByte('/')
			b.WriteByte('*')
			i++
			inBlock = true
		case c == '?' && i+1 < len(q) && (q[i+1] == '|' || q[i+1] == '&'):
			// Postgres JSON existence operators ?| and ?& — not placeholders.
			b.WriteByte(c)
			b.WriteByte(q[i+1])
			i++
		case c == '?':
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
	return pgIsUniqueViolation(err)
}

// IsUniqueViolationGeneric is identical to IsUniqueViolation on Postgres:
// SQLSTATE 23505 is already table-agnostic, so there is no narrower form to
// distinguish (the narrowing only exists for SQLite's string-based detection).
func (postgresDialect) IsUniqueViolationGeneric(err error) bool {
	return pgIsUniqueViolation(err)
}

// pgIsUniqueViolation reports a Postgres unique_violation (SQLSTATE 23505),
// duck-typing the driver error so core never imports pq/pgx.
func pgIsUniqueViolation(err error) bool {
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

// BeginWrite: explicit READ COMMITTED. The runtime write path takes pessimistic
// row locks (LockClause → `FOR UPDATE`) at its few contended sites; under READ
// COMMITTED those lock waits BLOCK and resolve, and a unique-violation can be
// recovered with ROLLBACK TO SAVEPOINT. Under SERIALIZABLE the same contention
// would raise an unrecoverable 40001 the handlers can't safely retry (they do
// irreversible side effects — fleet artifact uploads — before COMMIT). Being
// explicit guards against an accidental SERIALIZABLE session default.
func (postgresDialect) BeginWrite(ctx context.Context, db *sql.DB) (*sql.Tx, error) {
	return db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
}

// LockClause: `FOR UPDATE` — a pessimistic row lock that blocks concurrent
// writers under READ COMMITTED instead of aborting them.
func (postgresDialect) LockClause() string { return " FOR UPDATE" }

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
