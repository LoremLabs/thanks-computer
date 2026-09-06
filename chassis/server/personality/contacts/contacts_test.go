package contacts

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
	"github.com/loremlabs/thanks-computer/chassis/config"
	chcon "github.com/loremlabs/thanks-computer/chassis/contacts"
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
	store *chcon.Store
	srv   *httptest.Server
}

func newHarness(t *testing.T, conf config.Config) *harness {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(t.TempDir(), "contacts.db")+"?mode=rwc&_journal_mode=WAL&_busy_timeout=15000&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := chcon.NewStore(db, registry.SQLite)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	conf.Personalities = "web,contacts"
	if conf.ContactsPathPrefix == "" {
		conf.ContactsPathPrefix = "/carddav"
	}
	if conf.ContactsLoginRate == 0 {
		conf.ContactsLoginRate = 100
	}
	if conf.ContactsObserveSample == 0 {
		conf.ContactsObserveSample = 1
	}
	if conf.ContactsRespTimeout == "" {
		conf.ContactsRespTimeout = "30s"
	}
	if conf.ContactsObjectMaxBytes == 0 {
		conf.ContactsObjectMaxBytes = 1 << 20
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Admission: fakeAdmission{suspended: "suspended"}}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, store, fakeResolver{"pony.example.com": "acme", "other.example.com": "other", "sad.example.com": "suspended"})
	ctrl.now = func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) }
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

