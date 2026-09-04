package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// newCalendarDeps builds the op deps over a temp SQLite index with a mirror
// DB that owns exactly the given (hostname → tenant) pairs.
func newCalendarDeps(t *testing.T, owned map[string]string) calendarDeps {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "calendar.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := chcal.NewStore(db, registry.SQLite)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	mirror, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "mirror.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mirror.Close() })
	for _, q := range []string{
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT, revoked_at TEXT)`,
		`CREATE TABLE tenant_hostnames (hostname TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
		`CREATE TABLE dns_zones (origin TEXT, tenant_id TEXT, verified_at TEXT, revoked_at TEXT)`,
	} {
		if _, err := mirror.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for host, slug := range owned {
		if _, err := mirror.Exec(`INSERT OR IGNORE INTO tenants VALUES (?, ?, NULL)`, "t_"+slug, slug); err != nil {
			t.Fatal(err)
		}
		if _, err := mirror.Exec(`INSERT INTO tenant_hostnames VALUES (?, ?, '2026-09-03T00:00:00Z', NULL)`, host, "t_"+slug); err != nil {
			t.Fatal(err)
		}
	}
	fixed := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	return calendarDeps{store: store, snap: func() *sql.DB { return mirror }, dialect: registry.SQLite,
		maxBytes: 1 << 20, prefix: "/dav", now: func() time.Time { return fixed }}
}

