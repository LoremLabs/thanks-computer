package calendar

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/apppass"
	"github.com/loremlabs/thanks-computer/chassis/auth/registry"
	chcal "github.com/loremlabs/thanks-computer/chassis/calendar"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

type fakeResolver map[string]string

func (f fakeResolver) ResolveErr(k ingress.RouteKey) (ingress.RouteTarget, bool, error) {
	t, ok := f[k.Hostname]
	return ingress.RouteTarget{Tenant: t, Stack: "core", Verified: true}, ok, nil
}

type fakeAdmission struct{ suspended string }

func (f fakeAdmission) Decide(tenant string) admission.Decision {
	if tenant == f.suspended {
		return admission.Decision{Admit: false, Status: 402, Reason: "suspended", Retry: 30 * time.Second}
	}
	return admission.Decision{Admit: true}
}
func (fakeAdmission) AllowRate(string) (bool, time.Duration)           { return true, 0 }
func (fakeAdmission) AcquireConcurrency(string, *admission.Lease) bool { return true }

type fakeStack struct {
	mu      sync.Mutex
	seen    []string
	respond func(raw string) string
}

func (f *fakeStack) serve(bus <-chan *event.Envelope) {
	go func() {
		for env := range bus {
			if env == nil {
				return
			}
			raw := env.Payload.Raw
			f.mu.Lock()
			f.seen = append(f.seen, raw)
			f.mu.Unlock()
			out := "{}"
			if f.respond != nil {
				out = f.respond(raw)
			}
			go func(env *event.Envelope, out string) { env.ResCh <- event.Payload{Raw: out, Type: event.JSON} }(env, out)
		}
	}()
}

func (f *fakeStack) wait(t *testing.T, n int) []string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		got := append([]string{}, f.seen...)
		f.mu.Unlock()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("saw %d envelopes, want %d", len(got), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type harness struct {
	ctrl  *Controller
	store *chcal.Store
	srv   *httptest.Server
}

func newHarness(t *testing.T, conf config.Config) *harness {
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
	conf.Personalities = "web,calendar"
	if conf.CalendarPathPrefix == "" {
		conf.CalendarPathPrefix = "/dav"
	}
	if conf.CalendarLoginRate == 0 {
		conf.CalendarLoginRate = 100
	}
	if conf.CalendarObserveSample == 0 {
		conf.CalendarObserveSample = 1
	}
	if conf.CalendarRespTimeout == "" {
		conf.CalendarRespTimeout = "30s"
	}
	if conf.CalendarObjectMaxBytes == 0 {
		conf.CalendarObjectMaxBytes = 1 << 20
	}
	conf.CalendarFeedMaxAge = 300
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Admission: fakeAdmission{suspended: "suspended"}}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, store, fakeResolver{"pony.example.com": "acme", "other.example.com": "other", "sad.example.com": "suspended"})
	ctrl.now = func() time.Time { return time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC) }
	ctrl.Start()
	srv := httptest.NewServer(ctrl.Handler())
	t.Cleanup(func() { srv.Close(); ctrl.Stop(); cancel() })
	return &harness{ctrl: ctrl, store: store, srv: srv}
}

