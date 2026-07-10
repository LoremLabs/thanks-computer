package registry

import (
	"context"
	"database/sql"
	"testing"
	// go-sqlite3 is registered by oidc_test.go in this same test binary.
)

func mustBegin(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	return tx
}

func TestValidSavepointName(t *testing.T) {
	ok := []string{"sp", "sp_vivify", "ehs", "_x", "A9"}
	bad := []string{"", "9sp", "bad name", "sp;", "sp-1", "sp'", "drop table"}
	for _, s := range ok {
		if !validSavepointName(s) {
			t.Errorf("validSavepointName(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if validSavepointName(s) {
			t.Errorf("validSavepointName(%q) = true, want false", s)
		}
	}
}

func TestRunInSavepoint(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE t (k TEXT UNIQUE)`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	// (a) Invalid name → error, no SQL executed (tx untouched).
	txA := mustBegin(t, db)
	if err := RunInSavepoint(ctx, txA, "bad name", func() error { return nil }); err == nil {
		t.Error("invalid savepoint name must error")
	}
	_ = txA.Rollback()

	// (b) Success path: op runs inside the savepoint, released, tx commits.
	txB := mustBegin(t, db)
	if err := RunInSavepoint(ctx, txB, "sp", func() error {
		_, e := txB.ExecContext(ctx, `INSERT INTO t(k) VALUES('a')`)
		return e
	}); err != nil {
		t.Fatalf("success path: %v", err)
	}
	if err := txB.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// (c) op fails with a unique violation. The helper must return the driver
	// error UNCHANGED so IsUniqueViolationGeneric can classify it, and the tx
	// must remain usable (the savepoint was rolled back + released).
	txC := mustBegin(t, db)
	cerr := RunInSavepoint(ctx, txC, "sp", func() error {
		_, e := txC.ExecContext(ctx, `INSERT INTO t(k) VALUES('a')`) // duplicate
		return e
	})
	if cerr == nil {
		t.Fatal("expected the op's unique-violation error")
	}
	if !SQLite.IsUniqueViolationGeneric(cerr) {
		t.Errorf("helper must return the op error unwrapped (classifiable); got %v", cerr)
	}
	if _, e := txC.ExecContext(ctx, `INSERT INTO t(k) VALUES('b')`); e != nil {
		t.Errorf("tx must stay usable after savepoint recovery: %v", e)
	}
	if e := txC.Commit(); e != nil {
		t.Errorf("commit after recovery: %v", e)
	}

	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 { // 'a' (from b) + 'b' (from c); the duplicate 'a' rolled back
		t.Errorf("row count = %d, want 2", n)
	}
}