func callCal(t *testing.T, fn func(context.Context, calendarDeps, []byte) (event.Payload, error), d calendarDeps, tenant, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := fn(ctx, d, []byte(`{"_txc":{"op":"demo/100/calendar"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func TestCalendarAccountOp(t *testing.T) {
	d := newCalendarDeps(t, map[string]string{"pony.example.com": "acme"})

	out := callCal(t, calendarAccount, d, "acme", `{"username":"Paris@Pony.Example.com","password_style":"words"}`)
	if gjson.Get(out, "_calendar.error").Exists() {
		t.Fatalf("create: %s", out)
	}
	pw := gjson.Get(out, "_calendar.password").String()
	if gjson.Get(out, "_calendar.username").String() != "paris@pony.example.com" || !gjson.Get(out, "_calendar.created").Bool() ||
		strings.Count(pw, "-") != 4 || gjson.Get(out, "_calendar.principal").String() != "/dav/paris@pony.example.com/" {
		t.Errorf("create = %s", out)
	}
	a, ok, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if !ok || a.Tenant != "acme" {
		t.Fatalf("account = %+v ok=%v", a, ok)
	}
	if match, _ := apppass.VerifyPassword(a.PwHash, pw); !match {
		t.Error("generated password does not verify")
	}
	// The shared-credential path: an explicit password (the one IMAP minted).
	out = callCal(t, calendarAccount, d, "acme", `{"username":"paris@pony.example.com","password":"river-galaxy-bamboo-orbit-velvet","into":"_ca"}`)
	if gjson.Get(out, "_ca.error").Exists() || gjson.Get(out, "_ca.password").Exists() || gjson.Get(out, "_ca.created").Bool() {
		t.Errorf("explicit password = %s", out)
	}
	a2, _, _ := d.store.GetAccount(context.Background(), "paris@pony.example.com")
	if match, _ := apppass.VerifyPassword(a2.PwHash, "river-galaxy-bamboo-orbit-velvet"); !match {
		t.Error("explicit password not stored")
	}
	out = callCal(t, calendarAccount, d, "acme", `{"username":"paris@pony.example.com","rotate":true}`)
	if !gjson.Get(out, "_calendar.rotated").Bool() || gjson.Get(out, "_calendar.password").String() == "" {
		t.Errorf("rotate = %s", out)
	}
	for meta, code := range map[string]string{
		`{"username":"x@else.example.com"}`:                          "txco_calendar_domain_not_owned",
		`{"username":"nope"}`:                                        "txco_calendar_invalid_arg",
		`{"username":"x@pony.example.com","password":"short"}`:       "txco_calendar_invalid_arg",
		`{"username":"x@pony.example.com","policy":{"put":"maybe"}}`: "txco_calendar_invalid_arg",
	} {
		if got := gjson.Get(callCal(t, calendarAccount, d, "acme", meta), "_calendar.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}
	if got := gjson.Get(callCal(t, calendarAccount, d, "other", `{"username":"paris@pony.example.com"}`), "_calendar.error.code").String(); got != "txco_calendar_domain_not_owned" {
		t.Errorf("other tenant → %s", got)
	}
	if got := gjson.Get(callCal(t, calendarAccount, d, "", `{"username":"paris@pony.example.com"}`), "_calendar.error.code").String(); got != "txco_calendar_no_tenant" {
		t.Errorf("no tenant → %s", got)
	}
	if got := gjson.Get(callCal(t, calendarAccount, calendarDeps{}, "acme", `{"username":"paris@pony.example.com"}`), "_calendar.error.code").String(); got != "txco_calendar_disabled" {
		t.Errorf("no store → %s", got)
	}
}

func TestCalendarCalendarAndObjectsOps(t *testing.T) {
	d := newCalendarDeps(t, map[string]string{"pony.example.com": "acme"})
	callCal(t, calendarAccount, d, "acme", `{"username":"paris@pony.example.com"}`)

	out := callCal(t, calendarCalendar, d, "acme", `{"username":"paris@pony.example.com","name":"schedule","display_name":"Paris schedule","timezone":"UTC","color":"#6b4de6","policy":{"put":"stack","delete":"stack"},"feed":"ensure"}`)
	if gjson.Get(out, "_calendar.error").Exists() || !gjson.Get(out, "_calendar.created").Bool() ||
		gjson.Get(out, "_calendar.path").String() != "/dav/paris@pony.example.com/calendars/schedule/" ||
		gjson.Get(out, "_calendar.policy.put").String() != "stack" || !gjson.Get(out, "_calendar.feed").Bool() {
		t.Fatalf("ensure = %s", out)
	}
	token := gjson.Get(out, "_calendar.feed_token").String()
	if token == "" || gjson.Get(out, "_calendar.feed_path").String() != "/dav/feed/"+token+".ics" {
		t.Errorf("feed token = %s", out)
	}
	if c, ok, _ := d.store.CalendarByFeedHash(context.Background(), FeedTokenHash(token)); !ok || c.Name != "schedule" {
		t.Error("feed token hash does not resolve")
	}
	// Ensure again: no new token; rotate: a new one; disable: none.
	out = callCal(t, calendarCalendar, d, "acme", `{"username":"paris@pony.example.com","name":"schedule","feed":"ensure"}`)
	if gjson.Get(out, "_calendar.created").Bool() || gjson.Get(out, "_calendar.feed_token").Exists() || gjson.Get(out, "_calendar.display_name").String() != "Paris schedule" {
		t.Errorf("re-ensure = %s", out)
	}
	out = callCal(t, calendarCalendar, d, "acme", `{"username":"paris@pony.example.com","name":"schedule","feed":"rotate"}`)
	if t2 := gjson.Get(out, "_calendar.feed_token").String(); t2 == "" || t2 == token {
		t.Errorf("rotate = %s", out)
	}
	if _, ok, _ := d.store.CalendarByFeedHash(context.Background(), FeedTokenHash(token)); ok {
		t.Error("old feed token still resolves after rotate")
	}
	out = callCal(t, calendarCalendar, d, "acme", `{"username":"paris@pony.example.com","name":"schedule","feed":"disable"}`)
	if gjson.Get(out, "_calendar.feed").Bool() {
		t.Errorf("disable = %s", out)
	}
	for meta, code := range map[string]string{
		`{"username":"paris@pony.example.com","name":"Bad Name"}`:                     "txco_calendar_invalid_arg",
		`{"username":"paris@pony.example.com","name":"x","timezone":"Mars/Olympus"}`:  "txco_calendar_invalid_arg",
		`{"username":"paris@pony.example.com","name":"x","color":"blue"}`:             "txco_calendar_invalid_arg",
		`{"username":"paris@pony.example.com","name":"x","policy":{"fly":"observe"}}`: "txco_calendar_invalid_arg",
		`{"username":"paris@pony.example.com","name":"x","feed":"sometimes"}`:         "txco_calendar_invalid_arg",
		`{"username":"nobody@pony.example.com","name":"x"}`:                           "txco_calendar_no_account",
	} {
		if got := gjson.Get(callCal(t, calendarCalendar, d, "acme", meta), "_calendar.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}

	// put from event{}: uid derived from name.
	out = callCal(t, calendarPut, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","name":"daily-digest.ics","event":{"summary":"Daily digest","description":"What changed?","start":"2026-01-01T09:00:00Z","duration":"PT30M","rrule":"FREQ=DAILY"}}`)
	if gjson.Get(out, "_calendar.error").Exists() || !gjson.Get(out, "_calendar.created").Bool() || gjson.Get(out, "_calendar.noop").Bool() ||
		gjson.Get(out, "_calendar.uid").String() != "daily-digest.paris@pony.example.com" || gjson.Get(out, "_calendar.name").String() != "daily-digest.ics" ||
		gjson.Get(out, "_calendar.path").String() != "/dav/paris@pony.example.com/calendars/schedule/daily-digest.ics" || gjson.Get(out, "_calendar.sequence").Int() != 0 {
		t.Fatalf("put = %s", out)
	}
	etag := gjson.Get(out, "_calendar.etag").String()
	// Same event an hour later ⇒ noop, same etag.
	d.now = func() time.Time { return time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC) }
	out = callCal(t, calendarPut, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","name":"daily-digest.ics","event":{"summary":"Daily digest","description":"What changed?","start":"2026-01-01T09:00:00Z","duration":"PT30M","rrule":"FREQ=DAILY"}}`)
	if !gjson.Get(out, "_calendar.noop").Bool() || gjson.Get(out, "_calendar.etag").String() != etag || gjson.Get(out, "_calendar.created").Bool() {
		t.Errorf("noop put = %s", out)
	}
	// Changed hour, addressed by uid, no name ⇒ same resource, SEQUENCE 1, new etag.
	out = callCal(t, calendarPut, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","uid":"daily-digest.paris@pony.example.com","event":{"summary":"Daily digest","description":"What changed?","start":"2026-01-01T10:00:00Z","duration":"PT30M","rrule":"FREQ=DAILY"}}`)
	if gjson.Get(out, "_calendar.noop").Bool() || gjson.Get(out, "_calendar.etag").String() == etag || gjson.Get(out, "_calendar.sequence").Int() != 1 ||
		gjson.Get(out, "_calendar.name").String() != "daily-digest.ics" || gjson.Get(out, "_calendar.modseq").Int() != 2 {
		t.Errorf("changed put = %s", out)
	}
	// get: bytes + parsed facts.
	out = callCal(t, calendarGet, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","uid":"daily-digest.paris@pony.example.com"}`)
	if !strings.Contains(gjson.Get(out, "_calendar.ical").String(), "DTSTART:20260101T100000Z") || gjson.Get(out, "_calendar.event.start_utc").String() != "2026-01-01T10:00:00Z" ||
		gjson.Get(out, "_calendar.event.recur.freq").String() != "DAILY" || gjson.Get(out, "_calendar.event.sequence").Int() != 1 || !gjson.Get(out, "_calendar.recurs").Bool() {
		t.Errorf("get = %s", out)
	}
	// put from ical text, a client-shaped object; uid disagreement refused.
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\nUID:ABC-123\r\nDTSTAMP:20260904T100000Z\r\nDTSTART;TZID=Europe/Paris:20260905T090000\r\nDTEND;TZID=Europe/Paris:20260905T093000\r\nSUMMARY:Scan\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	meta, _ := sjsonSet(`{"username":"paris@pony.example.com","calendar":"schedule"}`, "ical", raw)
	out = callCal(t, calendarPut, d, "acme", meta)
	if gjson.Get(out, "_calendar.error").Exists() || gjson.Get(out, "_calendar.uid").String() != "ABC-123" || gjson.Get(out, "_calendar.name").String() != "ABC-123.ics" {
		t.Errorf("ical put = %s", out)
	}
	meta, _ = sjsonSet(meta, "uid", "other")
	if got := gjson.Get(callCal(t, calendarPut, d, "acme", meta), "_calendar.error.code").String(); got != "txco_calendar_invalid_arg" {
		t.Errorf("uid disagreement → %s", got)
	}
	for meta, code := range map[string]string{
		`{"username":"paris@pony.example.com","calendar":"schedule","event":{"start":"2026-01-01T09:00:00Z"}}`:      "txco_calendar_invalid_arg", // no uid, no name
		`{"username":"paris@pony.example.com","calendar":"schedule","name":"x.ics","event":{"summary":"no start"}}`: "txco_calendar_invalid_arg",
		`{"username":"paris@pony.example.com","calendar":"schedule","name":"x.ics"}`:                                "txco_calendar_invalid_arg", // neither
		`{"username":"paris@pony.example.com","calendar":"nope","name":"x.ics","event":{"start":"2026-01-01"}}`:     "txco_calendar_no_calendar",
		`{"username":"paris@pony.example.com","calendar":"schedule","name":"../x","event":{"start":"2026-01-01"}}`:  "txco_calendar_invalid_arg",
		`{"username":"paris@pony.example.com","calendar":"schedule","ical":"garbage"}`:                              "txco_calendar_invalid_arg",
	} {
		if got := gjson.Get(callCal(t, calendarPut, d, "acme", meta), "_calendar.error.code").String(); got != code {
			t.Errorf("%s → %s, want %s", meta, got, code)
		}
	}
	// Too large.
	small := d
	small.maxBytes = 100
	if got := gjson.Get(callCal(t, calendarPut, small, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","name":"big.ics","event":{"start":"2026-01-01"}}`), "_calendar.error.code").String(); got != "txco_calendar_too_large" {
		t.Errorf("too large → %s", got)
	}
	// list: calendars, then objects.
	out = callCal(t, calendarList, d, "acme", `{"username":"paris@pony.example.com"}`)
	if gjson.Get(out, "_calendar.count").Int() != 1 || gjson.Get(out, "_calendar.calendars.0.objects").Int() != 2 || gjson.Get(out, "_calendar.home").String() != "/dav/paris@pony.example.com/calendars/" {
		t.Errorf("list calendars = %s", out)
	}
	out = callCal(t, calendarList, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","limit":1}`)
	if gjson.Get(out, "_calendar.count").Int() != 1 || gjson.Get(out, "_calendar.next").Int() != 2 || gjson.Get(out, "_calendar.items.0.name").String() != "daily-digest.ics" {
		t.Errorf("list objects page 1 = %s", out)
	}
	out = callCal(t, calendarList, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","after":2}`)
	if gjson.Get(out, "_calendar.count").Int() != 1 || gjson.Get(out, "_calendar.next").Int() != 0 || gjson.Get(out, "_calendar.items.0.uid").String() != "ABC-123" {
		t.Errorf("list objects page 2 = %s", out)
	}
	// delete by uid, then by name (absent ⇒ deleted false).
	out = callCal(t, calendarDelete, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","uid":"ABC-123"}`)
	if !gjson.Get(out, "_calendar.deleted").Bool() || gjson.Get(out, "_calendar.name").String() != "ABC-123.ics" {
		t.Errorf("delete = %s", out)
	}
	out = callCal(t, calendarDelete, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","name":"ABC-123.ics"}`)
	if gjson.Get(out, "_calendar.deleted").Bool() {
		t.Errorf("second delete = %s", out)
	}
	if got := gjson.Get(callCal(t, calendarGet, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule","name":"ABC-123.ics"}`), "_calendar.error.code").String(); got != "txco_calendar_no_object" {
		t.Errorf("get deleted → %s", got)
	}
	// remove the calendar.
	out = callCal(t, calendarCalendar, d, "acme", `{"username":"paris@pony.example.com","name":"schedule","remove":true}`)
	if !gjson.Get(out, "_calendar.removed").Bool() {
		t.Errorf("remove = %s", out)
	}
	if got := gjson.Get(callCal(t, calendarList, d, "acme", `{"username":"paris@pony.example.com","calendar":"schedule"}`), "_calendar.error.code").String(); got != "txco_calendar_no_calendar" {
		t.Errorf("list removed → %s", got)
	}
}

func TestCalendarPathsAndUIDs(t *testing.T) {
	if got := calendarPath("/dav/", "a@b.c", "x"); got != "/dav/a@b.c/calendars/x/" {
		t.Errorf("calendarPath = %s", got)
	}
	if got := calendarPath("", "a@b.c", "x"); got != "/dav/a@b.c/calendars/x/" {
		t.Errorf("empty prefix = %s", got)
	}
	if got := feedPath("cal", "tok"); got != "/cal/feed/tok.ics" {
		t.Errorf("feedPath = %s", got)
	}
	if got := chcal.DefaultUID("Daily Digest.ics", "paris@pony.example.com"); got != "Daily-Digest.paris@pony.example.com" {
		t.Errorf("deriveUID = %s", got)
	}
	if got := chcal.DefaultObjectName("8F2C/1A34@apple"); got != "8F2C-1A34.ics" {
		t.Errorf("defaultObjectName = %s", got)
	}
}

func sjsonSet(doc, path, value string) (string, error) { return sjson.Set(doc, path, value) }
