package dbcache

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
)

// TestReloadFailingLoaderKeepsSnapshot proves the availability buffer that the
// Postgres read path relies on: when a MirrorLoader fails (e.g. a Neon blip),
// Reload() errors and the PREVIOUS mirror stays live — the swap only happens
// after a successful build. This is pure open-core logic (a fake loader), no
// Postgres needed; it guards the seam Seam B introduced.
func TestReloadFailingLoaderKeepsSnapshot(t *testing.T) {
	var failNow atomic.Bool
	RegisterLoader("postgres", func(ctx context.Context, dst, src *sql.DB, _ string) error {
		if failNow.Load() {
			return errors.New("simulated store outage")
		}
		if _, err := dst.ExecContext(ctx, `CREATE TABLE marker (x INTEGER)`); err != nil {
			return err
		}
		_, err := dst.ExecContext(ctx, `INSERT INTO marker(x) VALUES (1)`)
		return err
	})

	// A postgres:// runtime DSN selects the "postgres" loader in New.
	src, err := sql.Open("sqlite3", ":memory:") // unused by the fake loader, but non-nil
	if err != nil {
		t.Fatalf("open src: %v", err)
	}
	defer src.Close()

	dbc, err := New(config.Config{DbRuntimeDsn: "postgres://ignored"}, zap.NewNop(), context.Background(), src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if dbc.loaderName != "postgres" {
		t.Fatalf("loaderName = %q, want postgres", dbc.loaderName)
	}

	// First reload succeeds → mirror carries the marker row.
	if err := dbc.Reload(); err != nil {
		t.Fatalf("first Reload: %v", err)
	}
	good := dbc.Snapshot()
	var n int
	if err := good.QueryRow(`SELECT x FROM marker`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("marker after good reload: n=%d err=%v", n, err)
	}

	// Second reload fails → Reload errors, the handle is NOT swapped, and the
	// good mirror keeps serving.
	failNow.Store(true)
	if err := dbc.Reload(); err == nil {
		t.Fatal("expected Reload to fail when the loader errors")
	}
	if dbc.Snapshot() != good {
		t.Error("mirror handle was swapped despite a failed reload (availability buffer broken)")
	}
	if err := dbc.Snapshot().QueryRow(`SELECT x FROM marker`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("marker after failed reload: n=%d err=%v (old mirror must stay live)", n, err)
	}
}

// TestBuiltinSQLiteLoaderRegistered confirms the built-in loader self-registers
// so a file: runtime always has a loader (the open-core default path).
func TestBuiltinSQLiteLoaderRegistered(t *testing.T) {
	if _, ok := lookupLoader("sqlite"); !ok {
		t.Fatal(`built-in "sqlite" mirror loader is not registered`)
	}
}
