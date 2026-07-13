package controlapply

import (
	"context"
	"database/sql"
	"sync/atomic"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// fakePGSource serves its batches in order then goes empty, and records Acks.
type fakePGSource struct {
	batches [][]controlevent.Event
	polls   int
	acks    []string
}

func (f *fakePGSource) Name() string { return "fake-pg" }
func (f *fakePGSource) Poll(ctx context.Context, since uint64) ([]controlevent.Event, error) {
	if f.polls >= len(f.batches) {
		f.polls++
		return nil, nil
	}
	b := f.batches[f.polls]
	f.polls++
	return b, nil
}
func (f *fakePGSource) Ack(ctx context.Context, eventID string) error {
	f.acks = append(f.acks, eventID)
	return nil
}

// TestApplierInvalidateOnlyOnPostgres proves Phase 2b: on a postgres:// runtime
// the applier degenerates to a mirror-invalidation consumer — a poll batch
// triggers exactly ONE coalesced Dbc.Reload(), every event is Acked, and the
// shared runtime DB is NOT written (no cursor / applied_events). No real
// Postgres needed: a fake "postgres" mirror loader stands in for the re-source.
func TestApplierInvalidateOnlyOnPostgres(t *testing.T) {
	ctx := context.Background()

	var reloads atomic.Int64
	dbcache.RegisterLoader("postgres", func(ctx context.Context, dst, src *sql.DB, _ string) error {
		reloads.Add(1)
		_, err := dst.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS mirror_marker (x INTEGER)`)
		return err
	})

	// A local SQLite handle stands in for c.pu.RuntimeDB — the invalidate path
	// must NEVER touch it (that's the whole point on a shared PG store).
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE varvals (var TEXT, val TEXT, UNIQUE(var))`); err != nil {
		t.Fatalf("schema: %v", err)
	}

	conf := config.Config{DbRuntimeDsn: "postgres://ignored", FeedSource: "nats", FeedPollPeriod: 1}
	dbc, err := dbcache.New(conf, zap.NewNop(), ctx, db) // selects the "postgres" loader
	if err != nil {
		t.Fatalf("dbcache.New: %v", err)
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), RuntimeDB: db, Dbc: dbc}

	src := &fakePGSource{batches: [][]controlevent.Event{{
		{EventID: "e1", Type: controlevent.TypeStackActivated, ControlVersion: 10},
		{EventID: "e2", Type: "tenant.updated", ControlVersion: 11},
	}}}
	// admin + astore are nil: the invalidate path uses neither.
	c := NewController(ctx, pu, nil, src, nil)

	if !c.invalidateOnly() {
		t.Fatal("invalidateOnly() must be true for a postgres:// runtime DSN")
	}

	before := reloads.Load()
	c.PollOnceForTest(ctx)

	if got := reloads.Load() - before; got != 1 {
		t.Errorf("mirror reloads = %d, want 1 (one coalesced reload per batch)", got)
	}
	if len(src.acks) != 2 {
		t.Errorf("acks = %v, want both events acked", src.acks)
	}
	// The shared runtime DB must be untouched — no cursor row written.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM varvals WHERE var = ?`, cursorVar).Scan(&n); err != nil {
		t.Fatalf("cursor count: %v", err)
	}
	if n != 0 {
		t.Errorf("cursor written to the shared runtime DB (%d rows) — the PG path must not write it", n)
	}

	// A second poll returns no events → no extra reload.
	c.PollOnceForTest(ctx)
	if got := reloads.Load() - before; got != 1 {
		t.Errorf("reloads after empty poll = %d, want still 1", got)
	}
}

// TestApplierInvalidateDrainsBeforeReload pins the drain-then-reload shape:
// a burst spanning several feed batches must cost ONE mirror reload, not one
// per batch (the reload-treadmill class — see
// todo-control-plane-reload-scaling.md), with every drained event acked only
// after the reload succeeds.
func TestApplierInvalidateDrainsBeforeReload(t *testing.T) {
	ctx := context.Background()

	var reloads atomic.Int64
	dbcache.RegisterLoader("postgres", func(ctx context.Context, dst, src *sql.DB, _ string) error {
		reloads.Add(1)
		_, err := dst.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS mirror_marker (x INTEGER)`)
		return err
	})

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	conf := config.Config{DbRuntimeDsn: "postgres://ignored", FeedSource: "nats", FeedPollPeriod: 1}
	dbc, err := dbcache.New(conf, zap.NewNop(), ctx, db)
	if err != nil {
		t.Fatalf("dbcache.New: %v", err)
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), RuntimeDB: db, Dbc: dbc}

	// Three consecutive batches, as the durable consumer would serve a burst.
	src := &fakePGSource{batches: [][]controlevent.Event{
		{{EventID: "e1", ControlVersion: 10}, {EventID: "e2", ControlVersion: 11}},
		{{EventID: "e3", ControlVersion: 12}},
		{{EventID: "e4", ControlVersion: 13}, {EventID: "e5", ControlVersion: 14}},
	}}
	c := NewController(ctx, pu, nil, src, nil)

	before := reloads.Load()
	c.PollOnceForTest(ctx)

	if got := reloads.Load() - before; got != 1 {
		t.Errorf("mirror reloads = %d, want 1 (drain the burst, then one coalesced reload)", got)
	}
	if len(src.acks) != 5 {
		t.Errorf("acks = %v, want all 5 drained events acked", src.acks)
	}
}
