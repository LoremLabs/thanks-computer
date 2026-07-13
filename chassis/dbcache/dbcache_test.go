package dbcache

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
// overlay-before-publish ordering. The dump+replay AND the OnReload overlay
// both run off Mu now — the overlay is applied to the still-private new
// mirror BEFORE the pointer swap — so a Snapshot() reader must never observe
// a freshly reloaded row ('b') without the overlay row. If a future change
// swaps first and overlays after (or overlays a published handle), this
// fails.
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

// TestSnapshotNotBlockedDuringSlowOnReload pins the A″ fix from
// todo-control-plane-reload-scaling.md: the OnReload derived-cache chain is
// O(fleet) at production scale, so it must run off Mu — a Snapshot() caller
// must get the PREVIOUS mirror promptly while the hook chain is still
// running, never queue behind it.
func TestSnapshotNotBlockedDuringSlowOnReload(t *testing.T) {
	src := newTestSource(t)
	defer src.Close()

	dbc, err := New(config.Config{}, zap.NewNop(), context.Background(), src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := dbc.Reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	before := dbc.Snapshot()

	entered := make(chan struct{})
	release := make(chan struct{})
	dbc.OnReload = func(db *sql.DB) error {
		close(entered)
		<-release
		_, e := db.Exec(`INSERT INTO t (k) VALUES ('overlay')`)
		return e
	}

	done := make(chan error, 1)
	go func() { done <- dbc.Reload() }()
	<-entered

	// The hook chain is now mid-flight. Snapshot() must return the previous
	// (still-published) mirror without waiting for it.
	got := make(chan *sql.DB, 1)
	go func() { got <- dbc.Snapshot() }()
	select {
	case snap := <-got:
		if snap != before {
			t.Error("Snapshot() during OnReload returned an unpublished mirror")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot() blocked while OnReload was running (hook chain is back under Mu)")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !has(t, dbc.Snapshot(), "overlay") {
		t.Error("published mirror missing the overlay row")
	}
}

// TestReloadAfterWriteSQLiteIsSynchronous pins the dialect split: on the
// local SQLite file runtime the file IS the source of truth, so a reader
// immediately after ReloadAfterWrite returns must see the new row.
func TestReloadAfterWriteSQLiteIsSynchronous(t *testing.T) {
	src := newTestSource(t)
	defer src.Close()

	dbc, err := New(config.Config{}, zap.NewNop(), context.Background(), src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := dbc.Reload(); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	if _, err := src.Exec(`INSERT INTO t (k) VALUES ('written')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := dbc.ReloadAfterWrite(); err != nil {
		t.Fatalf("ReloadAfterWrite: %v", err)
	}
	if !has(t, dbc.Snapshot(), "written") {
		t.Error("row not visible immediately after ReloadAfterWrite on the SQLite runtime")
	}
}

// TestReloadAfterWritePostgresIsAsyncAndCoalesced pins the shared-runtime
// behavior: ReloadAfterWrite returns immediately, a burst of calls coalesces
// into ~one background reload, and the mirror catches up shortly after.
func TestReloadAfterWritePostgresIsAsyncAndCoalesced(t *testing.T) {
	// Each load stamps its generation so the test can watch a NEW mirror
	// arrive (the row's presence, not just the handle, proves the reload ran).
	var loads atomic.Int64
	RegisterLoader("postgres", func(ctx context.Context, dst, src *sql.DB, _ string) error {
		n := loads.Add(1)
		if _, err := dst.ExecContext(ctx, `CREATE TABLE t (k TEXT PRIMARY KEY)`); err != nil {
			return err
		}
		_, err := dst.ExecContext(ctx, `INSERT INTO t (k) VALUES (?)`, fmt.Sprintf("gen-%d", n))
		return err
	})

	src := newTestSource(t)
	defer src.Close()

	dbc, err := New(config.Config{DbRuntimeDsn: "postgres://ignored"}, zap.NewNop(), context.Background(), src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := dbc.Reload(); err != nil { // gen-1 mirror
		t.Fatalf("initial reload: %v", err)
	}
	baseline := loads.Load()

	// A write burst: every call must return without blocking on a reload.
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := dbc.ReloadAfterWrite(); err != nil {
			t.Fatalf("ReloadAfterWrite: %v", err)
		}
	}
	if took := time.Since(start); took > reloadDebounceQuiet/2 {
		t.Errorf("ReloadAfterWrite burst took %v — it must not block on a reload", took)
	}

	// Trailing-edge debounce: one background reload ~reloadDebounceQuiet
	// after the last call. Poll for the next-generation mirror.
	want := fmt.Sprintf("gen-%d", baseline+1)
	deadline := time.Now().Add(10 * time.Second)
	for !has(t, dbc.Snapshot(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("mirror never refreshed to %s after debounced ReloadAfterWrite", want)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Coalescing: 5 tightly-spaced calls must not cost 5 reloads. Allow 2 in
	// case a scheduler hiccup lets the quiet window elapse mid-burst.
	if got := loads.Load() - baseline; got > 2 {
		t.Errorf("burst of 5 ReloadAfterWrite calls ran %d reloads, want coalesced (≤2)", got)
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
