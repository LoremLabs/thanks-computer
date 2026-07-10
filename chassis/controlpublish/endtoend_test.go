package controlpublish_test

// End-to-end integration test for fleet sync: a producer chassis
// writes an outbox row, the pump drains it via the file Sink (events
// land in a shared directory + artifact store), and a consumer
// chassis's applier picks them up and mutates its own runtime.db.
//
// This is the smallest concrete proof that the open-core P1
// scaffolding works end-to-end on a single backend. The JetStream
// backend (P2, service overlay) plugs in at the feed.RegisterSink
// / feed.Register seam without changes to anything here.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
	_ "github.com/loremlabs/thanks-computer/chassis/artifact/filestore"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlapply"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/controlpublish"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/feed"
	_ "github.com/loremlabs/thanks-computer/chassis/feed/filesource"
	_ "github.com/loremlabs/thanks-computer/chassis/feed/nop"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/admin"
)

// chassisSchema is the minimum runtime.db shape both sides need:
// outbox + applied_events + varvals (cursor) + the target table
// (tenants for the RowsArtifact path used here).
const chassisSchema = `
CREATE TABLE varvals (var TEXT, val TEXT, UNIQUE(var));
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT);
CREATE TABLE applied_events (event_id TEXT PRIMARY KEY, control_version INTEGER NOT NULL, applied_at TEXT NOT NULL);
CREATE TABLE control_events_outbox (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id                  TEXT NOT NULL UNIQUE,
    event_type                TEXT NOT NULL,
    tenant_id                 TEXT,
    stack_id                  TEXT,
    version                   INTEGER,
    base_version              INTEGER,
    artifact_ref              TEXT,
    checksum                  TEXT,
    payload_json              BLOB NOT NULL,
    created_at                TEXT NOT NULL,
    attempt_count             INTEGER NOT NULL DEFAULT 0,
    last_error                TEXT,
    last_attempt_at           TEXT,
    published_control_version INTEGER,
    published_at              TEXT
);
`

type chassisEnd struct {
	db     *sql.DB
	pu     *processor.Unit
	dbc    *dbcache.DbCache
	astore artifact.Store
}

func newChassis(t *testing.T, label, dbPath, feedDir, artDir string) *chassisEnd {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc&cache=shared")
	if err != nil {
		t.Fatalf("%s: open db: %v", label, err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(chassisSchema); err != nil {
		t.Fatalf("%s: schema: %v", label, err)
	}
	conf := config.Config{
		DbRuntimeDsn:         "file:" + dbPath,
		FeedSource:           "file",
		FeedSink:             "file",
		FeedSourceFileDir:    feedDir,
		FeedPollPeriod:       1,
		FeedSinkBatchSize:    32,
		ArtifactStore:        "file",
		ArtifactStoreFileDir: artDir,
	}
	dbc, err := dbcache.New(conf, zap.NewNop(), context.Background(), db)
	if err != nil {
		t.Fatalf("%s: dbcache: %v", label, err)
	}
	if err := dbc.Reload(); err != nil {
		t.Fatalf("%s: dbcache reload: %v", label, err)
	}
	astore, err := artifact.Open("file", artifact.StoreConfig{FileDir: artDir})
	if err != nil {
		t.Fatalf("%s: artifact: %v", label, err)
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), RuntimeDB: db, Dbc: dbc}
	return &chassisEnd{db: db, pu: pu, dbc: dbc, astore: astore}
}

// TestProducerToConsumerEndToEnd: chassis A writes an outbox row →
// pump drains via file Sink → chassis B's file Source picks it up →
// applier upserts the row in B's tenants table.
func TestProducerToConsumerEndToEnd(t *testing.T) {
	root := t.TempDir()
	feedDir := filepath.Join(root, "feed")
	artDir := filepath.Join(root, "art")
	a := newChassis(t, "A", filepath.Join(root, "a.db"), feedDir, artDir)
	b := newChassis(t, "B", filepath.Join(root, "b.db"), feedDir, artDir)

	// Producer side wiring on A.
	sink, err := feed.OpenSink("file", feed.SourceConfig{FileDir: feedDir})
	if err != nil {
		t.Fatalf("open sink: %v", err)
	}
	pump := controlpublish.NewController(context.Background(), a.pu, sink)

	// Consumer side wiring on B.
	src, err := feed.Open("file", feed.SourceConfig{FileDir: feedDir})
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	bAdmin := admin.NewController(context.Background(), b.pu)
	applier := controlapply.NewController(context.Background(), b.pu, bAdmin, src, b.astore)

	// 1. Producer uploads artifact bytes (this is what an admin
	// handler would do BEFORE opening its tx).
	rows := controlevent.RowsArtifact{
		DB:    "runtime",
		Table: "tenants",
		Op:    "upsert",
		Rows:  []map[string]any{{"tenant_id": "tnt_fleet", "slug": "from-A"}},
	}
	artBytes, _ := json.Marshal(rows)
	artRef := "rows/tnt_fleet"
	if err := a.astore.Put(context.Background(), artRef, artBytes, []byte(`{}`)); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	checksum := "sha256:" + sha256OfBytes(artBytes)

	// 2. Producer writes the outbox row (same SQLite tx an admin
	// handler would use).
	eventID := "evt_" + hxid.NewTimeSort().String()
	ev := controlevent.Event{
		EventID:     eventID,
		Type:        controlevent.TypeTenantCreated,
		TenantID:    "tnt_fleet",
		ArtifactRef: artRef,
		Checksum:    checksum,
	}
	payload, _ := json.Marshal(ev)

	tx, err := a.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := controlpublish.AppendOutbox(context.Background(), tx,
		eventID, ev.Type, ev.TenantID, "", 0, 0,
		ev.ArtifactRef, ev.Checksum, payload, nil); err != nil {
		t.Fatalf("append outbox: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 3. Pump drains the outbox → file Sink writes to feed dir.
	pump.DrainForTest(context.Background())

	// Verify outbox row is now marked published.
	var pcv sql.NullInt64
	if err := a.db.QueryRow(
		`SELECT published_control_version FROM control_events_outbox WHERE event_id = ?`,
		eventID).Scan(&pcv); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if !pcv.Valid {
		t.Fatalf("producer A: outbox row never marked published")
	}

	// 4. Consumer applier polls the feed dir, fetches the artifact,
	// applies to B's runtime.db.
	applier.PollOnceForTest(context.Background())

	var slug string
	if err := b.db.QueryRow(
		`SELECT slug FROM tenants WHERE tenant_id = ?`, "tnt_fleet").Scan(&slug); err != nil {
		t.Fatalf("consumer B never received tenant row: %v", err)
	}
	if slug != "from-A" {
		t.Errorf("consumer B got slug=%q, want %q", slug, "from-A")
	}

	// 5. Idempotent re-application: re-running the applier MUST NOT
	// re-apply (applied_events guard). Cursor still advances normally;
	// data unchanged.
	applier.PollOnceForTest(context.Background())
	var n int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE tenant_id = ?`,
		"tnt_fleet").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("idempotency broken: tenants row count = %d, want 1", n)
	}
}

func sha256OfBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
