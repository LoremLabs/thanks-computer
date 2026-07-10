package controlpublish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// outboxSchema is the minimum runtime.db shape these tests need.
// Mirrors db/schema/sqlite/runtime/0009_control_events_outbox.sql.
const outboxSchema = `
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

// stubSink records what was Append'd and lets the test control
// success/failure + assign control_version sequentially.
type stubSink struct {
	mu    sync.Mutex
	count uint64
	fail  error // when non-nil, Append returns this error on the next call
	failN int   // when > 0, the next failN calls fail
	got   []controlevent.Event
}

func (s *stubSink) Name() string { return "stub" }

func (s *stubSink) Append(_ context.Context, e controlevent.Event) (controlevent.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failN > 0 {
		s.failN--
		if s.fail != nil {
			return e, s.fail
		}
		return e, errors.New("stub: configured to fail")
	}
	s.count++
	e.ControlVersion = s.count
	s.got = append(s.got, e)
	return e, nil
}

func newPumpHarness(t *testing.T) (*Controller, *sql.DB, *stubSink) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "runtime.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(outboxSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	conf := config.Config{
		FeedSink:          "stub",
		FeedSinkBatchSize: 64,
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), RuntimeDB: db}
	sink := &stubSink{}
	c := NewController(context.Background(), pu, sink)
	return c, db, sink
}

// queueOutbox inserts a row directly, bypassing tx semantics — fine
// for a pump test that's not exercising the handler-side helper.
func queueOutbox(t *testing.T, db *sql.DB, eventID, eventType string) {
	t.Helper()
	ev := controlevent.Event{EventID: eventID, Type: eventType}
	payload, _ := json.Marshal(ev)
	_, err := db.Exec(`INSERT INTO control_events_outbox
		(event_id, event_type, payload_json, created_at)
		VALUES (?, ?, ?, ?)`,
		eventID, eventType, payload,
		time.Now().UTC().Format(tsLayout))
	if err != nil {
		t.Fatalf("queue outbox: %v", err)
	}
}

func TestPumpDrainsPendingInOrder(t *testing.T) {
	c, db, sink := newPumpHarness(t)
	queueOutbox(t, db, "evt-1", controlevent.TypeTenantCreated)
	queueOutbox(t, db, "evt-2", controlevent.TypeTenantCreated)
	queueOutbox(t, db, "evt-3", controlevent.TypeTenantCreated)

	c.drainOnce(context.Background())

	if len(sink.got) != 3 {
		t.Fatalf("sink got %d events, want 3", len(sink.got))
	}
	for i, want := range []string{"evt-1", "evt-2", "evt-3"} {
		if sink.got[i].EventID != want {
			t.Errorf("got[%d].EventID = %q, want %q", i, sink.got[i].EventID, want)
		}
	}
	// All rows should have published_control_version set.
	rows, _ := db.Query(`SELECT event_id, published_control_version
	                      FROM control_events_outbox
	                      ORDER BY id`)
	defer rows.Close()
	i := 0
	for rows.Next() {
		var eid string
		var cv sql.NullInt64
		_ = rows.Scan(&eid, &cv)
		if !cv.Valid {
			t.Errorf("row %s: published_control_version still NULL", eid)
		}
		if cv.Int64 != int64(i+1) {
			t.Errorf("row %s: published_control_version=%d, want %d", eid, cv.Int64, i+1)
		}
		i++
	}
}

func TestPumpFailureLeavesRowPendingAndRecords(t *testing.T) {
	c, db, sink := newPumpHarness(t)
	queueOutbox(t, db, "evt-fail", controlevent.TypeTenantCreated)

	sink.failN = 1
	sink.fail = errors.New("simulated broker outage")
	c.drainOnce(context.Background())

	var cv sql.NullInt64
	var ac int
	var le sql.NullString
	if err := db.QueryRow(`SELECT published_control_version, attempt_count, last_error
	                        FROM control_events_outbox
	                        WHERE event_id = ?`, "evt-fail").Scan(&cv, &ac, &le); err != nil {
		t.Fatalf("query: %v", err)
	}
	if cv.Valid {
		t.Errorf("expected published_control_version NULL after failure, got %d", cv.Int64)
	}
	if ac != 1 {
		t.Errorf("attempt_count=%d, want 1", ac)
	}
	if !le.Valid || le.String == "" {
		t.Errorf("last_error should be set after failure")
	}

	// Next tick succeeds — same event_id, published normally.
	c.drainOnce(context.Background())
	if err := db.QueryRow(`SELECT published_control_version
	                        FROM control_events_outbox
	                        WHERE event_id = ?`, "evt-fail").Scan(&cv); err != nil {
		t.Fatalf("query 2: %v", err)
	}
	if !cv.Valid {
		t.Errorf("expected published_control_version set after retry success")
	}
}

func TestPumpSkipsAlreadyPublished(t *testing.T) {
	c, db, sink := newPumpHarness(t)
	// Pre-populate as already published.
	ev := controlevent.Event{EventID: "evt-done", Type: controlevent.TypeTenantCreated}
	payload, _ := json.Marshal(ev)
	if _, err := db.Exec(`INSERT INTO control_events_outbox
		(event_id, event_type, payload_json, created_at,
		 published_control_version, published_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"evt-done", controlevent.TypeTenantCreated, payload,
		time.Now().UTC().Format(tsLayout),
		42, time.Now().UTC().Format(tsLayout)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	c.drainOnce(context.Background())
	if len(sink.got) != 0 {
		t.Errorf("pump should not republish already-published rows, got %d", len(sink.got))
	}
}

