package registry

import (
	"context"
	"database/sql"
	"fmt"
)

// RunInSavepoint runs op inside a named SAVEPOINT so a recoverable failure — in
// particular a Postgres unique_violation (SQLSTATE 23505), which otherwise
// poisons the whole transaction until a rollback — leaves the enclosing tx
// usable. It returns op's error UNCHANGED for the caller to classify (e.g.
// IsUniqueViolationGeneric → re-lookup/adopt); on that error it first ROLLBACKs
// TO the savepoint. It always RELEASEs the savepoint afterward.
//
// Unlike an ad-hoc inline SAVEPOINT/RELEASE, this surfaces SAVEPOINT / ROLLBACK
// TO / RELEASE control errors rather than dropping them: a release/rollback
// failure means the transaction is not in the state the caller assumes, and
// should abort the write, not be silently ignored. Such a control error is
// wrapped (never a 23505), so IsUniqueViolationGeneric won't misclassify it as
// the recoverable case.
//
// Portable: SAVEPOINT syntax is identical on SQLite and Postgres. On SQLite the
// rollback is merely unnecessary for a constraint error (SQLite localizes it
// rather than poisoning the tx) — the savepoint ops are still real transaction
// control there, not no-ops. name must be a fixed identifier (these do not nest
// recursively per call).
func RunInSavepoint(ctx context.Context, tx *sql.Tx, name string, op func() error) error {
	// Savepoint identifiers cannot be parameterized, so they are interpolated
	// into the SQL. Callers pass fixed internal names; validate anyway so a
	// future caller can't inject via a non-identifier name.
	if !validSavepointName(name) {
		return fmt.Errorf("registry: invalid savepoint name %q (want [A-Za-z_][A-Za-z0-9_]*)", name)
	}
	if _, err := tx.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("savepoint %s: %w", name, err)
	}

	opErr := op()
	if opErr == nil {
		// Success: RELEASE. A release failure is the only error to report.
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name); err != nil {
			return fmt.Errorf("release savepoint %s: %w", name, err)
		}
		return nil
	}

	// op failed: roll the savepoint back so the enclosing tx stays usable, then
	// release it. A cleanup failure takes PRECEDENCE and wraps opErr for context
	// (the tx is not in the assumed state — the caller must not treat this as a
	// recoverable unique-violation). On CLEAN cleanup, return opErr UNCHANGED so
	// the caller can classify it (IsUniqueViolationGeneric) — do not wrap it.
	if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name); err != nil {
		return fmt.Errorf("rollback to savepoint %s (after op error: %v): %w", name, opErr, err)
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+name); err != nil {
		return fmt.Errorf("release savepoint %s (after op error: %v): %w", name, opErr, err)
	}
	return opErr
}

// validSavepointName reports whether s is a safe SQL identifier for use as a
// savepoint name: a leading letter or underscore, then letters/digits/underscores.
func validSavepointName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