func (h *harness) account(t *testing.T, tenant, username, password string) {
	t.Helper()
	hash, _ := apppass.HashPassword(password)
	if _, err := h.store.UpsertAccount(context.Background(), tenant, username, hash, "", nil); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) calendar(t *testing.T, tenant, username, name, policy string) chcal.Calendar {
	t.Helper()
	c := chcal.Calendar{Tenant: tenant, Username: username, Name: name, DisplayName: "Schedule"}
	if policy != "" {
		c.Policy = json.RawMessage(policy)
	}
	cal, _, err := h.store.EnsureCalendar(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return cal
}

func (h *harness) withStack(t *testing.T, fs *fakeStack) {
	t.Helper()
	bus := make(chan *event.Envelope, 16)
	h.ctrl.pu.Bus = bus
	h.ctrl.lanes.subscribed = func(tenant string) bool { return tenant == "acme" }
	h.ctrl.lanes.deadline = 500 * time.Millisecond
	fs.serve(bus)
}

type req struct {
	method, path, body, user, pass, host string
	headers                              map[string]string
}

func (h *harness) do(t *testing.T, r req) (*http.Response, string) {
	t.Helper()
	hr, err := http.NewRequest(r.method, h.srv.URL+r.path, strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	hr.Host = "pony.example.com"
	if r.host != "" {
		hr.Host = r.host
	}
	if r.user != "" {
		hr.SetBasicAuth(r.user, r.pass)
	}
	hr.Header.Set("X-Forwarded-Proto", "https")
	if r.body != "" && (r.method == "PROPFIND" || r.method == "REPORT" || r.method == "PROPPATCH" || r.method == "MKCALENDAR") {
		hr.Header.Set("Content-Type", "text/xml; charset=utf-8") // what Apple Calendar sends
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

const (
	user = "paris@pony.example.com"
	pw   = "river-galaxy-bamboo-orbit-velvet"
)

func icsDaily(uid, summary, start, dur string) string {
	return "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Apple Inc.//macOS 15.0//EN\r\nBEGIN:VEVENT\r\nUID:" + uid +
		"\r\nDTSTAMP:20260904T100000Z\r\nDTSTART:" + start + "\r\nDURATION:" + dur + "\r\nRRULE:FREQ=DAILY\r\nSUMMARY:" + summary + "\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
}

func TestAuthAndDiscovery(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	h.account(t, "other", "x@other.example.com", pw)
	h.account(t, "suspended", "y@sad.example.com", pw)
	h.calendar(t, "acme", user, "schedule", "")

	// Unrouted host ⇒ 404 before anything else.
	if resp, _ := h.do(t, req{method: "PROPFIND", path: "/dav/", host: "nobody.example.com", user: user, pass: pw}); resp.StatusCode != 404 {
		t.Errorf("unrouted host = %d", resp.StatusCode)
	}
	// Discovery redirect, unauthenticated.
	resp, _ := h.do(t, req{method: "PROPFIND", path: "/.well-known/caldav"})
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "/dav/" {
		t.Errorf("well-known = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Plaintext ⇒ 403 before credentials are read; insecure flag lifts it.
	hr, _ := http.NewRequest("PROPFIND", h.srv.URL+"/dav/", nil)
	hr.Host = "pony.example.com"
	hr.SetBasicAuth(user, pw)
	if r, _ := http.DefaultClient.Do(hr); r.StatusCode != 403 {
		t.Errorf("plaintext = %d", r.StatusCode)
	}
	// No credentials ⇒ 401 with a challenge.
	resp, _ = h.do(t, req{method: "PROPFIND", path: "/dav/"})
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `Basic realm="calendar"`) {
		t.Errorf("no creds = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	for name, r := range map[string]req{
		"bad password":   {method: "PROPFIND", path: "/dav/", user: user, pass: "nope"},
		"unknown user":   {method: "PROPFIND", path: "/dav/", user: "ghost@pony.example.com", pass: pw},
		"wrong tenant":   {method: "PROPFIND", path: "/dav/", user: "x@other.example.com", pass: pw},
		"other's tree":   {method: "PROPFIND", path: "/dav/x@other.example.com/", user: user, pass: pw},
		"other's cal":    {method: "PROPFIND", path: "/dav/x@other.example.com/calendars/schedule/", user: user, pass: pw},
		"suspended":      {method: "PROPFIND", path: "/dav/", host: "sad.example.com", user: "y@sad.example.com", pass: pw},
		"feed bad token": {method: "GET", path: "/dav/feed/nope.ics"},
	} {
		resp, _ := h.do(t, r)
		want := 401
		switch name {
		case "other's tree", "other's cal":
			want = 403
		case "suspended":
			want = 402
		case "feed bad token":
			want = 404
		}
		if resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", name, resp.StatusCode, want)
		}
	}
	// The discovery a client performs.
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:current-user-principal/></D:prop></D:propfind>`
	resp, out := h.do(t, req{method: "PROPFIND", path: "/dav/", user: user, pass: pw, body: body, headers: map[string]string{"Depth": "0"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "/dav/paris@pony.example.com/") {
		t.Errorf("root propfind = %d\n%s", resp.StatusCode, out)
	}
	body = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><C:calendar-home-set/></D:prop></D:propfind>`
	resp, out = h.do(t, req{method: "PROPFIND", path: "/dav/paris@pony.example.com/", user: user, pass: pw, body: body, headers: map[string]string{"Depth": "0"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "/dav/paris@pony.example.com/calendars/") {
		t.Errorf("principal propfind = %d\n%s", resp.StatusCode, out)
	}
	body = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:displayname/><D:resourcetype/><C:supported-calendar-component-set/></D:prop></D:propfind>`
	resp, out = h.do(t, req{method: "PROPFIND", path: "/dav/paris@pony.example.com/calendars/", user: user, pass: pw, body: body, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "/dav/paris@pony.example.com/calendars/schedule/") || !strings.Contains(out, "Schedule") || !strings.Contains(out, "VEVENT") {
		t.Errorf("home propfind = %d\n%s", resp.StatusCode, out)
	}
	// A verified login is cached: the throttle counts misses only.
	h.ctrl.loginIP = nil
	h.ctrl.loginAcct = nil
}

func TestObjectsQueriesAndFeed(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	cal := h.calendar(t, "acme", user, "schedule", "")
	base := "/dav/paris@pony.example.com/calendars/schedule/"

	// PUT creates: 201 + ETag; the stored bytes are canonical.
	resp, _ := h.do(t, req{method: "PUT", path: base + "digest.ics", user: user, pass: pw, body: icsDaily("A-1", "Digest", "20260101T090000Z", "PT30M"),
		headers: map[string]string{"Content-Type": "text/calendar; charset=utf-8", "If-None-Match": "*"}})
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != 201 || etag == "" {
		t.Fatalf("put = %d etag=%q", resp.StatusCode, etag)
	}
	o, ok, _ := h.store.GetObject(context.Background(), cal.ID, "digest.ics")
	if !ok || `"`+o.ETag+`"` != etag || !o.Recurs || o.DTStartUTC != "2026-01-01T09:00:00Z" || !strings.Contains(string(o.ICal), "BEGIN:VEVENT\r\nDTSTAMP") {
		t.Errorf("stored = %+v ok=%v etag=%s", o, ok, etag)
	}
	// If-None-Match: * on an existing resource ⇒ 412; stale If-Match ⇒ 412; current ⇒ 201.
	resp, _ = h.do(t, req{method: "PUT", path: base + "digest.ics", user: user, pass: pw, body: icsDaily("A-1", "Digest", "20260101T090000Z", "PT30M"),
		headers: map[string]string{"Content-Type": "text/calendar", "If-None-Match": "*"}})
	if resp.StatusCode != 412 {
		t.Errorf("if-none-match on existing = %d", resp.StatusCode)
	}
	resp, _ = h.do(t, req{method: "PUT", path: base + "digest.ics", user: user, pass: pw, body: icsDaily("A-1", "Digest v2", "20260101T090000Z", "PT30M"),
		headers: map[string]string{"Content-Type": "text/calendar", "If-Match": `"stale"`}})
	if resp.StatusCode != 412 {
		t.Errorf("stale if-match = %d", resp.StatusCode)
	}
	resp, _ = h.do(t, req{method: "PUT", path: base + "digest.ics", user: user, pass: pw, body: icsDaily("A-1", "Digest v2", "20260101T090000Z", "PT30M"),
		headers: map[string]string{"Content-Type": "text/calendar", "If-Match": etag}})
	if resp.StatusCode != 201 || resp.Header.Get("ETag") == etag {
		t.Errorf("update = %d etag=%s", resp.StatusCode, resp.Header.Get("ETag"))
	}
	// A second resource reusing the UID ⇒ 409.
	resp, _ = h.do(t, req{method: "PUT", path: base + "dupe.ics", user: user, pass: pw, body: icsDaily("A-1", "Dupe", "20260101T090000Z", "PT30M"),
		headers: map[string]string{"Content-Type": "text/calendar"}})
	if resp.StatusCode != 409 {
		t.Errorf("uid conflict = %d", resp.StatusCode)
	}
	// Garbage ⇒ 400; oversized ⇒ 413.
	resp, _ = h.do(t, req{method: "PUT", path: base + "bad.ics", user: user, pass: pw, body: "nope", headers: map[string]string{"Content-Type": "text/calendar"}})
	if resp.StatusCode != 400 {
		t.Errorf("garbage = %d", resp.StatusCode)
	}
	h.ctrl.maxBytes = 200
	resp, _ = h.do(t, req{method: "PUT", path: base + "big.ics", user: user, pass: pw, body: icsDaily("B-1", strings.Repeat("x", 300), "20260101T090000Z", "PT30M"), headers: map[string]string{"Content-Type": "text/calendar"}})
	if resp.StatusCode != 413 {
		t.Errorf("oversized = %d", resp.StatusCode)
	}
	h.ctrl.maxBytes = 1 << 20
	// A one-off in September.
	one := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//x//EN\r\nBEGIN:VEVENT\r\nUID:S-1\r\nDTSTAMP:20260904T100000Z\r\nDTSTART:20260910T090000Z\r\nDTEND:20260910T100000Z\r\nSUMMARY:One-off\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if resp, _ := h.do(t, req{method: "PUT", path: base + "oneoff.ics", user: user, pass: pw, body: one, headers: map[string]string{"Content-Type": "text/calendar"}}); resp.StatusCode != 201 {
		t.Fatalf("one-off put = %d", resp.StatusCode)
	}
	// GET returns the bytes with the etag.
	resp, out := h.do(t, req{method: "GET", path: base + "oneoff.ics", user: user, pass: pw})
	if resp.StatusCode != 200 || !strings.Contains(out, "SUMMARY:One-off") || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/calendar") {
		t.Errorf("get = %d %s\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), out)
	}
	// calendar-query: a week in October excludes the one-off and the
	// series' first occurrence, yet the daily series must be returned.
	query := `<?xml version="1.0"?><C:calendar-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop>` +
		`<C:filter><C:comp-filter name="VCALENDAR"><C:comp-filter name="VEVENT"><C:time-range start="20261005T000000Z" end="20261012T000000Z"/></C:comp-filter></C:comp-filter></C:filter></C:calendar-query>`
	resp, out = h.do(t, req{method: "REPORT", path: base, user: user, pass: pw, body: query, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "digest.ics") || strings.Contains(out, "oneoff.ics") {
		t.Errorf("time-range query = %d\n%s", resp.StatusCode, out)
	}
	query = strings.Replace(query, `start="20261005T000000Z" end="20261012T000000Z"`, `start="20260909T000000Z" end="20260911T000000Z"`, 1)
	resp, out = h.do(t, req{method: "REPORT", path: base, user: user, pass: pw, body: query, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "digest.ics") || !strings.Contains(out, "oneoff.ics") {
		t.Errorf("september query = %d\n%s", resp.StatusCode, out)
	}
	// multiget by href.
	mg := `<?xml version="1.0"?><C:calendar-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav"><D:prop><D:getetag/><C:calendar-data/></D:prop><D:href>` + base + `oneoff.ics</D:href></C:calendar-multiget>`
	resp, out = h.do(t, req{method: "REPORT", path: base, user: user, pass: pw, body: mg})
	if resp.StatusCode != 207 || !strings.Contains(out, "One-off") {
		t.Errorf("multiget = %d\n%s", resp.StatusCode, out)
	}
	// Depth:1 PROPFIND on the calendar lists both with etags.
	pf := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/></D:prop></D:propfind>`
	resp, out = h.do(t, req{method: "PROPFIND", path: base, user: user, pass: pw, body: pf, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || strings.Count(out, "getetag") < 2 {
		t.Errorf("calendar propfind = %d\n%s", resp.StatusCode, out)
	}
	// DELETE, then a re-PUT of the same name resurrects it.
	if resp, _ := h.do(t, req{method: "DELETE", path: base + "oneoff.ics", user: user, pass: pw}); resp.StatusCode != 204 {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "DELETE", path: base + "oneoff.ics", user: user, pass: pw}); resp.StatusCode != 404 {
		t.Errorf("second delete = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "PUT", path: base + "oneoff.ics", user: user, pass: pw, body: strings.Replace(one, "UID:S-1", "UID:S-2", 1), headers: map[string]string{"Content-Type": "text/calendar", "If-None-Match": "*"}}); resp.StatusCode != 201 {
		t.Errorf("resurrect = %d", resp.StatusCode)
	}
	// The feed: off until a token exists; then 200, 304 on the etag.
	if resp, _ := h.do(t, req{method: "GET", path: "/dav/feed/sometoken.ics"}); resp.StatusCode != 404 {
		t.Errorf("feed without token = %d", resp.StatusCode)
	}
	if err := h.store.SetFeedToken(context.Background(), cal.ID, feedTokenHash("sometoken")); err != nil {
		t.Fatal(err)
	}
	resp, out = h.do(t, req{method: "GET", path: "/dav/feed/sometoken.ics"})
	if resp.StatusCode != 200 || !strings.Contains(out, "X-WR-CALNAME:Schedule") || strings.Count(out, "BEGIN:VEVENT") != 2 || !strings.HasPrefix(resp.Header.Get("Cache-Control"), "private, max-age=300") {
		t.Errorf("feed = %d %s\n%s", resp.StatusCode, resp.Header.Get("Cache-Control"), out)
	}
	if resp, _ := h.do(t, req{method: "GET", path: "/dav/feed/sometoken.ics", headers: map[string]string{"If-None-Match": resp.Header.Get("ETag")}}); resp.StatusCode != 304 {
		t.Errorf("feed 304 = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "GET", path: "/dav/feed/sometoken.ics", host: "other.example.com"}); resp.StatusCode != 404 {
		t.Errorf("feed on another tenant's host = %d", resp.StatusCode)
	}
}

func TestMkcalendarProppatchAndRemove(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	home := "/dav/paris@pony.example.com/calendars/"
	mk := `<?xml version="1.0"?><C:mkcalendar xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:caldav" xmlns:A="http://apple.com/ns/ical/"><D:set><D:prop><D:displayname>Mine</D:displayname><A:calendar-color>#FF0000FF</A:calendar-color><A:calendar-order>3</A:calendar-order><C:calendar-description>d</C:calendar-description></D:prop></D:set></C:mkcalendar>`
	// Default policy denies client-created calendars.
	if resp, _ := h.do(t, req{method: "MKCALENDAR", path: home + "7A3B-UUID/", user: user, pass: pw, body: mk}); resp.StatusCode != 403 {
		t.Errorf("mkcalendar under default policy = %d", resp.StatusCode)
	}
	// An account policy opens it.
	if _, err := h.store.UpsertAccount(context.Background(), "acme", user, "", "", json.RawMessage(`{"mkcalendar":"local","remove":"local"}`)); err != nil {
		t.Fatal(err)
	}
	if resp, body := h.do(t, req{method: "MKCALENDAR", path: home + "7A3B-UUID/", user: user, pass: pw, body: mk}); resp.StatusCode != 201 {
		t.Fatalf("mkcalendar = %d %s", resp.StatusCode, body)
	}
	cal, ok, _ := h.store.GetCalendar(context.Background(), "acme", user, "7A3B-UUID")
	if !ok || cal.DisplayName != "Mine" || cal.Color != "#FF0000FF" || cal.SortOrder != 3 || cal.Description != "d" {
		t.Errorf("created = %+v ok=%v", cal, ok)
	}
	if resp, _ := h.do(t, req{method: "MKCALENDAR", path: home + "7A3B-UUID/", user: user, pass: pw}); resp.StatusCode != 405 {
		t.Errorf("mkcalendar existing = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "MKCALENDAR", path: home + "bad name/", user: user, pass: pw}); resp.StatusCode != 403 {
		t.Errorf("mkcalendar bad name = %d", resp.StatusCode)
	}
	// PROPPATCH: known props 200, unknown 403, in one 207.
	pp := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:X="http://example.com/x"><D:set><D:prop><D:displayname>Renamed</D:displayname><X:foo>bar</X:foo></D:prop></D:set></D:propertyupdate>`
	resp, out := h.do(t, req{method: "PROPPATCH", path: home + "7A3B-UUID/", user: user, pass: pw, body: pp})
	if resp.StatusCode != 207 || !strings.Contains(out, "200 OK") || !strings.Contains(out, "403 Forbidden") {
		t.Errorf("proppatch = %d\n%s", resp.StatusCode, out)
	}
	if c, _, _ := h.store.GetCalendar(context.Background(), "acme", user, "7A3B-UUID"); c.DisplayName != "Renamed" {
		t.Errorf("proppatch did not apply: %+v", c)
	}
	if resp, _ := h.do(t, req{method: "PROPPATCH", path: home + "7A3B-UUID/x.ics", user: user, pass: pw, body: pp}); resp.StatusCode != 403 {
		t.Errorf("proppatch on object = %d", resp.StatusCode)
	}
	// DELETE the calendar (account policy local).
	if resp, _ := h.do(t, req{method: "DELETE", path: home + "7A3B-UUID/", user: user, pass: pw}); resp.StatusCode != 204 {
		t.Errorf("remove = %d", resp.StatusCode)
	}
	if _, ok, _ := h.store.GetCalendar(context.Background(), "acme", user, "7A3B-UUID"); ok {
		t.Error("calendar still live after DELETE")
	}
}

func TestPolicyLanesAndRewrite(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	h.calendar(t, "acme", user, "locked", `{"put":"deny","delete":"deny"}`)
	h.calendar(t, "acme", user, "asked", `{"put":"stack","delete":"stack"}`)
	h.calendar(t, "acme", user, "watched", "")
	fs := &fakeStack{respond: func(raw string) string {
		if gjson.Get(raw, "_txc.calendar.phase").String() != "answer" {
			return "{}"
		}
		summary := gjson.Get(raw, "_txc.calendar.event.summary").String()
		switch {
		case strings.Contains(summary, "weekly"):
			return `{"_txc":{"calendar":{"res":{"ok":false,"code":"cannot","msg":"this pony runs daily"}}}}`
		case strings.Contains(summary, "snap"):
			// Accept with a rewrite: snapped to the hour, canonical shape;
			// the summary passes through so a second edit is a real change.
			return `{"_txc":{"calendar":{"res":{"ok":true,"event":{"summary":"` + summary + `","start":"2026-01-01T09:00:00Z","duration":"PT30M","rrule":"FREQ=DAILY"}}}}}`
		case gjson.Get(raw, "_txc.calendar.op").String() == "delete":
			return `{"_txc":{"calendar":{"res":{"ok":true}}}}`
		}
		return `{"_txc":{"calendar":{"res":{"ok":true}}}}`
	}}
	h.withStack(t, fs)
	base := "/dav/paris@pony.example.com/calendars/"
	ct := map[string]string{"Content-Type": "text/calendar"}

	// deny: 403, nothing dispatched.
	if resp, _ := h.do(t, req{method: "PUT", path: base + "locked/a.ics", user: user, pass: pw, body: icsDaily("L-1", "x", "20260101T090000Z", "PT30M"), headers: ct}); resp.StatusCode != 403 {
		t.Errorf("deny put = %d", resp.StatusCode)
	}
	if len(fs.seen) != 0 {
		t.Fatalf("deny must not dispatch: %v", fs.seen)
	}
	// stack refuses: 403 with the message, not stored.
	resp, out := h.do(t, req{method: "PUT", path: base + "asked/w.ics", user: user, pass: pw, body: icsDaily("W-1", "weekly thing", "20260101T090000Z", "PT30M"), headers: ct})
	if resp.StatusCode != 403 || !strings.Contains(out, "this pony runs daily") {
		t.Errorf("stack refusal = %d %s", resp.StatusCode, out)
	}
	seen := fs.wait(t, 1)
	env := seen[0]
	if gjson.Get(env, "_txc.src").String() != "calendar" || gjson.Get(env, "_txc.calendar.phase").String() != "answer" || gjson.Get(env, "_txc.calendar.op").String() != "put" ||
		gjson.Get(env, "_txc.calendar.tenant").String() != "acme" || gjson.Get(env, "_txc.calendar.account").String() != user ||
		gjson.Get(env, "_txc.calendar.calendar.name").String() != "asked" || gjson.Get(env, "_txc.calendar.object.name").String() != "w.ics" ||
		gjson.Get(env, "_txc.calendar.object.exists").Bool() || gjson.Get(env, "_txc.calendar.event.recur.freq").String() != "DAILY" ||
		!strings.Contains(gjson.Get(env, "_txc.calendar.ical").String(), "BEGIN:VEVENT") || gjson.Get(env, "_txc.client.ip").String() == "" {
		t.Errorf("answer envelope = %s", env)
	}
	// stack accepts with a rewrite: the stored object is the rewrite, UID kept.
	resp, _ = h.do(t, req{method: "PUT", path: base + "asked/s.ics", user: user, pass: pw, body: icsDaily("S-1", "snap me", "20260101T092300Z", "PT45M"), headers: ct})
	if resp.StatusCode != 201 {
		t.Fatalf("rewrite put = %d", resp.StatusCode)
	}
	resp, out = h.do(t, req{method: "GET", path: base + "asked/s.ics", user: user, pass: pw})
	if resp.StatusCode != 200 || !strings.Contains(out, "DTSTART:20260101T090000Z") || !strings.Contains(out, "SUMMARY:snap me") || !strings.Contains(out, "UID:S-1") || !strings.Contains(out, "DURATION:PT30M") {
		t.Errorf("rewritten object:\n%s", out)
	}
	// A rewrite of an EXISTING object bumps SEQUENCE past the stored one.
	resp, _ = h.do(t, req{method: "PUT", path: base + "asked/s.ics", user: user, pass: pw, body: icsDaily("S-1", "snap again", "20260101T101500Z", "PT45M"), headers: ct})
	if resp.StatusCode != 201 {
		t.Fatalf("rewrite update = %d", resp.StatusCode)
	}
	_, out = h.do(t, req{method: "GET", path: base + "asked/s.ics", user: user, pass: pw})
	if !strings.Contains(out, "SEQUENCE:1\r\n") {
		t.Errorf("rewrite update did not bump SEQUENCE:\n%s", out)
	}
	// stack delete: the envelope carries the prior facts.
	if resp, _ := h.do(t, req{method: "DELETE", path: base + "asked/s.ics", user: user, pass: pw}); resp.StatusCode != 204 {
		t.Errorf("stack delete = %d", resp.StatusCode)
	}
	seen = fs.wait(t, 4)
	if d := seen[3]; gjson.Get(d, "_txc.calendar.op").String() != "delete" || gjson.Get(d, "_txc.calendar.prior.event.summary").String() != "snap again" || gjson.Get(d, "_txc.calendar.object.uid").String() != "S-1" {
		t.Errorf("delete envelope = %s", d)
	}
	// observe (default): committed, then one fire-and-forget envelope.
	if resp, _ := h.do(t, req{method: "PUT", path: base + "watched/o.ics", user: user, pass: pw, body: icsDaily("O-1", "observed", "20260101T090000Z", "PT30M"), headers: ct}); resp.StatusCode != 201 {
		t.Errorf("observed put = %d", resp.StatusCode)
	}
	seen = fs.wait(t, 5)
	if o := seen[4]; gjson.Get(o, "_txc.calendar.phase").String() != "observe" || gjson.Get(o, "_txc.calendar.object.etag").String() == "" || gjson.Get(o, "_txc.calendar.calendar.name").String() != "watched" {
		t.Errorf("observe envelope = %s", o)
	}
	// Unsubscribed tenant with a stack policy ⇒ 503.
	h.ctrl.lanes.subscribed = func(string) bool { return false }
	if resp, _ := h.do(t, req{method: "PUT", path: base + "asked/n.ics", user: user, pass: pw, body: icsDaily("N-1", "x", "20260101T090000Z", "PT30M"), headers: ct}); resp.StatusCode != 503 {
		t.Errorf("unsubscribed stack policy = %d", resp.StatusCode)
	}
}

func TestThrottleCountsMissesOnly(t *testing.T) {
	h := newHarness(t, config.Config{CalendarLoginRate: 3})
	h.account(t, "acme", user, pw)
	h.calendar(t, "acme", user, "schedule", "")
	// Many correct logins: one miss, the rest cache hits — never throttled.
	for i := 0; i < 10; i++ {
		if resp, _ := h.do(t, req{method: "OPTIONS", path: "/dav/paris@pony.example.com/calendars/schedule/", user: user, pass: pw}); resp.StatusCode >= 400 {
			t.Fatalf("login %d = %d", i, resp.StatusCode)
		}
	}
	// Wrong passwords burn the budget, then 429.
	codes := []int{}
	for i := 0; i < 5; i++ {
		resp, _ := h.do(t, req{method: "OPTIONS", path: "/dav/", user: user, pass: "wrong"})
		codes = append(codes, resp.StatusCode)
	}
	if codes[0] != 401 || codes[4] != 429 {
		t.Errorf("codes = %v", codes)
	}
}

func TestEnvelopeAndAnswerTranslation(t *testing.T) {
	ev := chcal.Event{UID: "u", Summary: "s", Start: "2026-01-01T09:00:00Z"}
	m := mutation{tenant: "acme", account: user, op: opPut, calendar: calRef{ID: "cal_1", Name: "schedule", DisplayName: "S"},
		object: &objRef{Name: "a.ics", UID: "u", Size: 10}, ical: []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"), event: &ev, prior: &ev,
		props: map[string]string{"displayname": "x"}, clientIP: "203.0.113.9"}
	raw := buildEnvelope(m, phaseAnswer, "rid1", "node1", time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC))
	for path, want := range map[string]string{
		"_txc.src": "calendar", "_txc.rid": "rid1", "_ts": "2026-09-04T12:00:00Z", "_txc.calendar.tenant": "acme", "_txc.calendar.account": user,
		"_txc.calendar.phase": "answer", "_txc.calendar.op": "put", "_txc.calendar.node": "node1", "_txc.calendar.calendar.id": "cal_1",
		"_txc.calendar.object.name": "a.ics", "_txc.calendar.event.summary": "s", "_txc.calendar.prior.event.uid": "u",
		"_txc.calendar.props.displayname": "x", "_txc.client.ip": "203.0.113.9",
	} {
		if got := gjson.Get(raw, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if !gjson.Get(raw, "_txc.calendar.object.size").Exists() || gjson.Get(raw, "_txc.calendar.object.exists").Bool() {
		t.Errorf("object facts: %s", raw)
	}
	for raw, want := range map[string]answer{
		`{}`: {ok: false, outcome: "absent"},
		`{"_txc":{"calendar":{"res":{"ok":true}}}}`:                              {ok: true, outcome: "ok"},
		`{"_txc":{"calendar":{"res":{"ok":false,"code":"limit","msg":"full"}}}}`: {ok: false, code: "limit", msg: "full", outcome: "refused"},
		`{"_txc":{"calendar":{"res":{"ok":"yes"}}}}`:                             {ok: false, outcome: "refused"},
		`{"_txc":{"calendar":{"res":{"ok":false,"code":"weird"}}}}`:              {ok: false, outcome: "refused"},
	} {
		got := translateAnswer(raw)
		if got.ok != want.ok || got.code != want.code || got.outcome != want.outcome || (want.msg != "" && got.msg != want.msg) {
			t.Errorf("translateAnswer(%s) = %+v, want %+v", raw, got, want)
		}
	}
	a := translateAnswer(`{"_txc":{"calendar":{"res":{"ok":true,"event":{"summary":"re"}}}}}`)
	if a.rewrite == nil || a.rewrite.event == nil || a.rewrite.event.Summary != "re" {
		t.Errorf("event rewrite = %+v", a)
	}
	a = translateAnswer(`{"_txc":{"calendar":{"res":{"ok":true,"ical":"BEGIN:VCALENDAR"}}}}`)
	if a.rewrite == nil || string(a.rewrite.ical) != "BEGIN:VCALENDAR" {
		t.Errorf("ical rewrite = %+v", a)
	}
}

func TestPolicyDefaults(t *testing.T) {
	acct := &chcal.Account{Policy: json.RawMessage(`{"mkcalendar":"observe"}`)}
	cal := &chcal.Calendar{Policy: json.RawMessage(`{"put":"stack","proppatch":"deny"}`)}
	for verb, want := range map[string]string{
		chcal.VerbPut: "stack", chcal.VerbDelete: "observe", chcal.VerbProppatch: "deny", chcal.VerbRemove: "deny", chcal.VerbMkcalendar: "observe",
	} {
		if got := chcal.PolicyMode(cal, acct, verb); got != want {
			t.Errorf("%s = %s, want %s", verb, got, want)
		}
	}
	if chcal.PolicyMode(nil, nil, chcal.VerbMkcalendar) != "deny" || chcal.PolicyMode(nil, nil, chcal.VerbProppatch) != "local" || chcal.PolicyMode(nil, nil, chcal.VerbPut) != "observe" {
		t.Error("chassis defaults")
	}
	if err := chcal.ValidatePolicy(json.RawMessage(`{"put":"maybe"}`)); err == nil {
		t.Error("bad mode accepted")
	}
	if err := chcal.ValidatePolicy(json.RawMessage(`{"fly":"deny"}`)); err == nil {
		t.Error("bad verb accepted")
	}
}
