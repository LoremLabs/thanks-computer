package calendar

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
)

// newTestStore opens a per-test temp-file SQLite DB with the production
// DSN shape (WAL + busy timeout + immediate write lock), applies the schema
// twice (idempotence), and pins the clock.
func newTestStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "calendar.db")
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	s := NewStore(db, registry.SQLite)
	s.SetClock(func() time.Time { return clk })
	for i := 0; i < 2; i++ {
		if err := s.EnsureSchema(context.Background()); err != nil {
			t.Fatalf("ensure schema #%d: %v", i, err)
		}
	}
	return s, &clk
}

func vevent(uid, summary, dtstamp string) []byte {
	return []byte("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:" + uid +
		"\r\nDTSTAMP:" + dtstamp + "\r\nDTSTART:20260101T090000Z\r\nDURATION:PT30M\r\nRRULE:FREQ=DAILY\r\nSUMMARY:" + summary +
		"\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
}

func mustCal(t *testing.T, s *Store, name string) Calendar {
	t.Helper()
	c, _, err := s.EnsureCalendar(context.Background(), Calendar{Tenant: "acme", Username: "paris@example.com", Name: name, DisplayName: "Schedule"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAccounts(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	created, err := s.UpsertAccount(ctx, "acme", "Paris@Example.COM", "hash1", "", nil)
	if err != nil || !created {
		t.Fatalf("create: created=%v err=%v", created, err)
	}
	a, ok, err := s.GetAccount(ctx, "paris@example.com")
	if err != nil || !ok || a.Tenant != "acme" || a.PwHash != "hash1" || a.Status != StatusActive || string(a.Policy) != "{}" {
		t.Fatalf("account = %+v ok=%v err=%v", a, ok, err)
	}
	// Update keeps the hash when none is given.
	if c, err := s.UpsertAccount(ctx, "acme", "paris@example.com", "", StatusDisabled, nil); err != nil || c {
		t.Fatalf("update: created=%v err=%v", c, err)
	}
	a, _, _ = s.GetAccount(ctx, "paris@example.com")
	if a.PwHash != "hash1" || a.Status != StatusDisabled {
		t.Errorf("after update: %+v", a)
	}
	// Another tenant cannot take the username.
	if _, err := s.UpsertAccount(ctx, "other", "paris@example.com", "h", "", nil); !errors.Is(err, ErrUsernameTaken) {
		t.Errorf("cross-tenant upsert err = %v, want ErrUsernameTaken", err)
	}
	if _, err := s.UpsertAccount(ctx, "acme", "new@example.com", "", "", nil); err == nil {
		t.Error("create without a password must fail")
	}
	if _, err := s.UpsertAccount(ctx, "acme", "x@example.com", "h", "weird", nil); err == nil {
		t.Error("bad status must fail")
	}
}

func TestCalendarsEnsureUpdateRemoveResurrect(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c, created, err := s.EnsureCalendar(ctx, Calendar{Tenant: "acme", Username: "paris@example.com", Name: "schedule", DisplayName: "Paris schedule", Timezone: "UTC", Policy: []byte(`{"put":"stack"}`)})
	if err != nil || !created || c.ID == "" || c.DisplayName != "Paris schedule" || c.SyncToken != 0 {
		t.Fatalf("ensure: %+v created=%v err=%v", c, created, err)
	}
	// Ensure again: not created, empty fields untouched, non-empty applied.
	c2, created, err := s.EnsureCalendar(ctx, Calendar{Tenant: "acme", Username: "paris@example.com", Name: "schedule", Color: "#112233"})
	if err != nil || created || c2.ID != c.ID || c2.DisplayName != "Paris schedule" || c2.Color != "#112233" || string(c2.Policy) != `{"put":"stack"}` {
		t.Fatalf("re-ensure: %+v created=%v err=%v", c2, created, err)
	}
	if _, _, err := s.EnsureCalendar(ctx, Calendar{Tenant: "acme", Username: "paris@example.com", Name: "Bad Name"}); err == nil {
		t.Error("bad calendar name must fail")
	}
	list, err := s.ListCalendars(ctx, "acme", "paris@example.com")
	if err != nil || len(list) != 1 || list[0].Name != "schedule" {
		t.Fatalf("list = %+v err=%v", list, err)
	}
	if err := s.SetFeedToken(ctx, c.ID, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if fc, ok, _ := s.CalendarByFeedHash(ctx, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); !ok || fc.ID != c.ID {
		t.Errorf("feed lookup: ok=%v id=%s", ok, fc.ID)
	}
	if _, ok, _ := s.CalendarByFeedHash(ctx, "nope"); ok {
		t.Error("bad hash must not resolve")
	}
	// An object, then remove: object tombstoned, calendar gone, feed cleared.
	if _, err := s.PutObject(ctx, c.ID, Object{Name: "a.ics", UID: "a@x", ICal: vevent("a@x", "A", "20260904T120000Z")}, PutOpts{}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RemoveCalendar(ctx, c.ID); err != nil || !ok {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := s.GetCalendar(ctx, "acme", "paris@example.com", "schedule"); ok {
		t.Error("removed calendar still live")
	}
	if _, ok, _ := s.CalendarByFeedHash(ctx, "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); ok {
		t.Error("feed token survived removal")
	}
	objs, _ := s.ListObjects(ctx, c.ID, ListOpts{IncludeDeleted: true})
	if len(objs) != 1 || !objs[0].Deleted {
		t.Errorf("objects after remove = %+v", objs)
	}
	// Resurrect: same name, same id, objects stay tombstoned.
	c3, created, err := s.EnsureCalendar(ctx, Calendar{Tenant: "acme", Username: "paris@example.com", Name: "schedule"})
	if err != nil || !created || c3.ID != c.ID || c3.FeedTokenHash != "" {
		t.Fatalf("resurrect: %+v created=%v err=%v", c3, created, err)
	}
	if live, _ := s.ListObjects(ctx, c.ID, ListOpts{}); len(live) != 0 {
		t.Errorf("resurrected calendar has live objects: %+v", live)
	}
}

func TestPutObjectNoopReplaceByUIDAndPreconditions(t *testing.T) {
	s, clk := newTestStore(t)
	ctx := context.Background()
	c := mustCal(t, s, "schedule")

	r1, err := s.PutObject(ctx, c.ID, Object{Name: "digest.ics", UID: "digest@x", Summary: "Digest", Recurs: true, ICal: vevent("digest@x", "Digest", "20260904T120000Z")}, PutOpts{ByUID: true})
	if err != nil || !r1.Created || r1.Noop || r1.ModSeq != 1 || r1.Name != "digest.ics" || r1.ETag == "" {
		t.Fatalf("create: %+v err=%v", r1, err)
	}
	// Same content, new DTSTAMP ⇒ noop, etag and modseq unchanged.
	*clk = clk.Add(time.Hour)
	r2, err := s.PutObject(ctx, c.ID, Object{Name: "ignored.ics", UID: "digest@x", ICal: vevent("digest@x", "Digest", "20260904T130000Z")}, PutOpts{ByUID: true})
	if err != nil || !r2.Noop || r2.ETag != r1.ETag || r2.ModSeq != 1 || r2.Name != "digest.ics" {
		t.Fatalf("noop: %+v err=%v", r2, err)
	}
	if cal, _, _ := s.GetCalendarByID(ctx, c.ID); cal.SyncToken != 1 {
		t.Errorf("sync token moved on a noop: %d", cal.SyncToken)
	}
	// Changed content by uid ⇒ same name, new etag, modseq 2.
	r3, err := s.PutObject(ctx, c.ID, Object{Name: "ignored.ics", UID: "digest@x", Sequence: 1, ICal: vevent("digest@x", "Digest v2", "20260904T130000Z")}, PutOpts{ByUID: true})
	if err != nil || r3.Created || r3.Noop || r3.ETag == r1.ETag || r3.ModSeq != 2 || r3.Name != "digest.ics" || r3.Sequence != 1 {
		t.Fatalf("replace: %+v err=%v", r3, err)
	}
	o, ok, _ := s.GetObject(ctx, c.ID, "digest.ics")
	if !ok || o.Sequence != 1 || o.ETag != r3.ETag || string(o.ICal) != string(vevent("digest@x", "Digest v2", "20260904T130000Z")) {
		t.Errorf("stored object = %+v", o)
	}
	// A client put under a different name with the same uid ⇒ conflict.
	if _, err := s.PutObject(ctx, c.ID, Object{Name: "other.ics", UID: "digest@x", ICal: vevent("digest@x", "X", "20260904T130000Z")}, PutOpts{}); !errors.Is(err, ErrUIDConflict) {
		t.Errorf("uid conflict err = %v", err)
	}
	// Preconditions.
	if _, err := s.PutObject(ctx, c.ID, Object{Name: "digest.ics", UID: "digest@x", ICal: vevent("digest@x", "Y", "20260904T130000Z")}, PutOpts{IfNoneMatch: "*"}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("if-none-match * on existing err = %v", err)
	}
	if _, err := s.PutObject(ctx, c.ID, Object{Name: "digest.ics", UID: "digest@x", ICal: vevent("digest@x", "Y", "20260904T130000Z")}, PutOpts{IfMatch: "stale"}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("if-match stale err = %v", err)
	}
	if _, err := s.PutObject(ctx, c.ID, Object{Name: "new.ics", UID: "new@x", ICal: vevent("new@x", "N", "20260904T130000Z")}, PutOpts{IfMatch: "*"}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("if-match * on missing err = %v", err)
	}
	r4, err := s.PutObject(ctx, c.ID, Object{Name: "digest.ics", UID: "digest@x", ICal: vevent("digest@x", "Y", "20260904T130000Z")}, PutOpts{IfMatch: r3.ETag})
	if err != nil || r4.Noop || r4.ModSeq != 3 {
		t.Fatalf("if-match current: %+v err=%v", r4, err)
	}
	// Delete, tombstone, resurrect under the same name with a new uid.
	etag, found, err := s.DeleteObject(ctx, c.ID, "digest.ics")
	if err != nil || !found || etag != r4.ETag {
		t.Fatalf("delete: etag=%s found=%v err=%v", etag, found, err)
	}
	if _, found, _ := s.DeleteObject(ctx, c.ID, "digest.ics"); found {
		t.Error("second delete found the tombstone")
	}
	if _, ok, _ := s.GetObject(ctx, c.ID, "digest.ics"); ok {
		t.Error("deleted object still live")
	}
	dead, _ := s.ListObjects(ctx, c.ID, ListOpts{IncludeDeleted: true, SinceModSeq: 3})
	if len(dead) != 1 || !dead[0].Deleted || dead[0].ModSeq != 4 {
		t.Errorf("tombstone = %+v", dead)
	}
	r5, err := s.PutObject(ctx, c.ID, Object{Name: "digest.ics", UID: "fresh@x", ICal: vevent("fresh@x", "F", "20260904T130000Z")}, PutOpts{IfNoneMatch: "*"})
	if err != nil || !r5.Created || r5.ModSeq != 5 || r5.UID != "fresh@x" {
		t.Fatalf("resurrect: %+v err=%v", r5, err)
	}
	if o, ok, _ := s.GetObjectByUID(ctx, c.ID, "fresh@x"); !ok || o.Deleted || o.Name != "digest.ics" {
		t.Errorf("resurrected = %+v ok=%v", o, ok)
	}
	if _, err := s.PutObject(ctx, "cal_missing", Object{Name: "a.ics", UID: "a", ICal: []byte("x")}, PutOpts{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing calendar err = %v", err)
	}
	if _, err := s.PutObject(ctx, c.ID, Object{Name: "../x", UID: "a", ICal: []byte("x")}, PutOpts{}); err == nil {
		t.Error("bad resource name accepted")
	}
}

func TestObjectsInRangeAndNames(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	c := mustCal(t, s, "cal")
	put := func(name, uid, start, end string, recurs bool) {
		t.Helper()
		if _, err := s.PutObject(ctx, c.ID, Object{Name: name, UID: uid, DTStartUTC: start, DTEndUTC: end, Recurs: recurs, ICal: vevent(uid, name, "20260904T120000Z")}, PutOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	put("jan.ics", "jan", "2026-01-10T09:00:00Z", "2026-01-10T10:00:00Z", false)
	put("sep.ics", "sep", "2026-09-10T09:00:00Z", "2026-09-10T10:00:00Z", false)
	put("daily.ics", "daily", "2026-01-01T09:00:00Z", "2026-01-01T09:30:00Z", true)
	put("nobounds.ics", "nob", "", "", false)
	got, err := s.ObjectsInRange(ctx, c.ID, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, o := range got {
		names[o.Name] = true
	}
	if names["jan.ics"] || !names["sep.ics"] || !names["daily.ics"] || !names["nobounds.ics"] || len(got) != 3 {
		t.Errorf("range = %v", names)
	}
	all, _ := s.ObjectsInRange(ctx, c.ID, time.Time{}, time.Time{})
	if len(all) != 4 {
		t.Errorf("unbounded range = %d", len(all))
	}
	some, _ := s.ListObjects(ctx, c.ID, ListOpts{Names: []string{"jan.ics", "missing.ics"}})
	if len(some) != 1 || some[0].Name != "jan.ics" {
		t.Errorf("by names = %+v", some)
	}
}

func TestSameContentIgnoresVolatileLines(t *testing.T) {
	a := []byte("BEGIN:VEVENT\r\nDTSTAMP:20260904T120000Z\r\nSEQUENCE:1\r\nSUMMARY:a long summary that is folded\r\n  across lines\r\nEND:VEVENT\r\n")
	b := []byte("BEGIN:VEVENT\r\nDTSTAMP:20260904T130000Z\r\nSEQUENCE:2\r\nSUMMARY:a long summary that is folded across lines\r\nEND:VEVENT\r\n")
	if !SameContent(a, b) {
		t.Error("DTSTAMP/SEQUENCE/folding must not count")
	}
	if SameContent(a, []byte("BEGIN:VEVENT\r\nSUMMARY:other\r\nEND:VEVENT\r\n")) {
		t.Error("different content reported same")
	}
}
