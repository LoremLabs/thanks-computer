package controlapply

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
	_ "github.com/loremlabs/thanks-computer/chassis/artifact/filestore"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/feed"
	_ "github.com/loremlabs/thanks-computer/chassis/feed/filesource"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/admin"
)

const schema = `
CREATE TABLE varvals (var TEXT, val TEXT, UNIQUE(var));
CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT);
CREATE TABLE stacks (stack_id TEXT PRIMARY KEY, tenant_id TEXT, name TEXT, active_version INTEGER, created_at TEXT, UNIQUE(tenant_id,name));
CREATE TABLE stack_versions (version_id INTEGER PRIMARY KEY, stack_id TEXT, version_number INTEGER, parent_version_id INTEGER, status TEXT, created_by TEXT, created_at TEXT, activated_at TEXT, manifest_hash TEXT, UNIQUE(stack_id,version_number));
CREATE TABLE stack_files (version_id INTEGER, path TEXT, content TEXT, content_hash TEXT, PRIMARY KEY(version_id,path));
CREATE TABLE ops (tenant_id TEXT, stack TEXT, scope INTEGER, name TEXT, txcl TEXT, mock_req TEXT, mock_res TEXT);
CREATE TABLE applied_events (event_id TEXT PRIMARY KEY, control_version INTEGER NOT NULL, applied_at TEXT NOT NULL);
CREATE TABLE dns_zones (id TEXT PRIMARY KEY, tenant_id TEXT, origin TEXT, mname TEXT, rname TEXT, refresh INTEGER, retry INTEGER, expire INTEGER, minimum INTEGER, default_ttl INTEGER, mode TEXT, created_at TEXT, created_by TEXT, updated_at TEXT, revoked_at TEXT, verified_at TEXT);
CREATE TABLE cron_settings (tenant_id TEXT PRIMARY KEY, timezone TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL, updated_by TEXT);
INSERT INTO tenants VALUES ('tnt_a','a');
`

type harness struct {
	c       *Controller
	db      *sql.DB
	feedDir string
	astore  artifact.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "runtime.db")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	feedDir := filepath.Join(dir, "feed")
	artDir := filepath.Join(dir, "art")
	conf := config.Config{
		DbRuntimeDsn:      "file:" + dbPath,
		FeedSource:        "file",
		FeedSourceFileDir: feedDir,
		FeedPollPeriod:    1,
	}
	dbc, err := dbcache.New(conf, zap.NewNop(), context.Background(), db)
	if err != nil {
		t.Fatalf("dbcache: %v", err)
	}
	if err := dbc.Reload(); err != nil {
		t.Fatalf("dbcache reload: %v", err)
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), RuntimeDB: db, Dbc: dbc}
	astore, err := artifact.Open("file", artifact.StoreConfig{FileDir: artDir})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	src, err := feed.Open("file", feed.SourceConfig{FileDir: feedDir})
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	adminCtrl := admin.NewController(context.Background(), pu)
	return &harness{
		c:       NewController(context.Background(), pu, adminCtrl, src, astore),
		db:      db,
		feedDir: feedDir,
		astore:  astore,
	}
}

func (h *harness) putEvent(t *testing.T, name string, ev controlevent.Event) {
	t.Helper()
	b, _ := json.Marshal(ev)
	if err := os.WriteFile(filepath.Join(h.feedDir, name), b, 0o644); err != nil {
		t.Fatalf("write event: %v", err)
	}
}

func (h *harness) cursor(t *testing.T) uint64 {
	t.Helper()
	cv, err := h.c.readCursor(context.Background())
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	return cv
}