func TestAppendOutboxHelper(t *testing.T) {
	_, db, _ := newPumpHarness(t)
	ev := controlevent.Event{
		EventID:  "evt-helper",
		Type:     controlevent.TypeStackActivated,
		TenantID: "t_a", StackID: "web", Version: 7,
	}
	payload, _ := json.Marshal(ev)

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := AppendOutbox(context.Background(), tx,
		"evt-helper", controlevent.TypeStackActivated,
		"t_a", "web", 7, 6,
		"artifacts/t_a/web/7.json", "sha256:abc",
		payload, nil,
	); err != nil {
		t.Fatalf("AppendOutbox: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var eventType, tenantID, stackID, artifactRef, checksum string
	var version, baseVersion int64
	if err := db.QueryRow(`SELECT event_type, tenant_id, stack_id, version, base_version,
	                              artifact_ref, checksum
	                        FROM control_events_outbox
	                        WHERE event_id = ?`, "evt-helper").Scan(
		&eventType, &tenantID, &stackID, &version, &baseVersion, &artifactRef, &checksum,
	); err != nil {
		t.Fatalf("query: %v", err)
	}
	if eventType != controlevent.TypeStackActivated || tenantID != "t_a" ||
		stackID != "web" || version != 7 || baseVersion != 6 ||
		artifactRef != "artifacts/t_a/web/7.json" || checksum != "sha256:abc" {
		t.Errorf("unexpected row: type=%s tenant=%s stack=%s v=%d base=%d ref=%s sum=%s",
			eventType, tenantID, stackID, version, baseVersion, artifactRef, checksum)
	}
}

func TestAppendOutboxRejectsEmpty(t *testing.T) {
	_, db, _ := newPumpHarness(t)
	tx, _ := db.Begin()
	defer tx.Rollback()
	if err := AppendOutbox(context.Background(), tx, "", "type", "", "", 0, 0, "", "", []byte(`{}`), nil); err == nil {
		t.Errorf("empty event_id should be rejected")
	}
	if err := AppendOutbox(context.Background(), tx, "evt", "", "", "", 0, 0, "", "", []byte(`{}`), nil); err == nil {
		t.Errorf("empty event_type should be rejected")
	}
	if err := AppendOutbox(context.Background(), tx, "evt", "type", "", "", 0, 0, "", "", nil, nil); err == nil {
		t.Errorf("empty payload_json should be rejected")
	}
}

// TestPumpEnabledGatesOnAdminPersonality proves the C-4 single-producer gate:
// only a chassis with the "admin" personality drains the outbox, so once the
// runtime DB is shared Postgres, non-admin nodes don't double-publish it.
func TestPumpEnabledGatesOnAdminPersonality(t *testing.T) {
	sink := &stubSink{}
	ctrl := func(pers, feedSink string) *Controller {
		pu := &processor.Unit{
			Conf:   config.Config{FeedSink: feedSink, Personalities: pers},
			Logger: zap.NewNop(),
		}
		return NewController(context.Background(), pu, sink)
	}
	cases := []struct {
		pers, feedSink string
		want           bool
	}{
		{"web,admin,dns", "stub", true}, // admin present ⇒ drains
		{"admin", "stub", true},         // admin only ⇒ drains
		{"web,dns", "stub", false},      // no admin ⇒ never drains (shared-PG safety)
		{"web,admin", "nop", false},     // nop feed-sink always disables
		{"web,admin", "", false},        // empty feed-sink disables
	}
	for _, c := range cases {
		if got := ctrl(c.pers, c.feedSink).enabled(); got != c.want {
			t.Errorf("enabled(personalities=%q, feed-sink=%q) = %v, want %v", c.pers, c.feedSink, got, c.want)
		}
	}
}
