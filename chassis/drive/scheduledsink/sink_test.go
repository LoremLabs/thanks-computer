package scheduledsink

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	"github.com/loremlabs/thanks-computer/chassis/drive"
	"github.com/loremlabs/thanks-computer/chassis/drive/filestore"
	"github.com/loremlabs/thanks-computer/chassis/scheduled"
)

// TestSinkLandsScheduledRows drives a real drive store and a real scheduled
// store on the SAME sqlite file (the transactional case) and on separate
// files (the post-commit case), and checks one pending row per mutation,
// keyed drive:<resource_id>:<modseq>, with the nested payload shape.
func TestSinkLandsScheduledRows(t *testing.T) {
	for _, transactional := range []bool{true, false} {
		t.Run(map[bool]string{true: "transactional", false: "post-commit"}[transactional], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			open := func(name string) *sql.DB {
				db, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, name)+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Close() })
				return db
			}
			driveDB := open("shared.db")
			schedDB := driveDB
			if !transactional {
				schedDB = open("sched.db")
			}
			sch := scheduled.NewStore(schedDB, registry.SQLite)
			if err := sch.EnsureSchema(ctx); err != nil {
				t.Fatal(err)
			}
			objects, _ := filestore.New(filepath.Join(dir, "objs"))
			ds := drive.NewStore(driveDB, registry.SQLite, objects)
			if err := ds.EnsureSchema(ctx); err != nil {
				t.Fatal(err)
			}
			clk := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
			ds.SetClock(func() time.Time { return clk })
			ds.SetSink(New(sch, transactional))

			c, _, err := ds.EnsureCollection(ctx, "tnt_a", "pony")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ds.Mkdir(ctx, c.ID, "data"); err != nil { // modseq 1
				t.Fatal(err)
			}
			r, err := ds.Put(ctx, c.ID, "data/a.md", strings.NewReader("# hi"), 4, drive.PutOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ds.Put(ctx, c.ID, "data/a.md", strings.NewReader("# hi"), 4, drive.PutOpts{}); err != nil {
				t.Fatal(err) // noop, no row
			}
			if _, err := ds.Move(ctx, c.ID, "data/a.md", "data/b.md", false); err != nil {
				t.Fatal(err)
			}
			if _, err := ds.Delete(ctx, c.ID, "data/b.md", drive.DeleteOpts{}); err != nil {
				t.Fatal(err)
			}

			var total int
			if err := schedDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM scheduled_events WHERE status = 'pending'`).Scan(&total); err != nil {
				t.Fatal(err)
			}
			if total != 4 { // mkdir + put + move + delete; the noop put adds nothing
				t.Fatalf("pending rows = %d", total)
			}
			rows, err := schedDB.QueryContext(ctx,
				`SELECT tenant, idempotency_key, schedule_at, payload FROM scheduled_events WHERE status = 'pending' AND idempotency_key LIKE ? ORDER BY idempotency_key`,
				"drive:"+r.ResourceID+":%")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var keys, events []string
			for rows.Next() {
				var tenant, key, at, payload string
				if err := rows.Scan(&tenant, &key, &at, &payload); err != nil {
					t.Fatal(err)
				}
				if tenant != "tnt_a" || at != "2026-09-16T12:00:00Z" {
					t.Errorf("row: tenant=%s at=%s", tenant, at)
				}
				keys = append(keys, key)
				events = append(events, gjson.Get(payload, "event").String())
				d := gjson.Get(payload, "drive")
				if d.Get("collection").String() != "pony" || d.Get("collection_id").String() != c.ID || d.Get("resource_id").String() != r.ResourceID || d.Get("tenant").String() != "tnt_a" {
					t.Errorf("payload facts: %s", payload)
				}
				if gjson.Get(payload, "kind").Exists() {
					t.Errorf("top-level kind would shadow a consumer's discriminator: %s", payload)
				}
			}
			// mkdir = 1 (another resource), put = 2, move = 3, delete = 4.
			want := []string{
				"drive:" + r.ResourceID + ":2",
				"drive:" + r.ResourceID + ":3",
				"drive:" + r.ResourceID + ":4",
			}
			if strings.Join(keys, " ") != strings.Join(want, " ") {
				t.Fatalf("keys %v want %v", keys, want)
			}
			if strings.Join(events, " ") != "drive.resource.created drive.resource.moved drive.resource.deleted" {
				t.Fatalf("events %v", events)
			}
			// The moved event carries both paths.
			var payload string
			_ = schedDB.QueryRowContext(ctx, `SELECT payload FROM scheduled_events WHERE idempotency_key = ?`, want[1]).Scan(&payload)
			if gjson.Get(payload, "drive.from_path").String() != "data/a.md" || gjson.Get(payload, "drive.path").String() != "data/b.md" {
				t.Fatalf("moved payload: %s", payload)
			}
		})
	}
}