func (h *harness) book(t *testing.T, tenant, username, name, policy string) chcon.Addressbook {
	t.Helper()
	ab := chcon.Addressbook{Tenant: tenant, Username: username, Name: name, DisplayName: "Senders"}
	if policy != "" {
		ab.Policy = json.RawMessage(policy)
	}
	got, _, err := h.store.EnsureAddressbook(context.Background(), ab)
	if err != nil {
		t.Fatal(err)
	}
	return got
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
	switch r.method {
	case "PROPFIND", "REPORT", "PROPPATCH", "MKCOL":
		if r.body != "" {
			hr.Header.Set("Content-Type", "text/xml; charset=utf-8")
		}
	case "PUT":
		hr.Header.Set("Content-Type", "text/vcard; charset=utf-8")
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
	base = "/carddav/paris@pony.example.com/addressbooks/senders/"
)

// appleCard is what macOS Contacts sends: 3.0, item groups, TYPE=pref, an
// X-ABLabel with angle brackets, an escaped `\;`.
func appleCard(uid, fn, email, rev string) string {
	return "BEGIN:VCARD\r\nVERSION:3.0\r\nPRODID:-//Apple Inc.//macOS 15.0//EN\r\nN:" + fn + ";;;;\r\nFN:" + fn + "\r\n" +
		"item1.EMAIL;type=INTERNET;type=pref:" + email + "\r\nitem1.X-ABLabel:_$!<Other>!$_\r\nNOTE:Call first\\; then email.\r\n" +
		"UID:" + uid + "\r\nREV:" + rev + "\r\nEND:VCARD\r\n"
}

func TestAuthAndDiscovery(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	h.account(t, "other", "x@other.example.com", pw)
	h.account(t, "suspended", "y@sad.example.com", pw)
	h.book(t, "acme", user, "senders", "")

	if resp, _ := h.do(t, req{method: "PROPFIND", path: "/carddav/", host: "nobody.example.com", user: user, pass: pw}); resp.StatusCode != 404 {
		t.Errorf("unrouted host = %d", resp.StatusCode)
	}
	resp, _ := h.do(t, req{method: "PROPFIND", path: "/.well-known/carddav"})
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://pony.example.com/carddav/" {
		t.Errorf("well-known = %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	hr, _ := http.NewRequest("PROPFIND", h.srv.URL+"/carddav/", nil)
	hr.Host = "pony.example.com"
	hr.SetBasicAuth(user, pw)
	if r, _ := http.DefaultClient.Do(hr); r.StatusCode != 403 {
		t.Errorf("plaintext = %d", r.StatusCode)
	}
	resp, _ = h.do(t, req{method: "PROPFIND", path: "/carddav/"})
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `Basic realm="contacts"`) {
		t.Errorf("no creds = %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	for name, r := range map[string]req{
		"bad password": {method: "PROPFIND", path: "/carddav/", user: user, pass: "nope"},
		"unknown user": {method: "PROPFIND", path: "/carddav/", user: "ghost@pony.example.com", pass: pw},
		"wrong tenant": {method: "PROPFIND", path: "/carddav/", user: "x@other.example.com", pass: pw},
		"other's tree": {method: "PROPFIND", path: "/carddav/x@other.example.com/", user: user, pass: pw},
		"other's book": {method: "PROPFIND", path: "/carddav/x@other.example.com/addressbooks/senders/", user: user, pass: pw},
		"suspended":    {method: "PROPFIND", path: "/carddav/", host: "sad.example.com", user: "y@sad.example.com", pass: pw},
	} {
		resp, _ := h.do(t, r)
		want := 401
		switch name {
		case "other's tree", "other's book":
			want = 403
		case "suspended":
			want = 402
		}
		if resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", name, resp.StatusCode, want)
		}
	}
	// The discovery a client performs: the root's response href must be
	// the ROOT, and the principal must carry the address book home set.
	body := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:current-user-principal/><D:principal-URL/><D:resourcetype/><D:quota-used-bytes/></D:prop></D:propfind>`
	resp, out := h.do(t, req{method: "PROPFIND", path: "/carddav/", user: user, pass: pw, body: body, headers: map[string]string{"Depth": "0"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "<D:response><D:href>/carddav/</D:href>") ||
		!strings.Contains(out, "<D:current-user-principal><D:href>/carddav/paris%40pony.example.com/</D:href>") ||
		!strings.Contains(out, "quota-used-bytes") || !strings.Contains(out, "404 Not Found") || resp.Header.Get("DAV") != "1, 3, addressbook" {
		t.Errorf("root propfind = %d %s\n%s", resp.StatusCode, resp.Header.Get("DAV"), out)
	}
	body = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><C:addressbook-home-set/><C:principal-address/><D:principal-URL/><D:resourcetype/><C:directory-gateway/></D:prop></D:propfind>`
	resp, out = h.do(t, req{method: "PROPFIND", path: "/carddav/paris%40pony.example.com/", user: user, pass: pw, body: body, headers: map[string]string{"Depth": "0"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "<D:response><D:href>/carddav/paris%40pony.example.com/</D:href>") ||
		!strings.Contains(out, "<C:addressbook-home-set><D:href>/carddav/paris%40pony.example.com/addressbooks/</D:href>") ||
		!strings.Contains(out, "mailto:paris@pony.example.com") || !strings.Contains(out, "<D:principal/>") || !strings.Contains(out, "directory-gateway") {
		t.Errorf("principal propfind = %d\n%s", resp.StatusCode, out)
	}
	resp, out = h.do(t, req{method: "PROPFIND", path: "/carddav/paris@pony.example.com", user: user, pass: pw, body: body, headers: map[string]string{"Depth": "0"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "<D:response><D:href>/carddav/paris@pony.example.com/</D:href>") || !strings.Contains(out, "<D:href>/carddav/paris@pony.example.com/addressbooks/</D:href>") {
		t.Errorf("principal propfind (raw, no slash) = %d\n%s", resp.StatusCode, out)
	}
	// A bare local part completes to the request's host (what a person
	// types into an account dialog); on another host it names nobody.
	resp, out = h.do(t, req{method: "PROPFIND", path: "/carddav/", user: "paris", pass: pw, body: body, headers: map[string]string{"Depth": "0"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "/carddav/paris%40pony.example.com/") {
		t.Errorf("bare local part = %d\n%s", resp.StatusCode, out)
	}
	if resp, _ := h.do(t, req{method: "PROPFIND", path: "/carddav/", host: "other.example.com", user: "paris", pass: pw, body: body, headers: map[string]string{"Depth": "0"}}); resp.StatusCode != 401 {
		t.Errorf("bare local part on another host = %d", resp.StatusCode)
	}
	// The home set (the library): the book with its display name and type.
	body = `<?xml version="1.0"?><D:propfind xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><D:displayname/><D:resourcetype/><C:supported-address-data/></D:prop></D:propfind>`
	resp, out = h.do(t, req{method: "PROPFIND", path: "/carddav/paris@pony.example.com/addressbooks/", user: user, pass: pw, body: body, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "/carddav/paris@pony.example.com/addressbooks/senders/") || !strings.Contains(out, "Senders") || !strings.Contains(out, "addressbook") {
		t.Errorf("home propfind = %d\n%s", resp.StatusCode, out)
	}
}

func TestObjectsMultigetAndQuery(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	ab := h.book(t, "acme", user, "senders", "")
	sent := appleCard("8F2C-UID-1", "Carol Example", "Carol@Example.com", "2026-09-05T10:00:00Z")

	// PUT creates: 201 + ETag; the stored bytes are the client's.
	resp, _ := h.do(t, req{method: "PUT", path: base + "carol.vcf", user: user, pass: pw, body: sent, headers: map[string]string{"If-None-Match": "*"}})
	etag := resp.Header.Get("ETag")
	if resp.StatusCode != 201 || etag == "" {
		t.Fatalf("put = %d etag=%q", resp.StatusCode, etag)
	}
	o, ok, _ := h.store.GetObject(context.Background(), ab.ID, "carol.vcf")
	if !ok || `"`+o.ETag+`"` != etag || string(o.VCard) != sent || o.FN != "Carol Example" || o.Addresses[0] != "carol@example.com" || o.Kind != "individual" {
		t.Errorf("stored = %+v ok=%v etag=%s", o, ok, etag)
	}
	// GET returns the same bytes.
	resp, out := h.do(t, req{method: "GET", path: base + "carol.vcf", user: user, pass: pw})
	if resp.StatusCode != 200 || out != sent || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/vcard") || resp.Header.Get("ETag") != etag {
		t.Errorf("get = %d %s etag=%s\n%s", resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("ETag"), out)
	}
	if resp, _ := h.do(t, req{method: "GET", path: base + "carol.vcf", user: user, pass: pw, headers: map[string]string{"If-None-Match": etag}}); resp.StatusCode != 304 {
		t.Errorf("get 304 = %d", resp.StatusCode)
	}
	if resp, out := h.do(t, req{method: "HEAD", path: base + "carol.vcf", user: user, pass: pw}); resp.StatusCode != 200 || out != "" {
		t.Errorf("head = %d %q", resp.StatusCode, out)
	}
	// Same card, new REV ⇒ 204 and the same etag (a no-op).
	resp, _ = h.do(t, req{method: "PUT", path: base + "carol.vcf", user: user, pass: pw, body: appleCard("8F2C-UID-1", "Carol Example", "Carol@Example.com", "2026-09-05T11:00:00Z")})
	if resp.StatusCode != 204 || resp.Header.Get("ETag") != etag {
		t.Errorf("noop put = %d etag=%s", resp.StatusCode, resp.Header.Get("ETag"))
	}
	// Preconditions.
	if resp, _ := h.do(t, req{method: "PUT", path: base + "carol.vcf", user: user, pass: pw, body: sent, headers: map[string]string{"If-None-Match": "*"}}); resp.StatusCode != 412 {
		t.Errorf("if-none-match on existing = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "PUT", path: base + "carol.vcf", user: user, pass: pw, body: appleCard("8F2C-UID-1", "Carol E", "carol@example.com", "1"), headers: map[string]string{"If-Match": `"stale"`}}); resp.StatusCode != 412 {
		t.Errorf("stale if-match = %d", resp.StatusCode)
	}
	resp, _ = h.do(t, req{method: "PUT", path: base + "carol.vcf", user: user, pass: pw, body: appleCard("8F2C-UID-1", "Carol E", "carol@example.com", "1"), headers: map[string]string{"If-Match": etag}})
	if resp.StatusCode != 204 || resp.Header.Get("ETag") == etag || resp.Header.Get("ETag") == "" {
		t.Errorf("update = %d etag=%s", resp.StatusCode, resp.Header.Get("ETag"))
	}
	// A second resource reusing the UID ⇒ 409 no-uid-conflict.
	resp, out = h.do(t, req{method: "PUT", path: base + "dupe.vcf", user: user, pass: pw, body: appleCard("8F2C-UID-1", "Dupe", "d@x.io", "1")})
	if resp.StatusCode != 409 || !strings.Contains(out, "no-uid-conflict") {
		t.Errorf("uid conflict = %d %s", resp.StatusCode, out)
	}
	// Garbage ⇒ 400 valid-address-data; 2.1 ⇒ 415; wrong type ⇒ 415; oversized ⇒ 413.
	resp, out = h.do(t, req{method: "PUT", path: base + "bad.vcf", user: user, pass: pw, body: "nope"})
	if resp.StatusCode != 400 || !strings.Contains(out, "valid-address-data") {
		t.Errorf("garbage = %d %s", resp.StatusCode, out)
	}
	if resp, _ := h.do(t, req{method: "PUT", path: base + "old.vcf", user: user, pass: pw, body: "BEGIN:VCARD\r\nVERSION:2.1\r\nUID:o\r\nN:x\r\nEND:VCARD\r\n"}); resp.StatusCode != 415 {
		t.Errorf("2.1 = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "PUT", path: base + "json.vcf", user: user, pass: pw, body: sent, headers: map[string]string{"Content-Type": "application/json"}}); resp.StatusCode != 415 {
		t.Errorf("wrong content type = %d", resp.StatusCode)
	}
	h.ctrl.maxBytes = 200
	if resp, _ := h.do(t, req{method: "PUT", path: base + "big.vcf", user: user, pass: pw, body: appleCard("B-1", strings.Repeat("x", 300), "b@x.io", "1")}); resp.StatusCode != 413 {
		t.Errorf("oversized = %d", resp.StatusCode)
	}
	h.ctrl.maxBytes = 1 << 20
	// A group card is accepted and its facts say so.
	grp := "BEGIN:VCARD\r\nVERSION:3.0\r\nN:Team;;;;\r\nFN:Team\r\nX-ADDRESSBOOKSERVER-KIND:group\r\nX-ADDRESSBOOKSERVER-MEMBER:urn:uuid:8F2C-UID-1\r\nUID:GROUP-1\r\nEND:VCARD\r\n"
	if resp, _ := h.do(t, req{method: "PUT", path: base + "team.vcf", user: user, pass: pw, body: grp}); resp.StatusCode != 201 {
		t.Errorf("group put = %d", resp.StatusCode)
	}
	if g, _, _ := h.store.GetObject(context.Background(), ab.ID, "team.vcf"); g.Kind != "group" || len(g.Addresses) != 0 {
		t.Errorf("group facts = %+v", g)
	}
	// multiget: bytes verbatim (XML-escaped), a 404 for a missing href,
	// and 404-propstat for a property we do not have.
	mg := `<?xml version="1.0"?><C:addressbook-multiget xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><D:getetag/><C:address-data/><D:getlastmodified/></D:prop>` +
		`<D:href>` + base + `carol.vcf</D:href><D:href>/carddav/paris%40pony.example.com/addressbooks/senders/team.vcf</D:href><D:href>` + base + `missing.vcf</D:href></C:addressbook-multiget>`
	resp, out = h.do(t, req{method: "REPORT", path: base, user: user, pass: pw, body: mg, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || strings.Count(out, "<D:response>") != 3 || !strings.Contains(out, "FN:Carol E") || !strings.Contains(out, "_$!&lt;Other&gt;!$_") ||
		!strings.Contains(out, "X-ADDRESSBOOKSERVER-KIND:group") || !strings.Contains(out, "<D:href>"+base+"missing.vcf</D:href><D:status>HTTP/1.1 404 Not Found</D:status>") ||
		!strings.Contains(out, "getlastmodified") || strings.Count(out, "<D:getetag>") != 2 {
		t.Errorf("multiget = %d\n%s", resp.StatusCode, out)
	}
	// addressbook-query (the library) on FN.
	q := `<?xml version="1.0"?><C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:prop><D:getetag/><C:address-data/></D:prop>` +
		`<C:filter><C:prop-filter name="FN"><C:text-match collation="i;unicode-casemap" match-type="contains">Carol</C:text-match></C:prop-filter></C:filter></C:addressbook-query>`
	resp, out = h.do(t, req{method: "REPORT", path: base, user: user, pass: pw, body: q, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || !strings.Contains(out, "carol.vcf") || strings.Contains(out, "team.vcf") {
		t.Errorf("query = %d\n%s", resp.StatusCode, out)
	}
	// Depth:1 PROPFIND on the book (the library) lists both with etags.
	pf := `<?xml version="1.0"?><D:propfind xmlns:D="DAV:"><D:prop><D:getetag/><D:getcontenttype/></D:prop></D:propfind>`
	resp, out = h.do(t, req{method: "PROPFIND", path: base, user: user, pass: pw, body: pf, headers: map[string]string{"Depth": "1"}})
	if resp.StatusCode != 207 || strings.Count(out, "getetag") < 2 {
		t.Errorf("book propfind = %d\n%s", resp.StatusCode, out)
	}
	// DELETE, then a re-PUT of the same name resurrects it.
	if resp, _ := h.do(t, req{method: "DELETE", path: base + "team.vcf", user: user, pass: pw}); resp.StatusCode != 204 {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "DELETE", path: base + "team.vcf", user: user, pass: pw}); resp.StatusCode != 404 {
		t.Errorf("second delete = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "PUT", path: base + "team.vcf", user: user, pass: pw, body: strings.Replace(grp, "UID:GROUP-1", "UID:GROUP-2", 1), headers: map[string]string{"If-None-Match": "*"}}); resp.StatusCode != 201 {
		t.Errorf("resurrect = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "GET", path: base + "nope.vcf", user: user, pass: pw}); resp.StatusCode != 404 {
		t.Errorf("get missing = %d", resp.StatusCode)
	}
	if resp, _ := h.do(t, req{method: "PUT", path: "/carddav/paris@pony.example.com/addressbooks/nobook/x.vcf", user: user, pass: pw, body: sent}); resp.StatusCode != 404 {
		t.Errorf("put into a missing book = %d", resp.StatusCode)
	}
}

func TestMkcolProppatchAndRemove(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	home := "/carddav/paris@pony.example.com/addressbooks/"
	mk := `<?xml version="1.0"?><D:mkcol xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav"><D:set><D:prop><D:resourcetype><D:collection/><C:addressbook/></D:resourcetype><D:displayname>Mine</D:displayname><C:addressbook-description>d</C:addressbook-description></D:prop></D:set></D:mkcol>`
	// Default policy denies client-created address books.
	if resp, _ := h.do(t, req{method: "MKCOL", path: home + "7A3B-UUID/", user: user, pass: pw, body: mk}); resp.StatusCode != 403 {
		t.Errorf("mkcol under default policy = %d", resp.StatusCode)
	}
	if _, err := h.store.UpsertAccount(context.Background(), "acme", user, "", "", json.RawMessage(`{"mkaddressbook":"local","remove":"local"}`)); err != nil {
		t.Fatal(err)
	}
	if resp, body := h.do(t, req{method: "MKCOL", path: home + "7A3B-UUID/", user: user, pass: pw, body: mk}); resp.StatusCode != 201 {
		t.Fatalf("mkcol = %d %s", resp.StatusCode, body)
	}
	ab, ok, _ := h.store.GetAddressbook(context.Background(), "acme", user, "7A3B-UUID")
	if !ok || ab.DisplayName != "Mine" || ab.Description != "d" {
		t.Errorf("created = %+v ok=%v", ab, ok)
	}
	if resp, _ := h.do(t, req{method: "MKCOL", path: home + "7A3B-UUID/", user: user, pass: pw, body: mk}); resp.StatusCode != 405 {
		t.Errorf("mkcol existing = %d", resp.StatusCode)
	}
	// PROPPATCH: known props 200, unknown 403, in one 207.
	pp := `<?xml version="1.0"?><D:propertyupdate xmlns:D="DAV:" xmlns:X="http://example.com/x"><D:set><D:prop><D:displayname>Renamed</D:displayname><X:foo>bar</X:foo></D:prop></D:set></D:propertyupdate>`
	resp, out := h.do(t, req{method: "PROPPATCH", path: home + "7A3B-UUID/", user: user, pass: pw, body: pp})
	if resp.StatusCode != 207 || !strings.Contains(out, "200 OK") || !strings.Contains(out, "403 Forbidden") {
		t.Errorf("proppatch = %d\n%s", resp.StatusCode, out)
	}
	if got, _, _ := h.store.GetAddressbook(context.Background(), "acme", user, "7A3B-UUID"); got.DisplayName != "Renamed" {
		t.Errorf("proppatch did not apply: %+v", got)
	}
	if resp, _ := h.do(t, req{method: "PROPPATCH", path: home + "7A3B-UUID/x.vcf", user: user, pass: pw, body: pp}); resp.StatusCode != 403 {
		t.Errorf("proppatch on object = %d", resp.StatusCode)
	}
	// DELETE the book (account policy local).
	if resp, _ := h.do(t, req{method: "DELETE", path: home + "7A3B-UUID/", user: user, pass: pw}); resp.StatusCode != 204 {
		t.Errorf("remove = %d", resp.StatusCode)
	}
	if _, ok, _ := h.store.GetAddressbook(context.Background(), "acme", user, "7A3B-UUID"); ok {
		t.Error("book still live after DELETE")
	}
}

func TestPolicyLanesAndRewrite(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.account(t, "acme", user, pw)
	h.book(t, "acme", user, "locked", `{"put":"deny","delete":"deny"}`)
	h.book(t, "acme", user, "asked", `{"put":"stack","delete":"stack"}`)
	h.book(t, "acme", user, "watched", "")
	fs := &fakeStack{respond: func(raw string) string {
		if gjson.Get(raw, "_txc.contacts.phase").String() != "answer" {
			return "{}"
		}
		fn := gjson.Get(raw, "_txc.contacts.card.fn").String()
		switch {
		case gjson.Get(raw, "_txc.contacts.op").String() == "put" && len(gjson.Get(raw, "_txc.contacts.card.addresses").Array()) == 0:
			return `{"_txc":{"contacts":{"res":{"ok":false,"code":"cannot","msg":"add an email address to this card"}}}}`
		case strings.Contains(fn, "rewrite"):
			return `{"_txc":{"contacts":{"res":{"ok":true,"card":{"fn":"Rewritten","emails":[{"value":"snap@x.io"}]}}}}}`
		}
		return `{"_txc":{"contacts":{"res":{"ok":true}}}}`
	}}
	h.withStack(t, fs)
	books := "/carddav/paris@pony.example.com/addressbooks/"

	// deny: 403, nothing dispatched.
	if resp, _ := h.do(t, req{method: "PUT", path: books + "locked/a.vcf", user: user, pass: pw, body: appleCard("L-1", "x", "x@x.io", "1")}); resp.StatusCode != 403 {
		t.Errorf("deny put = %d", resp.StatusCode)
	}
	if len(fs.seen) != 0 {
		t.Fatalf("deny must not dispatch: %v", fs.seen)
	}
	// stack refuses: 403 with the message, not stored.
	noEmail := "BEGIN:VCARD\r\nVERSION:3.0\r\nN:Nobody;;;;\r\nFN:Nobody\r\nUID:N-1\r\nEND:VCARD\r\n"
	resp, out := h.do(t, req{method: "PUT", path: books + "asked/n.vcf", user: user, pass: pw, body: noEmail})
	if resp.StatusCode != 403 || !strings.Contains(out, "add an email address to this card") || !strings.Contains(out, "responsedescription") {
		t.Errorf("stack refusal = %d %s", resp.StatusCode, out)
	}
	seen := fs.wait(t, 1)
	env := seen[0]
	if gjson.Get(env, "_txc.src").String() != "contacts" || gjson.Get(env, "_txc.contacts.phase").String() != "answer" || gjson.Get(env, "_txc.contacts.op").String() != "put" ||
		gjson.Get(env, "_txc.contacts.tenant").String() != "acme" || gjson.Get(env, "_txc.contacts.account").String() != user ||
		gjson.Get(env, "_txc.contacts.addressbook.name").String() != "asked" || gjson.Get(env, "_txc.contacts.object.name").String() != "n.vcf" ||
		gjson.Get(env, "_txc.contacts.object.exists").Bool() || gjson.Get(env, "_txc.contacts.card.fn").String() != "Nobody" ||
		!strings.Contains(gjson.Get(env, "_txc.contacts.vcard").String(), "BEGIN:VCARD") || gjson.Get(env, "_txc.client.ip").String() == "" {
		t.Errorf("answer envelope = %s", env)
	}
	// stack accepts with a rewrite: the stored object is the rewrite, UID kept.
	resp, _ = h.do(t, req{method: "PUT", path: books + "asked/s.vcf", user: user, pass: pw, body: appleCard("S-1", "please rewrite", "s@x.io", "1")})
	if resp.StatusCode != 201 {
		t.Fatalf("rewrite put = %d", resp.StatusCode)
	}
	resp, out = h.do(t, req{method: "GET", path: books + "asked/s.vcf", user: user, pass: pw})
	if resp.StatusCode != 200 || !strings.Contains(out, "FN:Rewritten\r\n") || !strings.Contains(out, "UID:S-1\r\n") || !strings.Contains(out, "EMAIL;TYPE=INTERNET:snap@x.io") || !strings.Contains(out, "PRODID:"+chcon.ProdID) {
		t.Errorf("rewritten object:\n%s", out)
	}
	// stack accepts as-is: the client's bytes are stored.
	sent := appleCard("K-1", "Kept", "k@x.io", "1")
	if resp, _ := h.do(t, req{method: "PUT", path: books + "asked/k.vcf", user: user, pass: pw, body: sent}); resp.StatusCode != 201 {
		t.Fatalf("kept put = %d", resp.StatusCode)
	}
	if _, out := h.do(t, req{method: "GET", path: books + "asked/k.vcf", user: user, pass: pw}); out != sent {
		t.Errorf("kept object differs:\n%s", out)
	}
	// stack delete: the envelope carries the prior facts.
	if resp, _ := h.do(t, req{method: "DELETE", path: books + "asked/k.vcf", user: user, pass: pw}); resp.StatusCode != 204 {
		t.Errorf("stack delete = %d", resp.StatusCode)
	}
	seen = fs.wait(t, 4)
	if d := seen[3]; gjson.Get(d, "_txc.contacts.op").String() != "delete" || gjson.Get(d, "_txc.contacts.prior.card.fn").String() != "Kept" ||
		gjson.Get(d, "_txc.contacts.prior.card.addresses.0").String() != "k@x.io" || gjson.Get(d, "_txc.contacts.object.uid").String() != "K-1" {
		t.Errorf("delete envelope = %s", d)
	}
	// observe (default): committed, then one fire-and-forget envelope; a
	// no-op re-PUT dispatches nothing.
	if resp, _ := h.do(t, req{method: "PUT", path: books + "watched/o.vcf", user: user, pass: pw, body: appleCard("O-1", "observed", "o@x.io", "1")}); resp.StatusCode != 201 {
		t.Errorf("observed put = %d", resp.StatusCode)
	}
	seen = fs.wait(t, 5)
	if o := seen[4]; gjson.Get(o, "_txc.contacts.phase").String() != "observe" || gjson.Get(o, "_txc.contacts.object.etag").String() == "" || gjson.Get(o, "_txc.contacts.addressbook.name").String() != "watched" {
		t.Errorf("observe envelope = %s", o)
	}
	if resp, _ := h.do(t, req{method: "PUT", path: books + "watched/o.vcf", user: user, pass: pw, body: appleCard("O-1", "observed", "o@x.io", "2")}); resp.StatusCode != 204 {
		t.Errorf("observed noop = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)
	fs.mu.Lock()
	n := len(fs.seen)
	fs.mu.Unlock()
	if n != 5 {
		t.Errorf("a no-op put dispatched an observe envelope (%d envelopes)", n)
	}
	// Unsubscribed tenant with a stack policy ⇒ 503.
	h.ctrl.lanes.subscribed = func(string) bool { return false }
	if resp, _ := h.do(t, req{method: "PUT", path: books + "asked/u.vcf", user: user, pass: pw, body: appleCard("U-1", "x", "u@x.io", "1")}); resp.StatusCode != 503 {
		t.Errorf("unsubscribed stack policy = %d", resp.StatusCode)
	}
}

func TestThrottleCountsMissesOnly(t *testing.T) {
	h := newHarness(t, config.Config{ContactsLoginRate: 3})
	h.account(t, "acme", user, pw)
	h.book(t, "acme", user, "senders", "")
	for i := 0; i < 10; i++ {
		if resp, _ := h.do(t, req{method: "OPTIONS", path: base, user: user, pass: pw}); resp.StatusCode >= 400 {
			t.Fatalf("login %d = %d", i, resp.StatusCode)
		}
	}
	codes := []int{}
	for i := 0; i < 5; i++ {
		resp, _ := h.do(t, req{method: "OPTIONS", path: "/carddav/", user: user, pass: "wrong"})
		codes = append(codes, resp.StatusCode)
	}
	if codes[0] != 401 || codes[4] != 429 {
		t.Errorf("codes = %v", codes)
	}
}

func TestEnvelopeAndAnswerTranslation(t *testing.T) {
	card := chcon.Card{UID: "u", FN: "s", Addresses: []string{"s@x.io"}}
	m := mutation{tenant: "acme", account: user, op: opPut, addressbook: abRef{ID: "ab_1", Name: "senders", DisplayName: "S"},
		object: &objRef{Name: "a.vcf", UID: "u", Size: 10}, vcard: []byte("BEGIN:VCARD\r\nEND:VCARD\r\n"), card: &card, prior: &card,
		props: map[string]string{"displayname": "x"}, clientIP: "203.0.113.9"}
	raw := buildEnvelope(m, phaseAnswer, "rid1", "node1", time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
	for path, want := range map[string]string{
		"_txc.src": "contacts", "_txc.rid": "rid1", "_ts": "2026-09-05T12:00:00Z", "_txc.contacts.tenant": "acme", "_txc.contacts.account": user,
		"_txc.contacts.phase": "answer", "_txc.contacts.op": "put", "_txc.contacts.node": "node1", "_txc.contacts.addressbook.id": "ab_1",
		"_txc.contacts.object.name": "a.vcf", "_txc.contacts.card.fn": "s", "_txc.contacts.card.addresses.0": "s@x.io", "_txc.contacts.prior.card.uid": "u",
		"_txc.contacts.props.displayname": "x", "_txc.client.ip": "203.0.113.9",
	} {
		if got := gjson.Get(raw, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	for raw, want := range map[string]answer{
		`{}`: {ok: false, outcome: "absent"},
		`{"_txc":{"contacts":{"res":{"ok":true}}}}`:                              {ok: true, outcome: "ok"},
		`{"_txc":{"contacts":{"res":{"ok":false,"code":"limit","msg":"full"}}}}`: {ok: false, code: "limit", msg: "full", outcome: "refused"},
		`{"_txc":{"contacts":{"res":{"ok":"yes"}}}}`:                             {ok: false, outcome: "refused"},
	} {
		got := translateAnswer(raw)
		if got.ok != want.ok || got.code != want.code || got.outcome != want.outcome || (want.msg != "" && got.msg != want.msg) {
			t.Errorf("translateAnswer(%s) = %+v, want %+v", raw, got, want)
		}
	}
	a := translateAnswer(`{"_txc":{"contacts":{"res":{"ok":true,"card":{"fn":"re"}}}}}`)
	if a.rewrite == nil || a.rewrite.card == nil || a.rewrite.card.FN != "re" {
		t.Errorf("card rewrite = %+v", a)
	}
	a = translateAnswer(`{"_txc":{"contacts":{"res":{"ok":true,"vcard":"BEGIN:VCARD"}}}}`)
	if a.rewrite == nil || string(a.rewrite.vcard) != "BEGIN:VCARD" {
		t.Errorf("vcard rewrite = %+v", a)
	}
}

func TestPolicyDefaults(t *testing.T) {
	acct := &chcon.Account{Policy: json.RawMessage(`{"mkaddressbook":"observe"}`)}
	ab := &chcon.Addressbook{Policy: json.RawMessage(`{"put":"stack","proppatch":"deny"}`)}
	for verb, want := range map[string]string{
		chcon.VerbPut: "stack", chcon.VerbDelete: "observe", chcon.VerbProppatch: "deny", chcon.VerbRemove: "deny", chcon.VerbMkaddressbook: "observe",
	} {
		if got := chcon.PolicyMode(ab, acct, verb); got != want {
			t.Errorf("%s = %s, want %s", verb, got, want)
		}
	}
	if chcon.PolicyMode(nil, nil, chcon.VerbMkaddressbook) != "deny" || chcon.PolicyMode(nil, nil, chcon.VerbProppatch) != "local" || chcon.PolicyMode(nil, nil, chcon.VerbPut) != "observe" {
		t.Error("chassis defaults")
	}
}
