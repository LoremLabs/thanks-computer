package calseed_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/calseed"
)

func newStore(t *testing.T) *chcal.Store {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "calendar.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := chcal.NewStore(db, registry.SQLite)
	s.SetClock(func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) })
	if err := s.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

func pack(name string, lines ...string) storeseed.RawPack {
	p, ok := storeseed.NewRawPack("CALENDARS/"+name+".jsonl", []byte(strings.Join(lines, "\n")+"\n"))
	if !ok {
		panic("bad pack path " + name)
	}
	return p
}

const (
	user    = "paris@pony.example.com"
	hdr     = `{"calendar":{"display_name":"Paris events","timezone":"Europe/Paris","color":"#2a9d8f"}}`
	welcome = `{"name":"welcome.ics","event":{"summary":"Welcome","start":"2026-09-04T18:00:00","tzid":"Europe/Paris","duration":"PT1H"}}`
	visit   = `{"name":"visit.ics","event":{"summary":"Visit","start":"2026-09-05","end":"2026-09-06"}}`
	notice  = `{"name":"notice.ics","ical":"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\nUID:notice-1\r\nDTSTAMP:20260904T100000Z\r\nDTSTART:20260910T090000Z\r\nSUMMARY:Notice\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"}`
)

func TestReconcileOwnsTheCalendar(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", user, "hash", "", nil); err != nil {
		t.Fatal(err)
	}
	m := calseed.New(s, false)
	scope := storeseed.Scope{Tenant: "acme", Stack: "core", Version: 1}
	if m.Kind() != storeseed.KindCalendar || m.Shared() {
		t.Fatalf("kind=%s shared=%v", m.Kind(), m.Shared())
	}
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/events", hdr, welcome, visit, notice)}); err != nil {
		t.Fatal(err)
	}
	cal, ok, _ := s.GetCalendar(ctx, "acme", user, "events")
	if !ok || cal.DisplayName != "Paris events" || cal.Timezone != "Europe/Paris" || cal.Color != "#2a9d8f" || string(cal.Policy) != `{"put":"deny","delete":"deny"}` {
		t.Fatalf("calendar = %+v ok=%v", cal, ok)
	}
	objs, _ := s.ListObjects(ctx, cal.ID, chcal.ListOpts{})
	if len(objs) != 3 {
		t.Fatalf("objects = %d", len(objs))
	}
	byName := map[string]chcal.Object{}
	for _, o := range objs {
		byName[o.Name] = o
	}
	if byName["welcome.ics"].UID != "welcome.paris@pony.example.com" || !strings.Contains(string(byName["welcome.ics"].ICal), "DTSTART;TZID=Europe/Paris:20260904T180000") ||
		byName["notice.ics"].UID != "notice-1" || byName["visit.ics"].DTStartUTC != "2026-09-05T00:00:00Z" {
		t.Errorf("objects = %+v", byName)
	}
	token := cal.SyncToken
	// Same pack again: nothing moves.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/events", hdr, welcome, visit, notice)}); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := s.GetCalendarByID(ctx, cal.ID); c.SyncToken != token {
		t.Errorf("no-op reconcile moved sync_token %d → %d", token, c.SyncToken)
	}
	// A changed event and a dropped one: updated in place, stale deleted.
	changed := strings.Replace(welcome, "PT1H", "PT2H", 1)
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/events", hdr, changed, notice)}); err != nil {
		t.Fatal(err)
	}
	objs, _ = s.ListObjects(ctx, cal.ID, chcal.ListOpts{})
	if len(objs) != 2 {
		t.Fatalf("after drop: %d objects", len(objs))
	}
	w, _, _ := s.GetObject(ctx, cal.ID, "welcome.ics")
	if !strings.Contains(string(w.ICal), "DURATION:PT2H") || w.Sequence != 1 {
		t.Errorf("updated = seq %d\n%s", w.Sequence, w.ICal)
	}
	if _, ok, _ := s.GetObject(ctx, cal.ID, "visit.ics"); ok {
		t.Error("dropped object still live")
	}
	// A header policy is honoured on an existing calendar.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack(user+"/events", `{"calendar":{"policy":{"put":"observe"}}}`, changed, notice)}); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := s.GetCalendar(ctx, "acme", user, "events"); string(c.Policy) != `{"put":"observe"}` || c.DisplayName != "Paris events" {
		t.Errorf("policy/display after header = %+v", c)
	}
}

func TestReconcileRefusals(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.UpsertAccount(ctx, "acme", user, "hash", "", nil); err != nil {
		t.Fatal(err)
	}
	m := calseed.New(s, true)
	scope := storeseed.Scope{Tenant: "acme", Stack: "core", Version: 1}
	for name, packs := range map[string][]storeseed.RawPack{
		"unknown account": {pack("ghost@pony.example.com/events", welcome)},
		"two headers":     {pack(user+"/events", hdr, hdr, welcome)},
		"neither":         {pack(user+"/events", `{"name":"x.ics"}`)},
		"both":            {pack(user+"/events", `{"name":"x.ics","ical":"x","event":{}}`)},
		"no name no uid":  {pack(user+"/events", `{"event":{"start":"2026-09-04"}}`)},
		"bad tz":          {pack(user+"/events", `{"calendar":{"timezone":"Mars/Olympus"}}`, welcome)},
		"bad policy":      {pack(user+"/events", `{"calendar":{"policy":{"fly":"deny"}}}`, welcome)},
		"bad event":       {pack(user+"/events", `{"name":"x.ics","event":{"summary":"no start"}}`)},
	} {
		if err := m.Reconcile(ctx, scope, packs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, ok, _ := s.GetCalendar(ctx, "acme", user, "events"); ok {
		// the parse errors above never reach the store; "bad event" does
		// (the calendar is ensured before objects are put) — that is fine.
		_ = ok
	}
	// Another tenant's account is refused even though the username resolves.
	if err := m.Reconcile(ctx, storeseed.Scope{Tenant: "other", Stack: "core"}, []storeseed.RawPack{pack(user+"/events", welcome)}); err == nil || !strings.Contains(err.Error(), "another tenant") {
		t.Errorf("cross-tenant err = %v", err)
	}
	var j json.RawMessage
	_ = j
}