func TestStackActivatedEndToEndAndIdempotent(t *testing.T) {
	h := newHarness(t)
	art := StackActivatedArtifact{
		TenantID: "tnt_a", Stack: "web", Version: 1,
		Files: []StackArtifactFile{{Path: "100/hello.txcl", Content: `EXEC "https://example.test/x"`}},
	}
	data, _ := json.Marshal(art)
	if err := h.astore.Put(context.Background(), "stacks/tnt_a/web/1", data, []byte(`{}`)); err != nil {
		t.Fatalf("put artifact: %v", err)
	}
	h.putEvent(t, "e1.json", controlevent.Event{
		EventID: "evt-stack-1",
		Type:    controlevent.TypeStackActivated, TenantID: "tnt_a", StackID: "web",
		Version: 1, ArtifactRef: "stacks/tnt_a/web/1",
		Checksum: "sha256:" + sha256Hex(data), ControlVersion: 5,
	})

	h.c.pollOnce(context.Background())

	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM ops WHERE tenant_id='tnt_a' AND stack='web' AND scope=100 AND name='hello'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("ops not materialised: n=%d err=%v", n, err)
	}
	var av sql.NullInt64
	_ = h.db.QueryRow(`SELECT active_version FROM stacks WHERE tenant_id='tnt_a' AND name='web'`).Scan(&av)
	if !av.Valid {
		t.Fatalf("active_version not set")
	}
	if h.cursor(t) != 5 {
		t.Fatalf("cursor = %d, want 5", h.cursor(t))
	}

	// Re-poll: event is <= cursor → idempotent no-op, cursor unchanged.
	h.c.pollOnce(context.Background())
	if h.cursor(t) != 5 {
		t.Fatalf("cursor moved on idempotent re-poll: %d", h.cursor(t))
	}
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM ops WHERE tenant_id='tnt_a' AND stack='web'`).Scan(&n)
	if n != 1 {
		t.Fatalf("ops duplicated on re-poll: %d", n)
	}
}

func TestGenericRowUpsert(t *testing.T) {
	h := newHarness(t)
	rows := RowsArtifact{DB: "runtime", Table: "tenants", Op: "upsert",
		Rows: []map[string]any{{"tenant_id": "tnt_b", "slug": "bee"}}}
	data, _ := json.Marshal(rows)
	_ = h.astore.Put(context.Background(), "rows/1", data, []byte(`{}`))
	h.putEvent(t, "e1.json", controlevent.Event{
		EventID: "evt-rows-1",
		Type:    controlevent.TypeTenantCreated, ArtifactRef: "rows/1",
		Checksum: "sha256:" + sha256Hex(data), ControlVersion: 7,
	})
	h.c.pollOnce(context.Background())

	var slug string
	if err := h.db.QueryRow(`SELECT slug FROM tenants WHERE tenant_id='tnt_b'`).Scan(&slug); err != nil || slug != "bee" {
		t.Fatalf("row not upserted: slug=%q err=%v", slug, err)
	}
	if h.cursor(t) != 7 {
		t.Fatalf("cursor=%d want 7", h.cursor(t))
	}
}

// TestDNSZoneUpsertLandsOnNode proves the long-term fleet-sync fix: a data-plane
// node applying a dns.zone.upserted event gains the delegated-zone row, so it can
// re-derive routing hosts (and, with the dns head, serve the zone) instead of
// relying on the admin node having shipped each hostname row.
func TestDNSZoneUpsertLandsOnNode(t *testing.T) {
	h := newHarness(t)
	rows := RowsArtifact{DB: "runtime", Table: "dns_zones", Op: "upsert",
		Rows: []map[string]any{{
			"id": "dnz_1", "tenant_id": "tnt_a", "origin": "ops.example.com",
			"mname": "ns1.thanks.computer", "rname": "hostmaster.ops.example.com",
			"refresh": 7200, "retry": 3600, "expire": 1209600, "minimum": 90,
			"default_ttl": 60, "mode": "pattern",
			"created_at": "2026-06-04T00:00:00Z", "updated_at": "2026-06-04T00:00:00Z",
		}}}
	data, _ := json.Marshal(rows)
	_ = h.astore.Put(context.Background(), "rows/dns_zones/dnz_1", data, []byte(`{}`))
	h.putEvent(t, "e1.json", controlevent.Event{
		EventID: "evt-dnszone-1",
		Type:    controlevent.TypeDNSZoneUpserted, ArtifactRef: "rows/dns_zones/dnz_1",
		Checksum: "sha256:" + sha256Hex(data), ControlVersion: 3,
	})
	h.c.pollOnce(context.Background())

	var origin, mode string
	if err := h.db.QueryRow(`SELECT origin, mode FROM dns_zones WHERE id='dnz_1'`).Scan(&origin, &mode); err != nil {
		t.Fatalf("dns_zones row not applied on node: %v", err)
	}
	if origin != "ops.example.com" || mode != "pattern" {
		t.Fatalf("zone applied wrong: origin=%q mode=%q", origin, mode)
	}
	if h.cursor(t) != 3 {
		t.Fatalf("cursor=%d want 3", h.cursor(t))
	}

	// Revoke arrives as an upsert with revoked_at set → row flips inactive.
	rev := RowsArtifact{DB: "runtime", Table: "dns_zones", Op: "upsert",
		Rows: []map[string]any{{
			"id": "dnz_1", "tenant_id": "tnt_a", "origin": "ops.example.com",
			"mname": "ns1.thanks.computer", "rname": "hostmaster.ops.example.com",
			"refresh": 7200, "retry": 3600, "expire": 1209600, "minimum": 90,
			"default_ttl": 60, "mode": "pattern",
			"created_at": "2026-06-04T00:00:00Z", "updated_at": "2026-06-04T01:00:00Z",
			"revoked_at": "2026-06-04T01:00:00Z",
		}}}
	rdata, _ := json.Marshal(rev)
	_ = h.astore.Put(context.Background(), "rows/dns_zones/dnz_1-rev", rdata, []byte(`{}`))
	h.putEvent(t, "e2.json", controlevent.Event{
		EventID: "evt-dnszone-2",
		Type:    controlevent.TypeDNSZoneUpserted, ArtifactRef: "rows/dns_zones/dnz_1-rev",
		Checksum: "sha256:" + sha256Hex(rdata), ControlVersion: 4,
	})
	h.c.pollOnce(context.Background())
	var revoked string
	if err := h.db.QueryRow(`SELECT COALESCE(revoked_at,'') FROM dns_zones WHERE id='dnz_1'`).Scan(&revoked); err != nil {
		t.Fatalf("read revoked: %v", err)
	}
	if revoked == "" {
		t.Fatal("zone not revoked on node after revoke event")
	}
}

func TestCronSettingsUpsertLandsOnNode(t *testing.T) {
	h := newHarness(t)
	rows := RowsArtifact{DB: "runtime", Table: "cron_settings", Op: "upsert",
		Rows: []map[string]any{{
			"tenant_id": "tnt_a", "timezone": "Asia/Tokyo",
			"updated_at": "2026-06-04T00:00:00Z", "updated_by": "act_1",
		}}}
	data, _ := json.Marshal(rows)
	_ = h.astore.Put(context.Background(), "rows/cron_settings/tnt_a", data, []byte(`{}`))
	h.putEvent(t, "e1.json", controlevent.Event{
		EventID: "evt-cron-1",
		Type:    controlevent.TypeCronSettingsUpserted, ArtifactRef: "rows/cron_settings/tnt_a",
		Checksum: "sha256:" + sha256Hex(data), ControlVersion: 5,
	})
	h.c.pollOnce(context.Background())

	var tz string
	if err := h.db.QueryRow(`SELECT timezone FROM cron_settings WHERE tenant_id='tnt_a'`).Scan(&tz); err != nil {
		t.Fatalf("cron_settings row not applied on node: %v", err)
	}
	if tz != "Asia/Tokyo" {
		t.Fatalf("cron timezone applied wrong: %q", tz)
	}
	if h.cursor(t) != 5 {
		t.Fatalf("cursor=%d want 5", h.cursor(t))
	}

	// Clearing the timezone arrives as an upsert with timezone='' → back to UTC.
	clr := RowsArtifact{DB: "runtime", Table: "cron_settings", Op: "upsert",
		Rows: []map[string]any{{
			"tenant_id": "tnt_a", "timezone": "",
			"updated_at": "2026-06-04T01:00:00Z",
		}}}
	cdata, _ := json.Marshal(clr)
	_ = h.astore.Put(context.Background(), "rows/cron_settings/tnt_a-clr", cdata, []byte(`{}`))
	h.putEvent(t, "e2.json", controlevent.Event{
		EventID: "evt-cron-2",
		Type:    controlevent.TypeCronSettingsUpserted, ArtifactRef: "rows/cron_settings/tnt_a-clr",
		Checksum: "sha256:" + sha256Hex(cdata), ControlVersion: 6,
	})
	h.c.pollOnce(context.Background())
	if err := h.db.QueryRow(`SELECT timezone FROM cron_settings WHERE tenant_id='tnt_a'`).Scan(&tz); err != nil {
		t.Fatalf("read cleared tz: %v", err)
	}
	if tz != "" {
		t.Fatalf("timezone not cleared on node: %q", tz)
	}
}

func TestAuthEventNoAuthDBIsNoOpButAdvances(t *testing.T) {
	h := newHarness(t) // pu.AuthDB is nil
	rows := RowsArtifact{DB: "auth", Table: "actors", Op: "upsert",
		Rows: []map[string]any{{"actor_id": "act_1", "label": "x"}}}
	data, _ := json.Marshal(rows)
	_ = h.astore.Put(context.Background(), "rows/auth", data, []byte(`{}`))
	h.putEvent(t, "e1.json", controlevent.Event{
		EventID: "evt-auth-1",
		Type:    controlevent.TypeActorChanged, ArtifactRef: "rows/auth",
		Checksum: "sha256:" + sha256Hex(data), ControlVersion: 9,
	})
	h.c.pollOnce(context.Background())
	if h.cursor(t) != 9 {
		t.Fatalf("auth no-op must still advance cursor; got %d", h.cursor(t))
	}
}

func TestSystemOpstackIsNoOpButAdvances(t *testing.T) {
	h := newHarness(t)
	h.putEvent(t, "e1.json", controlevent.Event{
		EventID: "evt-sys-1",
		Type:    controlevent.TypeSystemOpstack, ControlVersion: 11,
	})
	h.c.pollOnce(context.Background())
	if h.cursor(t) != 11 {
		t.Fatalf("system.opstack must advance cursor; got %d", h.cursor(t))
	}
}

// TestAppliedEventsDedupsBeyondCursor exercises the load-bearing
// semantic-dedup guard. A redelivery whose ControlVersion is
// strictly greater than the cursor (so the cursor check doesn't
// catch it) but whose event_id already appears in applied_events
// must NOT re-apply — it just advances the cursor and moves on.
//
// Scenario: rid event_id=evt-X delivered at CV=20 and applied.
// Cursor lands at 20. Then a re-publish of the SAME event_id
// arrives at CV=21 (broker dedup expired, fresh sequence). The
// data row should NOT be re-upserted, but the cursor should
// advance to 21 (and applied_events should not duplicate).
func TestAppliedEventsDedupsBeyondCursor(t *testing.T) {
	h := newHarness(t)
	rows := RowsArtifact{DB: "runtime", Table: "tenants", Op: "upsert",
		Rows: []map[string]any{{"tenant_id": "tnt_dedup", "slug": "first"}}}
	data, _ := json.Marshal(rows)
	_ = h.astore.Put(context.Background(), "rows/dedup", data, []byte(`{}`))

	h.putEvent(t, "first.json", controlevent.Event{
		EventID: "evt-dedup-X",
		Type:    controlevent.TypeTenantCreated, ArtifactRef: "rows/dedup",
		Checksum: "sha256:" + sha256Hex(data), ControlVersion: 20,
	})
	h.c.pollOnce(context.Background())
	if h.cursor(t) != 20 {
		t.Fatalf("first apply: cursor=%d want 20", h.cursor(t))
	}

	// Simulate a republish AFTER dedup window expired: same EventID,
	// fresh ControlVersion. Different artifact bytes to PROVE the
	// applier didn't re-fetch + re-apply (would clobber slug=first
	// with slug=clobbered if it ran).
	rowsClobber := RowsArtifact{DB: "runtime", Table: "tenants", Op: "upsert",
		Rows: []map[string]any{{"tenant_id": "tnt_dedup", "slug": "clobbered"}}}
	dataClobber, _ := json.Marshal(rowsClobber)
	_ = h.astore.Put(context.Background(), "rows/dedup-clobber", dataClobber, []byte(`{}`))

	h.putEvent(t, "second.json", controlevent.Event{
		EventID: "evt-dedup-X", // SAME event_id
		Type:    controlevent.TypeTenantCreated, ArtifactRef: "rows/dedup-clobber",
		Checksum: "sha256:" + sha256Hex(dataClobber), ControlVersion: 21,
	})
	h.c.pollOnce(context.Background())

	if h.cursor(t) != 21 {
		t.Fatalf("dedup should still advance cursor; got %d, want 21", h.cursor(t))
	}
	var slug string
	if err := h.db.QueryRow(`SELECT slug FROM tenants WHERE tenant_id='tnt_dedup'`).Scan(&slug); err != nil {
		t.Fatalf("query: %v", err)
	}
	if slug != "first" {
		t.Errorf("dedup failed: row was re-applied (slug=%q, want %q)", slug, "first")
	}

	// applied_events has exactly one row for this event_id.
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM applied_events WHERE event_id = ?`, "evt-dedup-X").Scan(&n); err != nil {
		t.Fatalf("query applied_events: %v", err)
	}
	if n != 1 {
		t.Errorf("applied_events should have exactly 1 row for evt-dedup-X, got %d", n)
	}
}

func TestChecksumMismatchHaltsAndDoesNotAdvance(t *testing.T) {
	h := newHarness(t)
	rows := RowsArtifact{DB: "runtime", Table: "tenants", Op: "upsert",
		Rows: []map[string]any{{"tenant_id": "tnt_c", "slug": "see"}}}
	data, _ := json.Marshal(rows)
	_ = h.astore.Put(context.Background(), "rows/c", data, []byte(`{}`))
	h.putEvent(t, "e1.json", controlevent.Event{
		EventID: "evt-bad-1",
		Type:    controlevent.TypeTenantCreated, ArtifactRef: "rows/c",
		Checksum: "sha256:deadbeef", ControlVersion: 13,
	})
	h.c.pollOnce(context.Background())

	if h.cursor(t) != 0 {
		t.Fatalf("cursor must not advance on checksum mismatch; got %d", h.cursor(t))
	}
	var n int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM tenants WHERE tenant_id='tnt_c'`).Scan(&n)
	if n != 0 {
		t.Fatalf("no partial state allowed on mismatch; got %d", n)
	}
}
