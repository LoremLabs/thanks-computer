package dataset

import (
	"context"
	"database/sql"
	"fmt"
)

// IntegrityCheck opens the SQLite file at path read-only and runs the full
// `PRAGMA integrity_check`. Slow on multi-GB artifacts by design — callers
// run it once per new artifact (the CLI before first upload), not per
// deploy. A non-SQLite file fails here with "file is not a database".
//
// The plain sqlite3 driver would work too, but going through the dataset
// driver keeps every dataset file open in this codebase on the same
// restricted path.
func IntegrityCheck(path string) error {
	db, err := sql.Open(DriverName, DSN(path))
	if err != nil {
		return fmt.Errorf("dataset open: %w", err)
	}
	defer db.Close()
	var verdict string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&verdict); err != nil {
		return fmt.Errorf("dataset integrity_check: %w", err)
	}
	if verdict != "ok" {
		return fmt.Errorf("dataset integrity_check failed: %s", verdict)
	}
	return nil
}

// ValidateArtifact opens the artifact at path and prepares every query the
// manifest declares. This is the activation gate: it proves the SQL
// compiles against the ACTUAL shipped schema (missing tables/columns fail
// here, before the version goes live) and — because preparation runs under
// the read-only authorizer — that no declared query is anything but a
// read. Returns the first failure, named for the deploy error.
func ValidateArtifact(ctx context.Context, path string, m *Manifest) error {
	db, err := sql.Open(DriverName, DSN(path))
	if err != nil {
		return fmt.Errorf("dataset open: %w", err)
	}
	defer db.Close()
	for _, name := range m.QueryNames() {
		stmt, err := db.PrepareContext(ctx, m.Queries[name].SQL)
		if err != nil {
			return fmt.Errorf("query %q: %w", name, err)
		}
		_ = stmt.Close()
	}
	return nil
}
