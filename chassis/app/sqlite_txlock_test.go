package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// openRuntimeStyleDB opens a temp SQLite DB with the chassis runtime DSN options
// (WAL + busy_timeout), optionally with _txlock=immediate — mirroring
// openSQLiteOrDie. It returns a DB seeded with a one-row table `t`.
func openRuntimeStyleDB(t *testing.T, immediate bool, busyMs int) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "runtime-test.db") +
		"?mode=rwc&_journal_mode=WAL&_busy_timeout=" + strconv.Itoa(busyMs)
	if immediate {
		dsn += "&_txlock=immediate"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, n INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (n) VALUES (0)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

// TestRuntimeDSNDeferredUpgradeLocks documents the bug the fix targets: with the
// default DEFERRED transaction (no _txlock), a read→write upgrade that happens
// after another connection has committed returns an *unretryable* "database is
// locked" (SQLITE_BUSY_SNAPSHOT). busy_timeout cannot rescue it — this is the
// instant `create_stack` 500 on POST /stacks/<name>/draft, whose handler opens a
// deferred tx, SELECTs `stacks`, then INSERTs.
func TestRuntimeDSNDeferredUpgradeLocks(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeStyleDB(t, false /* deferred */, 2000)

	// Deferred read tx: the SELECT pins this transaction's WAL snapshot, exactly
	// like handleCreateDraft's opening "SELECT ... FROM stacks".
	txReader, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin reader: %v", err)
	}
	defer func() { _ = txReader.Rollback() }()
	var n int
	if err := txReader.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("reader select: %v", err)
	}

	// Another connection commits a write, advancing the DB past that snapshot.
	if _, err := db.ExecContext(ctx, `INSERT INTO t (n) VALUES (1)`); err != nil {
		t.Fatalf("concurrent write: %v", err)
	}

	// The reader now upgrades read→write against a stale snapshot → immediate
	// lock, regardless of the 2s busy_timeout.
	if _, err := txReader.ExecContext(ctx, `INSERT INTO t (n) VALUES (2)`); err == nil {
		t.Fatal("expected deferred read→write upgrade to fail with a lock error")
	} else if !strings.Contains(err.Error(), "locked") {
		t.Fatalf("expected a 'database is locked' upgrade error, got: %v", err)
	}
}

// TestRuntimeDSNImmediateSerializes verifies the fix: with _txlock=immediate,
// BeginTx takes the RESERVED write lock up front, so two transactions each doing
// SELECT-then-INSERT serialize on busy_timeout and BOTH commit — no unretryable
// upgrade lock. This is what the runtime DB now does (openSQLiteOrDie appends
// _txlock=immediate for kind=="runtime").
func TestRuntimeDSNImmediateSerializes(t *testing.T) {
	ctx := context.Background()
	db := openRuntimeStyleDB(t, true /* immediate */, 5000)

	// txA takes the write lock at BEGIN and does SELECT+INSERT, holding the lock
	// until we commit it below.
	txA, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	var n int
	if err := txA.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("A select: %v", err)
	}
	if _, err := txA.ExecContext(ctx, `INSERT INTO t (n) VALUES (1)`); err != nil {
		t.Fatalf("A insert: %v", err)
	}

	// txB (a second connection) runs the same pattern concurrently. Its BeginTx
	// blocks on the write lock until txA commits, then proceeds; it must never
	// see "database is locked".
	started := make(chan struct{})
	var bErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		txB, err := db.BeginTx(ctx, nil) // waits for txA's RESERVED lock
		if err != nil {
			bErr = err
			return
		}
		var m int
		if err := txB.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&m); err != nil {
			bErr = err
			_ = txB.Rollback()
			return
		}
		if _, err := txB.ExecContext(ctx, `INSERT INTO t (n) VALUES (2)`); err != nil {
			bErr = err
			_ = txB.Rollback()
			return
		}
		bErr = txB.Commit()
	}()

	<-started
	// Let txB reach BeginTx and block on the lock, then release it. The
	// assertions below hold regardless of the exact interleave — _txlock=immediate
	// + busy_timeout guarantees txB waits rather than erroring — so this is not a
	// timing-flaky test.
	time.Sleep(150 * time.Millisecond)
	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit: %v", err)
	}
	wg.Wait()

	if bErr != nil {
		t.Fatalf("txB should serialize behind txA, not lock: %v", bErr)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM t`).Scan(&total); err != nil {
		t.Fatalf("final count: %v", err)
	}
	if total != 3 { // seed + A + B
		t.Fatalf("expected 3 rows after serialized writes, got %d", total)
	}
}
