package dbcache

import (
	"context"
	"database/sql"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// newTestSource builds an in-memory runtime DB to dump from. MaxOpenConns(1)
// is required: each go-sqlite3 connection gets its own :memory: DB.
func newTestSource(t *testing.T) *sql.DB {
	t.Helper()
	src, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	src.SetMaxOpenConns(1)
	if _, err := src.Exec(`CREATE TABLE t (k TEXT PRIMARY KEY)`); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if _, err := src.Exec(`INSERT INTO t (k) VALUES ('a')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return src
}

// has reports whether key k is present in db. Safe to call from goroutines
// (uses t.Errorf, never t.Fatal).
func has(t *testing.T, db *sql.DB, k string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM t WHERE k = ?`, k).Scan(&n); err != nil {
		t.Errorf("query %q: %v", k, err)
		return false
	}
	return n > 0
}

// TestReloadOverlayNeverPartiallyVisible is the regression guard for the
// reloadMu/Mu split. The dump+replay now runs OUTSIDE Mu (so it can't block
// readers), but the swap + OnReload overlay still share Mu — so a Snapshot()
// reader must never observe a freshly reloaded row ('b') without the overlay
// row that OnReload applies in the same critical section. If a future change
// moves the swap out from under the overlay, this fails.
func TestReloadOverlayNeverPartiallyVisible(t *testing.T) {
	src := newTestSource(t)
	defer src.Close()

	dbc, err := New(config.Config{}, zap.NewNop(), context.Background(), src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Overlay re-applied into every freshly built mirror (mirrors sysops
	// re-applying the trusted opstacks the dump doesn't carry).
	dbc.OnReload = func(db *sql.DB) error {
		_, e := db.Exec(`INSERT INTO t (k) VALUES ('overlay')`)
		return e
	}
	if err := dbc.Reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	// A new durable row in the source; reloads will mirror it.
	if _, err := src.Exec(`INSERT INTO t (k) VALUES ('b')`); err != nil {
		t.Fatalf("insert b: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			snap := dbc.Snapshot()
			if has(t, snap, "b") && !has(t, snap, "overlay") {
				t.Errorf("invariant violated: saw reloaded row 'b' without the OnReload overlay")
				return
			}
		}
	}()

	for i := 0; i < 50; i++ {
		if err := dbc.Reload(); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	snap := dbc.Snapshot()
	if !has(t, snap, "a") || !has(t, snap, "b") || !has(t, snap, "overlay") {
		t.Errorf("final mirror missing rows: a=%v b=%v overlay=%v",
			has(t, snap, "a"), has(t, snap, "b"), has(t, snap, "overlay"))
	}
}

// TestReloadConcurrentReadersAndReloads stresses the lock split under -race:
// many Snapshot() readers concurrent with repeated Reload()s must never race,
// deadlock, or lose a durably-seeded row.
func TestReloadConcurrentReadersAndReloads(t *testing.T) {
	src := newTestSource(t)
	defer src.Close()

	dbc, err := New(config.Config{}, zap.NewNop(), context.Background(), src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := dbc.Reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if !has(t, dbc.Snapshot(), "a") {
					t.Errorf("reader saw mirror without seeded row 'a'")
					return
				}
			}
		}()
	}

	for i := 0; i < 40; i++ {
		if err := dbc.Reload(); err != nil {
			t.Fatalf("reload %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if !has(t, dbc.Snapshot(), "a") {
		t.Error("row 'a' lost after concurrent reloads")
	}
}
