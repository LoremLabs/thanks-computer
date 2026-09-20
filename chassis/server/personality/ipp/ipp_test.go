package ipp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/authn"
	"github.com/loremlabs/thanks-computer/chassis/authn/authntest"
	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// --- fakes ----------------------------------------------------------------------

type fakeAdmission struct{ suspended string }

func (f fakeAdmission) Decide(tenant string) admission.Decision {
	if tenant == f.suspended {
		return admission.Decision{Admit: false, Status: 402, Reason: "suspended", Retry: 30 * time.Second}
	}
	return admission.Decision{Admit: true}
}
func (fakeAdmission) AllowRate(string) (bool, time.Duration)           { return true, 0 }
func (fakeAdmission) AcquireConcurrency(string, *admission.Lease) bool { return true }

// memIndex is the sha-ownership half of blob.Index, in memory.
type memIndex struct {
	mu   sync.Mutex
	shas map[string]blob.ShaRow // tenant + "/" + sha
	fail error
}

func (m *memIndex) GetName(context.Context, string, string) (blob.NameRow, bool, error) {
	return blob.NameRow{}, false, nil
}
func (m *memIndex) PutName(context.Context, string, blob.NameRow) error { return nil }
func (m *memIndex) DeleteName(context.Context, string, string) error    { return nil }
func (m *memIndex) ListNames(context.Context, string, blob.ListOpts) (blob.ListPage, error) {
	return blob.ListPage{}, nil
}
func (m *memIndex) GetSha(_ context.Context, tenant, sha string) (blob.ShaRow, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.shas[tenant+"/"+sha]
	return r, ok, nil
}
func (m *memIndex) PutShaIfAbsent(_ context.Context, tenant string, row blob.ShaRow) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return false, m.fail
	}
	if _, ok := m.shas[tenant+"/"+row.SHA256]; ok {
		return false, nil
	}
	m.shas[tenant+"/"+row.SHA256] = row
	return true, nil
}

// failingCAS refuses every stream — a storage backend that is down.
type failingCAS struct{ filecas.Store }

func (failingCAS) PutStream(context.Context, io.Reader, int64) (string, int64, error) {
	return "", 0, errors.New("bucket unavailable")
}

// --- harness --------------------------------------------------------------------

const (
	zoneHost = "ipp.dripl.example"
	tenant   = "driplit"
	// The default client: the address pony:research is bound under, and a
	// password that carries a credential id (authntest.Grant).
	username  = "research@dripl.example"
	principal = "pony:research"
	password  = "bcdf-five-correct-horse-battery-staples"
	// Another principal of the same tenant, with a printer of its own.
	lyonUsername = "lyon@dripl.example"
	lyonPassword = "ghjk-lyon-has-its-own-password"
	// The `other` tenant's client signs in under the same address — a binding
	// is per tenant — with its own password.
	otherPassword = "mnpq-others-password"
)

type harness struct {
	t     *testing.T
	ctrl  *Controller
	store *chipp.Store
	fcas  *filestore.FileStore
	ix    *memIndex
	srv   *httptest.Server // TLS
	plain *httptest.Server
	bus   chan *event.Envelope
	mu    sync.Mutex
	seen  []string              // envelopes the "bus" accepted
	who   []authn.Authenticated // …and whom each was pinned to
	zones map[string]string
	subs  map[string]bool
	ids   *authn.Store
}

func newHarness(t *testing.T, conf config.Config) *harness {
	t.Helper()
	store, err := chipp.Open("sqlite", chipp.Config{DBPath: filepath.Join(t.TempDir(), "ipp.db")})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	conf.Personalities = "web,ipp"
	conf.IPPAnonymousAttributes = true
	if conf.LoginRate == 0 {
		conf.LoginRate = 1000
	}
	if conf.IPPMaxJobBytes == 0 {
		conf.IPPMaxJobBytes = 1 << 20
	}
	if conf.IPPMaxInflight == 0 {
		conf.IPPMaxInflight = 4
	}
	conf.OpTimeoutMax = "5s"
	h := &harness{
		t: t, store: store, fcas: fs, ix: &memIndex{shas: map[string]blob.ShaRow{}},
		bus:   make(chan *event.Envelope, 16),
		zones: map[string]string{"dripl.example": tenant, "other.example": "other", "quiet.example": "quiet", "noprinter.example": "noprinter", "broke.example": "suspended"},
		subs:  map[string]bool{tenant: true, "other": true, "noprinter": true, "suspended": true},
		ids:   authntest.NewSQLiteStore(t),
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Bus: h.bus, Admission: fakeAdmission{suspended: "suspended"}}
	ctx, cancel := context.WithCancel(context.Background())
	c := NewController(ctx, pu, store, nil)
	c.SetFileCAS(fs)
	c.SetBlobIndex(h.ix)
	c.SetAuth(authn.NewResolver(h.ids, authn.ResolverConfig{Rate: conf.LoginRate, TenantID: authntest.SameTenantID}))
	// Who may sign in, and to what. `quiet` has a printer but no `_ipp`
	// stack; `noprinter` has the stack and no printer.
	h.grant(tenant, principal, username, password, "ipp:*:*")
	h.grant(tenant, "pony:lyon", lyonUsername, lyonPassword, "ipp:*:*")
	h.grant("other", principal, username, otherPassword, "ipp:*:*")
	h.grant("suspended", principal, username, password, "ipp:*:*")
	for _, label := range []string{"research", "expenses", "paris"} {
		h.printer(tenant, label, principal, "")
	}
	h.printer(tenant, "lyon", "pony:lyon", "Lyon")
	h.printer("other", "research", principal, "")
	h.printer("other", "paris", principal, "")
	h.printer("quiet", "research", principal, "")
	h.printer("suspended", "research", principal, "")
	c.tenantFor = func(_ context.Context, x string) (string, string, bool, error) {
		slug, ok := h.zones[x]
		return slug, "zone:" + x, ok, nil
	}
	c.subscribed = func(_ context.Context, slug string) (bool, error) { return h.subs[slug], nil }
	c.pollInterval = 20 * time.Millisecond
	c.dispatchTimeout = 200 * time.Millisecond
	h.ctrl = c

	// The "bus": accepts envelopes and NEVER answers them. If anything in the
	// head waited for a run's reply, these tests would hang or fail.
	go func() {
		for env := range h.bus {
			who, _ := authn.AuthenticatedFrom(env.Ctx)
			h.mu.Lock()
			h.seen = append(h.seen, env.Payload.Raw)
			h.who = append(h.who, who)
			h.mu.Unlock()
		}
	}()
	h.srv = httptest.NewTLSServer(c)
	h.plain = httptest.NewServer(c)
	t.Cleanup(func() {
		h.srv.Close()
		h.plain.Close()
		c.Stop()
		cancel()
		_ = store.Close()
	})
	return h
}

func (h *harness) envelopes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// pinned is whom each delivered run was pinned to, in delivery order; the
// zero Authenticated for a run with no principal.
func (h *harness) pinned() []authn.Authenticated {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]authn.Authenticated(nil), h.who...)
}

// grant makes user sign in to tenant as principal with pass.
func (h *harness) grant(tenant, principal, user, pass string, scopes ...string) {
	h.t.Helper()
	p, err := authn.ParsePrincipal(principal)
	if err != nil {
		h.t.Fatal(err)
	}
	authntest.Grant(h.t, h.ids, tenant, p, user, pass, scopes...)
}

// printer registers (or changes) a printer row.
func (h *harness) printer(tenant, label, principal, display string) {
	h.t.Helper()
	if _, _, err := h.store.UpsertPrinter(context.Background(), chipp.Printer{
		Tenant: tenant, Label: label, PrincipalID: principal, DisplayName: display, CreatedBy: "web",
	}); err != nil {
		h.t.Fatal(err)
	}
}

// credentialID is the id of the one credential principal holds in tenant.
func (h *harness) credentialID(tenant, principal string) string {
	h.t.Helper()
	var id string
	if err := h.ids.DB.QueryRow(`SELECT id FROM credentials WHERE tenant_id = ? AND principal_id = ?`, tenant, principal).Scan(&id); err != nil {
		h.t.Fatal(err)
	}
	return id
}

// request is one IPP operation as a client sends it.
type request struct {
	host, path string
	op         goipp.Op
	opAttrs    []goipp.Attribute
	jobAttrs   []goipp.Attribute
	doc        []byte
	user, pass string
	noAuth     bool
	plain      bool
	contentLen bool // send a Content-Length (default: chunked, like CUPS)
	skipHeader bool // omit charset/language (a malformed request)
}

func (h *harness) do(rq request) (*http.Response, *goipp.Message) {
	h.t.Helper()
	if rq.host == "" {
		rq.host = zoneHost
	}
	if rq.path == "" {
		rq.path = "/p/research"
	}
	msg := goipp.NewRequest(goipp.MakeVersion(2, 0), rq.op, 7)
	if !rq.skipHeader {
		msg.Operation.Add(goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")))
		msg.Operation.Add(goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")))
	}
	msg.Operation.Add(goipp.MakeAttribute("printer-uri", goipp.TagURI, goipp.String("ipps://"+rq.host+rq.path)))
	for _, a := range rq.opAttrs {
		msg.Operation.Add(a)
	}
	for _, a := range rq.jobAttrs {
		msg.Job.Add(a)
	}
	head, err := msg.EncodeBytes()
	if err != nil {
		h.t.Fatal(err)
	}
	wire := append(head, rq.doc...)

	srv := h.srv
	if rq.plain {
		srv = h.plain
	}
	// As CUPS does it: an operation with no document carries a
	// Content-Length; a document is sent chunked (unless the test asks).
	var body io.Reader = bytes.NewReader(wire)
	if len(rq.doc) > 0 && !rq.contentLen {
		body = struct{ io.Reader }{bytes.NewReader(wire)} // hide Len(): chunked
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+rq.path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Host = rq.host
	req.Header.Set("Content-Type", chipp.ContentType)
	if !rq.noAuth {
		user, pass := rq.user, rq.pass
		if user == "" {
			user = username
		}
		if pass == "" {
			pass = password
		}
		req.SetBasicAuth(user, pass)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), chipp.ContentType) {
		return resp, nil
	}
	out := &goipp.Message{}
	if err := out.DecodeBytes(raw); err != nil {
		h.t.Fatalf("decode response: %v", err)
	}
	return resp, out
}

func str(name string, tag goipp.Tag, v string) goipp.Attribute {
	return goipp.MakeAttribute(name, tag, goipp.String(v))
}

func pdfFormat() goipp.Attribute {
	return str("document-format", goipp.TagMimeType, "application/pdf")
}

func status(m *goipp.Message) goipp.Status { return goipp.Status(m.Code) }

func attr(attrs goipp.Attributes, name string) (goipp.Attribute, bool) {
	for _, a := range attrs {
		if a.Name == name {
			return a, true
		}
	}
	return goipp.Attribute{}, false
}

func intOf(t *testing.T, attrs goipp.Attributes, name string) int {
	t.Helper()
	a, ok := attr(attrs, name)
	if !ok {
		t.Fatalf("%s missing from %v", name, attrs)
	}
	return int(a.Values[0].V.(goipp.Integer))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var pdfDoc = append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("page content \n"), 2000)...)

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// --- tests ----------------------------------------------------------------------

// Every reason a printer "is not there" gives ONE identical answer, before
// the body is read: nothing about which tenants exist, which run an `_ipp`
// stack, or what they run leaks to a prober. (Whether a LABEL is registered
// does: see site.)
func TestNotFoundIsOneAnswer(t *testing.T) {
	h := newHarness(t, config.Config{})
	if _, _, err := h.store.UpsertPrinter(context.Background(), chipp.Printer{
		Tenant: tenant, Label: "retired", PrincipalID: principal, Status: chipp.PrinterDisabled,
	}); err != nil {
		t.Fatal(err)
	}
	var first string
	for name, rq := range map[string]request{
		"unknown zone":             {host: "ipp.nobody.example"},
		"subdomain of a real zone": {host: "ipp.sub.dripl.example"},
		"tenant without _ipp":      {host: "ipp.quiet.example"},
		"tenant without a printer": {host: "ipp.noprinter.example"},
		"unregistered label":       {path: "/p/nosuch"},
		"disabled printer":         {path: "/p/retired"},
		"not a printer path":       {path: "/printers/research"},
		"bad printer label":        {path: "/p/Research_Pony!"},
		"nested path":              {path: "/p/research/extra"},
	} {
		rq.op = goipp.OpGetPrinterAttributes
		resp, _ := h.do(rq)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404", name, resp.StatusCode)
		}
		sig := resp.Status + "|" + resp.Header.Get("Content-Type") + "|" + resp.Header.Get("WWW-Authenticate")
		if first == "" {
			first = sig
		} else if sig != first {
			t.Errorf("%s answers differently: %q vs %q", name, sig, first)
		}
	}
	if len(h.envelopes()) != 0 {
		t.Fatal("a not-found request reached the bus")
	}
}

// A browser at a printer's address gets the printer page: the label, and
// the three fields macOS's Add Printer › IP tab asks for, with the port
// spelled out (the Host behind the edge carries none, and IPP's default is
// 631). It is served only where an IPP request would get its 401 — a
// printer that does not exist is the same 404 as ever — and it carries no
// credential and nothing the URL does not already say.
func TestPrinterPage(t *testing.T) {
	h := newHarness(t, config.Config{StructuredHostSuffix: ".stacks.example"})
	h.zones["core-abc123.stacks.example"] = tenant

	get := func(method, host, path string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(method, h.srv.URL+path, nil)
		req.Host = host
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	for _, c := range []struct{ host, path, uri, address, queue string }{
		{zoneHost, "/p/research", "ipps://ipp.dripl.example:443/p/research", "ipp.dripl.example:443", "p/research"},
		{zoneHost + ":8443", "/p/research", "ipps://ipp.dripl.example:8443/p/research", "ipp.dripl.example:8443", "p/research"},
		{"ipp.stacks.example", "/p/core-abc123/paris", "ipps://ipp.stacks.example:443/p/core-abc123/paris", "ipp.stacks.example:443", "p/core-abc123/paris"},
	} {
		resp, body := get(http.MethodGet, c.host, c.path)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("GET %s%s: %d %q", c.host, c.path, resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		for _, want := range []string{"thanks, c", c.uri, c.address, c.queue} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s%s: page lacks %q", c.host, c.path, want)
			}
		}
		if strings.Contains(body, password) || strings.Contains(body, username) {
			t.Fatalf("GET %s%s: the page carries a credential", c.host, c.path)
		}
	}

	// Prod's shape: TLS ends at the edge, which forwards plain HTTP with
	// X-Forwarded-Proto and a Host that names no port. Still ipps, :443.
	req, _ := http.NewRequest(http.MethodGet, h.plain.URL+"/p/core-abc123/paris", nil)
	req.Host = "ipp.stacks.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	edge, err := h.plain.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(edge.Body)
	edge.Body.Close()
	if edge.StatusCode != http.StatusOK || !strings.Contains(string(b), "ipps://ipp.stacks.example:443/p/core-abc123/paris") {
		t.Fatalf("behind the edge: %d, page lacks the ipps :443 address", edge.StatusCode)
	}
	// …and a truly plaintext request (dev's :8080) is honest about it.
	req, _ = http.NewRequest(http.MethodGet, h.plain.URL+"/p/research", nil)
	req.Host = zoneHost + ":8080"
	plain, err := h.plain.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(plain.Body)
	plain.Body.Close()
	if !strings.Contains(string(b), "ipp://ipp.dripl.example:8080/p/research") || strings.Contains(string(b), "ipps://") {
		t.Fatal("plaintext request: want the ipp:// :8080 address and no ipps")
	}

	resp, body := get(http.MethodHead, zoneHost, "/p/research")
	if resp.StatusCode != http.StatusOK || body != "" || resp.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD: %d, %d body bytes, Content-Length %q", resp.StatusCode, len(body), resp.Header.Get("Content-Length"))
	}

	// No printer, no page: the one 404, whatever the reason.
	for _, c := range []struct{ host, path string }{
		{"ipp.quiet.example", "/p/research"},     // a tenant without an _ipp stack
		{"ipp.noprinter.example", "/p/research"}, // …without a printer
		{zoneHost, "/p/nosuch"},                  // a label nobody registered
		{"ipp.nowhere.example", "/p/research"},
		{zoneHost, "/p/Not_A_Label"},
	} {
		if resp, _ := get(http.MethodGet, c.host, c.path); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s%s: %d, want 404", c.host, c.path, resp.StatusCode)
		}
	}
	if len(h.envelopes()) != 0 {
		t.Fatal("a page view reached the bus")
	}

	// A registered display name is the page's heading; without one it is the
	// label. Either way the fields below it are the URL's.
	_, body = get(http.MethodGet, zoneHost, "/p/lyon")
	if !strings.Contains(body, "Lyon") {
		t.Error("the page does not show the display name")
	}
	if !strings.Contains(body, "p/lyon") {
		t.Error("the display name replaced the queue, which is the label")
	}
}

func TestTransportGates(t *testing.T) {
	h := newHarness(t, config.Config{})

	// Neither IPP nor a browser: a printer answers GET (its page), HEAD and
	// POST; a job only POST.
	for _, c := range []struct{ method, path, allow string }{
		{http.MethodPut, "/p/research", "GET, HEAD, POST"},
		{http.MethodGet, "/p/research/jobs/1", http.MethodPost},
	} {
		req, _ := http.NewRequest(c.method, h.srv.URL+c.path, nil)
		req.Host = zoneHost
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != c.allow {
			t.Fatalf("%s %s: %d Allow=%q, want 405 Allow=%q", c.method, c.path, resp.StatusCode, resp.Header.Get("Allow"), c.allow)
		}
	}

	// Wrong content type.
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/p/research", strings.NewReader("{}"))
	req.Host = zoneHost
	req.Header.Set("Content-Type", "application/json")
	resp, _ := h.srv.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("json body: %d", resp.StatusCode)
	}

	// Garbage claiming to be IPP.
	req, _ = http.NewRequest(http.MethodPost, h.srv.URL+"/p/research", strings.NewReader("not ipp at all"))
	req.Host = zoneHost
	req.Header.Set("Content-Type", chipp.ContentType)
	resp, _ = h.srv.Client().Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("garbage: %d", resp.StatusCode)
	}

	// charset/language must open the request.
	if _, m := h.do(request{op: goipp.OpGetJobs, skipHeader: true}); m == nil || status(m) != goipp.StatusErrorBadRequest {
		t.Fatalf("missing charset: %v", m)
	}
	// An operation this printer does not have.
	if _, m := h.do(request{op: goipp.OpPausePrinter}); m == nil || status(m) != goipp.StatusErrorOperationNotSupported {
		t.Fatalf("Pause-Printer: %v", m)
	}
}

func TestAuthentication(t *testing.T) {
	h := newHarness(t, config.Config{})

	// No credential → a Basic challenge (and only for a non-anonymous op).
	resp, _ := h.do(request{op: goipp.OpGetJobs, noAuth: true})
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `realm="ipp"`) {
		t.Fatalf("no creds: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// Every way of not being this printer's principal is one 401.
	h.grant(tenant, "pony:mailonly", "mailonly@dripl.example", "rstv-opens-mail-and-nothing-else", "imap:*:*")
	h.grant(tenant, "pony:oneprinter", "oneprinter@dripl.example", "vwxz-prints-to-expenses-only", "ipp:expenses:print")
	h.printer(tenant, "mailonly", "pony:mailonly", "")
	for name, rq := range map[string]request{
		"wrong password":          {pass: "bcdf-nope"},
		"a password with no id":   {pass: "nope"},
		"other tenant's password": {pass: otherPassword},
		// A valid login — for somebody else. The printer is granted to one
		// principal; another's password is a wrong password here.
		"another principal":     {user: lyonUsername, pass: lyonPassword},
		"no ipp scope":          {path: "/p/mailonly", user: "mailonly@dripl.example", pass: "rstv-opens-mail-and-nothing-else"},
		"scope for another one": {user: "oneprinter@dripl.example", pass: "vwxz-prints-to-expenses-only"},
	} {
		rq.op = goipp.OpGetJobs
		resp, _ := h.do(rq)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s: %d, want a 401 challenge", name, resp.StatusCode)
		}
	}
	// The right one works, on its own printer: each principal's.
	if _, m := h.do(request{op: goipp.OpGetJobs}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("valid credential refused: %v", m)
	}
	// WHATEVER THE USERNAME. The printer's row names the principal, so the
	// name a print client sends is decoration — a person typing an address
	// into a phone's print sheet is where a print gets abandoned. The
	// password still decides, and it is still this printer's principal's.
	for name, user := range map[string]string{
		"no username at all":        "",
		"the printer's own label":   "research",
		"the pre-principal default": "print",
		"somebody else entirely":    "nobody@dripl.example",
		"another principal's name":  lyonUsername,
	} {
		if _, m := h.do(request{op: goipp.OpGetJobs, user: user}); m == nil || status(m) != goipp.StatusOk {
			t.Errorf("%s: refused, although the password is this printer's: %v", name, m)
		}
	}
	// And a name alone opens nothing: the other principal's own password is
	// still refused here, whatever it calls itself.
	if resp, _ := h.do(request{op: goipp.OpGetJobs, user: "research", pass: lyonPassword}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("another principal's password with this printer's name: %d, want 401", resp.StatusCode)
	}
	if _, m := h.do(request{op: goipp.OpGetJobs, path: "/p/lyon", user: lyonUsername, pass: lyonPassword}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("lyon refused at its own printer: %v", m)
	}
	// A scope naming one printer opens that printer once it is granted.
	h.printer(tenant, "expenses", "pony:oneprinter", "")
	if _, m := h.do(request{op: goipp.OpGetJobs, path: "/p/expenses", user: "oneprinter@dripl.example", pass: "vwxz-prints-to-expenses-only"}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("a printer-scoped credential refused at its printer: %v", m)
	}

	// Revocation is immediate: the login was verified and cached a moment
	// ago, and the next request is refused all the same.
	if _, err := h.ids.DB.Exec(`UPDATE credentials SET revoked_at = '2026-09-19T00:00:00Z' WHERE tenant_id = ? AND principal_id = ?`, tenant, "pony:lyon"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.do(request{op: goipp.OpGetJobs, path: "/p/lyon", user: lyonUsername, pass: lyonPassword}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a revoked credential still prints: %d", resp.StatusCode)
	}
	// Re-pointing the printer moves the grant with it, at once.
	h.printer(tenant, "lyon", principal, "")
	if _, m := h.do(request{op: goipp.OpGetJobs, path: "/p/lyon"}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("the printer's new principal refused: %v", m)
	}

	// Plaintext is refused BEFORE a credential is read.
	if resp, _ := h.do(request{op: goipp.OpGetJobs, plain: true}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("plaintext auth: %d, want 403", resp.StatusCode)
	}
	// Admission: a suspended tenant gets its own status, after a valid login.
	resp, _ = h.do(request{op: goipp.OpGetJobs, host: "ipp.broke.example"})
	if resp.StatusCode != http.StatusPaymentRequired || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("suspended tenant: %d Retry-After=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// Guessing is capped — and the cap binds the correct guess too, which is the
// point of counting every cache MISS rather than every failure. The budget
// is the chassis's one login budget (--login-rate), shared with the other
// heads.
func TestAuthThrottle(t *testing.T) {
	h := newHarness(t, config.Config{LoginRate: 3})
	for i := 0; i < 3; i++ {
		if resp, _ := h.do(request{op: goipp.OpGetJobs, pass: "bcdf-guess"}); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := h.do(request{op: goipp.OpGetJobs}) // the right password, too late
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("over the limit: %d", resp.StatusCode)
	}
}

// A legitimate client re-authenticates on every operation and must never
// throttle itself: only its first request is a cache miss.
func TestVerifiedLoginIsNotThrottled(t *testing.T) {
	h := newHarness(t, config.Config{LoginRate: 2})
	for i := 0; i < 10; i++ {
		if _, m := h.do(request{op: goipp.OpGetJobs}); m == nil || status(m) != goipp.StatusOk {
			t.Fatalf("request %d throttled", i)
		}
	}
}

// tripwire is a request body that must never be touched.
type tripwire struct {
	t    *testing.T
	what string
}

func (b tripwire) Read([]byte) (int, error) {
	b.t.Errorf("%s: the request body was read before the request was authenticated", b.what)
	return 0, io.EOF
}
func (tripwire) Close() error { return nil }

// WHO BEFORE WHAT. Reading the body is what makes net/http send
// `100 Continue`, and a print client answers that by streaming its document;
// a 401 sent afterwards lands mid-upload, the connection resets, and CUPS
// retries the entire request without credentials, forever (an ipptool did
// ~50,000 Print-Jobs in two minutes against the first cut of this head).
// So a request that will be refused for WHO it is must be refused from its
// headers alone — not one body byte read.
func TestRefusalsNeverTouchTheBody(t *testing.T) {
	h := newHarness(t, config.Config{})
	serve := func(name string, mutate func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "https://"+zoneHost+"/p/research", nil)
		req.Host = zoneHost
		req.Header.Set("Content-Type", chipp.ContentType)
		req.Header.Set("Expect", "100-continue")
		req.ContentLength = -1 // chunked: a document is on its way
		req.TransferEncoding = []string{"chunked"}
		req.Body = tripwire{t, name}
		mutate(req)
		rec := httptest.NewRecorder()
		h.ctrl.ServeHTTP(rec, req)
		return rec
	}

	rec := serve("no credential", func(*http.Request) {})
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("no credential: %d", rec.Code)
	}
	rec = serve("wrong password", func(r *http.Request) { r.SetBasicAuth(username, "bcdf-nope") })
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong password: %d", rec.Code)
	}
	// A large body that DOES announce its length is just as much a document.
	rec = serve("content-length document", func(r *http.Request) {
		r.ContentLength = 5 << 20
		r.TransferEncoding = nil
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("content-length document: %d", rec.Code)
	}
	// Plaintext: refused before a credential is even invited.
	rec = serve("plaintext", func(r *http.Request) { r.TLS = nil; r.URL.Scheme = "http" })
	if rec.Code != http.StatusForbidden {
		t.Errorf("plaintext: %d, want 403", rec.Code)
	}
	// Unknown tenant: 404 without reading either.
	rec = serve("unknown zone", func(r *http.Request) { r.Host = "ipp.nobody.example" })
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown zone: %d", rec.Code)
	}
	// With anonymous attributes off, even a small body is not read.
	h.ctrl.anonAttrs = false
	rec = serve("anonymous attributes off", func(r *http.Request) { r.ContentLength = 120; r.TransferEncoding = nil })
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous attributes off: %d", rec.Code)
	}
}

// The other half: a refusal that can only be made AFTER the header is read
// (it depends on the operation) must drain the document first, or the
// client — still sending — reads a reset instead of the answer. Sent over a
// real connection with a document far bigger than any socket buffer.
func TestLateRefusalsStillReachTheClient(t *testing.T) {
	h := newHarness(t, config.Config{IPPMaxJobBytes: 64 << 20})
	big := append([]byte("%!PS-Adobe-3.0\n"), bytes.Repeat([]byte("showpage "), 1<<20)...) // ~9 MiB of PostScript
	for name, rq := range map[string]request{
		"format not supported": {op: goipp.OpPrintJob, doc: big,
			opAttrs: []goipp.Attribute{str("document-format", goipp.TagMimeType, "application/postscript")}},
		"claims pdf, is not": {op: goipp.OpPrintJob, doc: big, opAttrs: []goipp.Attribute{pdfFormat()}},
		"no such job": {op: goipp.OpSendDocument, doc: big,
			opAttrs: []goipp.Attribute{pdfFormat(), goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(999))}},
		"unsupported operation": {op: goipp.OpPausePrinter, doc: big},
	} {
		_, m := h.do(rq)
		if m == nil {
			t.Errorf("%s: the client got no IPP answer", name)
			continue
		}
		if st := status(m); st == goipp.StatusOk {
			t.Errorf("%s: accepted (%v)", name, st)
		}
	}
	if ok, _ := h.fcas.Exists(context.Background(), shaOf(big)); ok {
		t.Fatal("a refused document reached the CAS")
	}
}

func TestGetPrinterAttributes(t *testing.T) {
	h := newHarness(t, config.Config{})

	// Anonymous: a client adding the printer has no password yet.
	_, m := h.do(request{op: goipp.OpGetPrinterAttributes, noAuth: true})
	if m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("anonymous Get-Printer-Attributes: %v", m)
	}
	uri, _ := attr(m.Printer, "printer-uri-supported")
	if got := uri.Values[0].V.String(); got != "ipps://"+zoneHost+"/p/research" {
		t.Fatalf("printer-uri-supported = %q", got)
	}
	name, _ := attr(m.Printer, "printer-name")
	if name.Values[0].V.String() != "research" {
		t.Fatalf("printer-name: %v", name.Values)
	}
	if a, _ := attr(m.Printer, "document-format-supported"); len(a.Values) != 1 || a.Values[0].V.String() != "application/pdf" {
		t.Fatalf("formats: %v", a.Values)
	}
	if a, _ := attr(m.Printer, "uri-authentication-supported"); a.Values[0].V.String() != "basic" {
		t.Fatalf("auth: %v", a.Values)
	}

	// No display name, no printer-dns-sd-name; with one, it is what a client
	// that adds the printer by URI names its queue — so it is in the
	// anonymous answer, which is the one that client reads.
	if _, ok := attr(m.Printer, "printer-dns-sd-name"); ok {
		t.Fatal("printer-dns-sd-name sent for a printer with no display name")
	}
	_, m = h.do(request{op: goipp.OpGetPrinterAttributes, noAuth: true, path: "/p/lyon"})
	if a, ok := attr(m.Printer, "printer-dns-sd-name"); !ok || a.Values[0].V.String() != "Lyon" {
		t.Fatalf("printer-dns-sd-name: %v %v", ok, a.Values)
	}
	if a, _ := attr(m.Printer, "printer-info"); a.Values[0].V.String() != "Lyon" {
		t.Fatalf("printer-info: %v", a.Values)
	}
	if a, _ := attr(m.Printer, "printer-name"); a.Values[0].V.String() != "lyon" {
		t.Fatalf("printer-name must stay the label: %v", a.Values)
	}

	// requested-attributes narrows the answer.
	_, m = h.do(request{op: goipp.OpGetPrinterAttributes, noAuth: true, opAttrs: []goipp.Attribute{
		goipp.MakeAttr("requested-attributes", goipp.TagKeyword, goipp.String("printer-state"), goipp.String("copies-supported"))}})
	if len(m.Printer) != 2 {
		t.Fatalf("requested two attributes, got %v", m.Printer)
	}

	// With the toggle off the same request is challenged.
	h.ctrl.anonAttrs = false
	if resp, _ := h.do(request{op: goipp.OpGetPrinterAttributes, noAuth: true}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous attributes disabled: %d", resp.StatusCode)
	}
	if _, m := h.do(request{op: goipp.OpGetPrinterAttributes}); m == nil || status(m) != goipp.StatusOk {
		t.Fatal("authenticated Get-Printer-Attributes refused")
	}
}

func TestValidateJob(t *testing.T) {
	h := newHarness(t, config.Config{})
	if _, m := h.do(request{op: goipp.OpValidateJob, opAttrs: []goipp.Attribute{pdfFormat()}}); status(m) != goipp.StatusOk {
		t.Fatalf("pdf: %v", status(m))
	}
	_, m := h.do(request{op: goipp.OpValidateJob, opAttrs: []goipp.Attribute{str("document-format", goipp.TagMimeType, "application/postscript")}})
	if status(m) != goipp.StatusErrorDocumentFormatNotSupported {
		t.Fatalf("postscript: %v", status(m))
	}
	_, m = h.do(request{op: goipp.OpValidateJob, opAttrs: []goipp.Attribute{pdfFormat(), str("compression", goipp.TagKeyword, "gzip")}})
	if status(m) != goipp.StatusErrorCompressionNotSupported {
		t.Fatalf("gzip: %v", status(m))
	}
}

// The whole point, end to end: a document of unknown hash streams into the
// CAS, the tenant owns it, the job is durable, the client is answered — and
// then the envelope reaches the bus and the job is COMPLETED although the
// "stack" never answers.
func TestPrintJobEndToEnd(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.ctrl.Start()

	_, m := h.do(request{
		op: goipp.OpPrintJob,
		opAttrs: []goipp.Attribute{pdfFormat(),
			str("job-name", goipp.TagName, "Quarterly report"),
			str("document-name", goipp.TagName, "report.pdf"),
			str("requesting-user-name", goipp.TagName, "matt")},
		// 17 copies, duplex, A4: properties of paper. Accepted, and ignored OUT LOUD.
		jobAttrs: []goipp.Attribute{
			goipp.MakeAttribute("copies", goipp.TagInteger, goipp.Integer(17)),
			str("sides", goipp.TagKeyword, "two-sided-long-edge"),
			str("media", goipp.TagKeyword, "iso_a4_210x297mm")},
		doc: pdfDoc,
	})
	if m == nil || status(m) != goipp.StatusOkIgnoredOrSubstituted {
		t.Fatalf("Print-Job status: %v", m)
	}
	if len(m.Unsupported) != 3 {
		t.Fatalf("ignored attributes: %v", m.Unsupported)
	}
	jobNo := intOf(t, m.Job, "job-id")
	if jobNo != 1 {
		t.Fatalf("job-id = %d", jobNo)
	}
	if u, _ := attr(m.Job, "job-uri"); u.Values[0].V.String() != "ipps://"+zoneHost+"/p/research/jobs/1" {
		t.Fatalf("job-uri: %v", u.Values)
	}

	// Bytes: in the CAS, byte-identical, owned by THIS tenant and nobody else.
	sha := shaOf(pdfDoc)
	got, err := h.fcas.Get(context.Background(), sha)
	if err != nil || !bytes.Equal(got, pdfDoc) {
		t.Fatalf("CAS content: err=%v equal=%v", err, bytes.Equal(got, pdfDoc))
	}
	if row, ok, _ := h.ix.GetSha(context.Background(), tenant, sha); !ok || row.Size != int64(len(pdfDoc)) || row.ContentType != "application/pdf" {
		t.Fatalf("ownership row: %+v %v", row, ok)
	}
	if _, ok, _ := h.ix.GetSha(context.Background(), "other", sha); ok {
		t.Fatal("another tenant owns the document")
	}

	// Delivery: the bus takes the envelope, nobody answers it, and the job is
	// completed anyway.
	waitFor(t, "delivery", func() bool { return len(h.envelopes()) == 1 })
	env := h.envelopes()[0]
	for path, want := range map[string]string{
		"_txc.src":                 "ipp",
		"_txc.ipp.tenant":          tenant,
		"_txc.ipp.printer":         "research",
		"_txc.ipp.host":            zoneHost,
		"_txc.ipp.printer_uri":     "ipps://" + zoneHost + "/p/research",
		"_txc.ipp.job_name":        "Quarterly report",
		"_txc.ipp.requesting_user": "matt",
		"_txc.ipp.document.sha256": sha,
		"_txc.ipp.document.format": "application/pdf",
		"_txc.ipp.document.name":   "report.pdf",
	} {
		if got := gjson.Get(env, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if gjson.Get(env, "_txc.ipp.document.size").Int() != int64(len(pdfDoc)) || gjson.Get(env, "_txc.ipp.job_number").Int() != 1 {
		t.Errorf("size/job_number: %s", env)
	}
	if !strings.HasPrefix(gjson.Get(env, "_txc.ipp.job_id").String(), "ipj_") || gjson.Get(env, "_txc.rid").String() == "" {
		t.Errorf("job_id/rid: %s", env)
	}
	// Paper never reaches the stack; neither do the bytes; and the head does
	// not route — detect-tenant does, from the trusted tenant stamp.
	for _, path := range []string{"_txc.ipp.copies", "_txc.ipp.media", "_txc.ipp.sides", "_txc.ipp.attrs", "_txc.route", "_txc.ipp.document.content"} {
		if gjson.Get(env, path).Exists() {
			t.Errorf("%s must not be in the envelope", path)
		}
	}
	if len(env) > 2048 {
		t.Errorf("envelope is %d bytes for a %d byte document", len(env), len(pdfDoc))
	}
	// WHO printed: pinned on the run's context (the processor stamps
	// `_txc.principal` from it at every run entry), never written into the
	// envelope by the head, where a stack could not tell it from a claim.
	cred := h.credentialID(tenant, principal)
	if who := h.pinned()[0]; who.Principal.ID != principal || who.Credential != cred {
		t.Errorf("the run is pinned to %+v, want %s / %s", who, principal, cred)
	}
	if gjson.Get(env, "_txc.principal").Exists() {
		t.Error("the head wrote _txc.principal into the envelope")
	}

	waitFor(t, "job completed", func() bool {
		_, m := h.do(request{op: goipp.OpGetJobAttributes, opAttrs: []goipp.Attribute{goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobNo))}})
		return m != nil && status(m) == goipp.StatusOk && intOf(t, m.Job, "job-state") == 9
	})
	j, _ := h.store.GetJob(context.Background(), tenant, "research", 1)
	if j.State != chipp.StateDelivered || j.Rid == "" || j.PrincipalID != principal || j.CredentialID != cred {
		t.Fatalf("stored job: %+v", j)
	}

	// Get-Jobs: nothing active, one completed.
	_, m = h.do(request{op: goipp.OpGetJobs})
	if len(m.Groups) != 1 { // operation group only
		t.Fatalf("active jobs: %d groups", len(m.Groups))
	}
	_, m = h.do(request{op: goipp.OpGetJobs, opAttrs: []goipp.Attribute{str("which-jobs", goipp.TagKeyword, "completed")}})
	if len(m.Groups) != 2 || intOf(t, m.Groups[1].Attrs, "job-id") != 1 {
		t.Fatalf("completed jobs: %v", m.Groups)
	}
	// Delivered is final: there is nothing left to cancel.
	_, m = h.do(request{op: goipp.OpCancelJob, opAttrs: []goipp.Attribute{goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(1))}})
	if status(m) != goipp.StatusErrorNotPossible {
		t.Fatalf("cancel a delivered job: %v", status(m))
	}
}

func TestDocumentRefusals(t *testing.T) {
	h := newHarness(t, config.Config{IPPMaxJobBytes: 4096})
	count := func() int {
		a, _ := h.store.ListJobs(context.Background(), tenant, "research", false, 0)
		return len(a)
	}

	// Claims PDF, is PostScript: refused, nothing stored.
	ps := []byte("%!PS-Adobe-3.0\n/Times-Roman findfont\n")
	_, m := h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: ps})
	if status(m) != goipp.StatusErrorDocumentFormatError {
		t.Fatalf("postscript as pdf: %v", status(m))
	}
	if ok, _ := h.fcas.Exists(context.Background(), shaOf(ps)); ok {
		t.Fatal("a refused document reached the CAS")
	}
	// Declares PostScript honestly: refused before a byte is read.
	_, m = h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{str("document-format", goipp.TagMimeType, "application/postscript")}, doc: ps})
	if status(m) != goipp.StatusErrorDocumentFormatNotSupported {
		t.Fatalf("postscript: %v", status(m))
	}
	// No document at all.
	_, m = h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}})
	if status(m) != goipp.StatusErrorDocumentFormatError {
		t.Fatalf("empty document: %v", status(m))
	}

	// Too large, both ways a client can send it.
	big := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 8192)...)
	_, m = h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: big}) // chunked
	if m == nil || status(m) != goipp.StatusErrorRequestEntity {
		t.Fatalf("oversize (chunked): %v", m)
	}
	huge := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("x"), 80<<10)...)
	_, m = h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: huge, contentLen: true}) // refused pre-read
	if m == nil || status(m) != goipp.StatusErrorRequestEntity {
		t.Fatalf("oversize (content-length): %v", m)
	}
	for _, d := range [][]byte{big, huge} {
		if ok, _ := h.fcas.Exists(context.Background(), shaOf(d)); ok {
			t.Fatal("an oversize document reached the CAS")
		}
	}
	// None of the refusals left a live job behind, and none reached the bus.
	if n := count(); n != 0 {
		t.Fatalf("%d live jobs after refusals", n)
	}
	if len(h.envelopes()) != 0 {
		t.Fatal("a refused job reached the bus")
	}
	// Exactly at the limit is fine.
	exact := append([]byte("%PDF-1.7\n"), bytes.Repeat([]byte("y"), 4096-9)...)
	if _, m = h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: exact}); status(m) != goipp.StatusOk {
		t.Fatalf("document of exactly the limit: %v", status(m))
	}
}

func TestStorageFailuresAreTemporary(t *testing.T) {
	h := newHarness(t, config.Config{})
	h.ctrl.SetFileCAS(failingCAS{h.fcas})
	_, m := h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: pdfDoc})
	if status(m) != goipp.StatusErrorTemporary {
		t.Fatalf("CAS down: %v", status(m))
	}
	h.ctrl.SetFileCAS(h.fcas)
	h.ix.fail = errors.New("kv unavailable")
	_, m = h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: pdfDoc})
	if status(m) != goipp.StatusErrorTemporary {
		t.Fatalf("index down: %v", status(m))
	}
	h.ix.fail = nil
	// Both jobs are terminal `failed`; neither is deliverable.
	done, _ := h.store.ListJobs(context.Background(), tenant, "research", true, 0)
	if len(done) != 2 || done[0].State != chipp.StateFailed || done[1].State != chipp.StateFailed {
		t.Fatalf("failed jobs: %+v", done)
	}
}

func TestCreateJobSendDocumentAndCancel(t *testing.T) {
	h := newHarness(t, config.Config{})
	jobID := func(n int) []goipp.Attribute {
		return []goipp.Attribute{goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(n))}
	}

	_, m := h.do(request{op: goipp.OpCreateJob, opAttrs: []goipp.Attribute{str("job-name", goipp.TagName, "two-step")}})
	if status(m) != goipp.StatusOk || intOf(t, m.Job, "job-state") != 3 {
		t.Fatalf("Create-Job: %v", m)
	}
	n := intOf(t, m.Job, "job-id")

	// Send-Document by job-id…
	_, m = h.do(request{op: goipp.OpSendDocument, doc: pdfDoc,
		opAttrs: append(jobID(n), pdfFormat(), goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(true)))})
	if status(m) != goipp.StatusOk || intOf(t, m.Job, "job-state") != 5 {
		t.Fatalf("Send-Document: %v", m)
	}
	// …and one document per job.
	_, m = h.do(request{op: goipp.OpSendDocument, doc: pdfDoc, opAttrs: append(jobID(n), pdfFormat())})
	if status(m) != goipp.StatusErrorMultipleJobsNotSupported {
		t.Fatalf("second document: %v", status(m))
	}
	// The job URL works as an address too.
	_, m = h.do(request{op: goipp.OpGetJobAttributes, path: "/p/research/jobs/1"})
	if status(m) != goipp.StatusOk || intOf(t, m.Job, "job-id") != 1 {
		t.Fatalf("job by URL: %v", m)
	}

	// Cancel a job that never got its document.
	_, m = h.do(request{op: goipp.OpCreateJob})
	second := intOf(t, m.Job, "job-id")
	if _, m = h.do(request{op: goipp.OpCancelJob, opAttrs: jobID(second)}); status(m) != goipp.StatusOk {
		t.Fatalf("cancel: %v", status(m))
	}
	if _, m = h.do(request{op: goipp.OpCancelJob, opAttrs: jobID(second)}); status(m) != goipp.StatusOk {
		t.Fatalf("cancel twice: %v", status(m))
	}
	if _, m = h.do(request{op: goipp.OpSendDocument, doc: pdfDoc, opAttrs: append(jobID(second), pdfFormat())}); status(m) != goipp.StatusErrorMultipleJobsNotSupported {
		t.Fatalf("document into a canceled job: %v", status(m))
	}
	// Unknown job; and a job is invisible from another printer / tenant.
	if _, m = h.do(request{op: goipp.OpGetJobAttributes, opAttrs: jobID(999)}); status(m) != goipp.StatusErrorNotFound {
		t.Fatalf("unknown job: %v", status(m))
	}
	if _, m = h.do(request{op: goipp.OpGetJobAttributes, path: "/p/expenses", opAttrs: jobID(n)}); status(m) != goipp.StatusErrorNotFound {
		t.Fatalf("job seen from another printer: %v", status(m))
	}
	if _, m = h.do(request{op: goipp.OpGetJobAttributes, host: "ipp.other.example", pass: otherPassword, opAttrs: jobID(n)}); status(m) != goipp.StatusErrorNotFound {
		t.Fatalf("job seen from another tenant: %v", status(m))
	}
	// A printer-uri that names a different printer than the URL is refused.
	if _, m = h.do(request{op: goipp.OpGetJobs, opAttrs: []goipp.Attribute{str("job-uri", goipp.TagURI, "ipps://"+zoneHost+"/p/expenses/jobs/1")}}); status(m) != goipp.StatusErrorNotFound {
		t.Fatalf("mismatched job-uri: %v", status(m))
	}
}

func TestBusyAndDraining(t *testing.T) {
	h := newHarness(t, config.Config{IPPMaxInflight: 2})
	for i := 0; i < 2; i++ {
		if _, m := h.do(request{op: goipp.OpCreateJob}); status(m) != goipp.StatusOk {
			t.Fatal("create")
		}
	}
	// Two documents are "in flight" (created, not yet sent): a third is busy.
	if _, m := h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: pdfDoc}); status(m) != goipp.StatusErrorBusy {
		t.Fatalf("over the in-flight cap: %v", status(m))
	}

	admission.SetDraining(true)
	t.Cleanup(func() { admission.SetDraining(false) })
	resp, _ := h.do(request{op: goipp.OpPrintJob, opAttrs: []goipp.Attribute{pdfFormat()}, doc: pdfDoc})
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("draining: %d", resp.StatusCode)
	}
	// Read-only operations still answer while draining.
	if _, m := h.do(request{op: goipp.OpGetJobs}); m == nil || status(m) != goipp.StatusOk {
		t.Fatal("Get-Jobs refused while draining")
	}
}

// A job belongs to whoever created it. Create-Job and Send-Document are two
// requests, so the principal rides the row between them; and a job with no
// principal — one an older build created mid-roll — is still delivered, with
// no one pinned, for the stack to judge.
func TestJobsKeepTheirPrincipal(t *testing.T) {
	h := newHarness(t, config.Config{})
	ctx := context.Background()

	_, m := h.do(request{op: goipp.OpCreateJob, opAttrs: []goipp.Attribute{pdfFormat()}})
	if m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("Create-Job: %v", m)
	}
	n := intOf(t, m.Job, "job-id")
	if j, _ := h.store.GetJob(ctx, tenant, "research", int64(n)); j.PrincipalID != principal {
		t.Fatalf("Create-Job did not record who created it: %+v", j)
	}

	// The printer changes hands while the job waits for its document. Its new
	// principal signs in fine — and may not finish somebody else's job.
	h.printer(tenant, "research", "pony:lyon", "")
	send := request{op: goipp.OpSendDocument, user: lyonUsername, pass: lyonPassword, doc: pdfDoc, opAttrs: []goipp.Attribute{
		goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(n)),
		goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(true)), pdfFormat()}}
	if _, m = h.do(send); m == nil || status(m) != goipp.StatusErrorNotAuthorized {
		t.Fatalf("another principal's Send-Document: %v", m)
	}
	if j, _ := h.store.GetJob(ctx, tenant, "research", int64(n)); j.State != chipp.StateReceiving {
		t.Fatalf("the refused document changed the job: %+v", j)
	}
	// Back in its owner's hands, the owner finishes it.
	h.printer(tenant, "research", principal, "")
	send.user, send.pass = "", ""
	if _, m = h.do(send); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("the owner's Send-Document: %v", m)
	}
	h.ctrl.pass(ctx)
	waitFor(t, "delivery", func() bool { return len(h.pinned()) == 1 })
	if who := h.pinned()[0]; who.Principal.ID != principal {
		t.Fatalf("pinned to %+v, want %s", who, principal)
	}

	// A row with no principal: delivered, pinned to no one.
	old, err := h.store.CreateJob(ctx, chipp.NewJob{Tenant: tenant, Printer: "research", Host: zoneHost})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.store.SetCommitted(ctx, old.ID, shaOf(pdfDoc), int64(len(pdfDoc)), "application/pdf", ""); !ok || err != nil {
		t.Fatal(err)
	}
	h.ctrl.pass(ctx)
	waitFor(t, "the unsigned job's delivery", func() bool { return len(h.pinned()) == 2 })
	if who := h.pinned()[1]; !who.IsZero() {
		t.Fatalf("a job with no principal was pinned to %+v", who)
	}
	// Nor is a principal that does not parse ever guessed at.
	if _, ok := jobPrincipal(chipp.Job{PrincipalID: "not a principal"}); ok {
		t.Fatal("an unparseable principal was pinned")
	}
}

// The dispatcher in isolation: a bus that will not take the envelope hands
// the job back (it never ran); draining leases nothing; and delivery needs
// no reader on the reply channel.
func TestDispatcher(t *testing.T) {
	h := newHarness(t, config.Config{})
	ctx := context.Background()
	commit := func() chipp.Job {
		j, err := h.store.CreateJob(ctx, chipp.NewJob{Tenant: tenant, Printer: "research", Host: zoneHost})
		if err != nil {
			t.Fatal(err)
		}
		if ok, err := h.store.SetCommitted(ctx, j.ID, shaOf(pdfDoc), int64(len(pdfDoc)), "application/pdf", ""); !ok || err != nil {
			t.Fatal(err)
		}
		return j
	}

	// A full bus: the handoff times out and the lease is released.
	blocked := make(chan *event.Envelope) // unbuffered, nobody reads
	realBus := h.ctrl.pu.Bus
	h.ctrl.pu.Bus = blocked
	a := commit()
	h.ctrl.pass(ctx)
	got, _ := h.store.GetJobByID(ctx, a.ID)
	if got.State != chipp.StateCommitted || got.Attempts != 1 {
		t.Fatalf("after a refused handoff: %+v", got)
	}
	if l, _ := h.store.LeaseCommitted(ctx, "probe", 1); len(l) != 1 {
		t.Fatal("the lease was not released")
	}
	_ = h.store.ReleaseLease(ctx, a.ID)

	// Draining: nothing is leased.
	h.ctrl.pu.Bus = realBus
	admission.SetDraining(true)
	h.ctrl.pass(ctx)
	admission.SetDraining(false)
	if got, _ := h.store.GetJobByID(ctx, a.ID); got.State != chipp.StateCommitted {
		t.Fatalf("a draining node delivered: %+v", got)
	}

	// Normal: delivered although the envelope is never answered.
	h.ctrl.pass(ctx)
	if got, _ := h.store.GetJobByID(ctx, a.ID); got.State != chipp.StateDelivered || got.Rid == "" {
		t.Fatalf("not delivered: %+v", got)
	}
	// "Delivered" is the bus TAKING the envelope; the fake bus records it a
	// moment after the receive, so wait for the record rather than race it.
	waitFor(t, "the bus to record the envelope", func() bool { return len(h.envelopes()) == 1 })
}

func TestTargetParsing(t *testing.T) {
	const shared = "stacks.example"
	ok := map[[2]string]target{
		{"ipp.dripl.it", "/p/research"}:           {x: "dripl.it", host: "ipp.dripl.it", base: "/p", printer: "research"},
		{"IPP.Dripl.IT:443", "/p/research/"}:      {x: "dripl.it", host: "ipp.dripl.it", base: "/p", printer: "research"},
		{"ipp.dripl.it", "/p/research/jobs/42"}:   {x: "dripl.it", host: "ipp.dripl.it", base: "/p", printer: "research", job: 42},
		{"ipp.localhost:8443", "/p/a.b-c_1"}:      {x: "localhost", host: "ipp.localhost", base: "/p", printer: "a.b-c_1"},
		{"ipp.pony.acme.example", "/p/summarize"}: {x: "pony.acme.example", host: "ipp.pony.acme.example", base: "/p", printer: "summarize"},
		{"ipp.dripl.it", "/p/jobs"}:               {x: "dripl.it", host: "ipp.dripl.it", base: "/p", printer: "jobs"}, // a printer may be called anything
		// The shared front door: the tenant rides the path as the HANDLE of a
		// hostname it has under the suffix, and that hostname decides the tenant.
		{"ipp.stacks.example", "/p/core-hmhzx2isby/paris"}: {
			x: "core-hmhzx2isby.stacks.example", host: "ipp.stacks.example", handle: "core-hmhzx2isby", base: "/p/core-hmhzx2isby", printer: "paris"},
		{"ipp.stacks.example:443", "/p/core-hmhzx2isby/paris/jobs/7"}: {
			x: "core-hmhzx2isby.stacks.example", host: "ipp.stacks.example", handle: "core-hmhzx2isby", base: "/p/core-hmhzx2isby", printer: "paris", job: 7},
		{"ipp.stacks.example", "/p/core-hmhzx2isby/jobs"}: {
			x: "core-hmhzx2isby.stacks.example", host: "ipp.stacks.example", handle: "core-hmhzx2isby", base: "/p/core-hmhzx2isby", printer: "jobs"},
		// The plain form still parses on the shared host (whether the suffix
		// itself names a tenant is the lookup's question — under `txco dev`
		// it does: the suffix is `localhost`, which is also a bound hostname).
		{"ipp.stacks.example", "/p/research"}: {x: "stacks.example", host: "ipp.stacks.example", base: "/p", printer: "research"},
	}
	for in, want := range ok {
		got, valid := ippTarget(in[0], in[1], shared)
		if !valid || got != want {
			t.Errorf("ippTarget(%q,%q) = %+v,%v want %+v", in[0], in[1], got, valid, want)
		}
	}
	if got, _ := ippTarget("ipp.stacks.example", "/p/core-hmhzx2isby/paris", shared); got.uriPath() != "/p/core-hmhzx2isby/paris" {
		t.Errorf("shared uriPath = %q", got.uriPath())
	}
	for _, in := range [][2]string{
		{"dripl.it", "/p/research"},            // not an ipp host
		{"ipp-dripl.it", "/p/research"},        // the marker is a LABEL
		{"notipp.dripl.it", "/p/research"},     //
		{"ipp.", "/p/research"},                // nothing behind the label
		{"ipp.dripl.it", "/"},                  //
		{"ipp.dripl.it", "/p/"},                //
		{"ipp.dripl.it", "/p/Research"},        // labels are lowercase
		{"ipp.dripl.it", "/p/-bad"},            //
		{"ipp.dripl.it", "/p/a/b"},             // the handle form exists ONLY on the shared door
		{"ipp.dripl.it", "/p/a/b/jobs/3"},      //
		{"ipp.dripl.it", "/p/a/jobs/0"},        //
		{"ipp.dripl.it", "/p/a/jobs/x"},        //
		{"ipp.dripl.it", "/p/../etc"},          //
		{"ipp.dripl.it", "/printers/research"}, //
		// On the shared door the handle is ONE dns label: a dot would let the
		// path name a host at another depth (or climb out of the suffix).
		{"ipp.stacks.example", "/p/a.b/paris"},               //
		{"ipp.stacks.example", "/p/evil.example.com/paris"},  //
		{"ipp.stacks.example", "/p/-bad/paris"},              //
		{"ipp.stacks.example", "/p/UPPER/paris"},             //
		{"ipp.stacks.example", "/p/core-abc/paris/extra"},    //
		{"ipp.stacks.example", "/p/core-abc/Paris"},          //
		{"ipp.core-abc.stacks.example", "/p/core-abc/paris"}, // two labels under the suffix is not the door
	} {
		if got, valid := ippTarget(in[0], in[1], shared); valid {
			t.Errorf("ippTarget(%q,%q) accepted: %+v", in[0], in[1], got)
		}
	}
	// No structured suffix configured: there is no shared door at all.
	if got, valid := ippTarget("ipp.stacks.example", "/p/core-abc/paris", ""); valid {
		t.Errorf("handle form accepted with no shared zone: %+v", got)
	}
	if normalizeZone(".Stacks.Example.") != "stacks.example" || normalizeZone("") != "" {
		t.Error("normalizeZone")
	}
	if !IsIPPHost("ipp.dripl.it:443") || IsIPPHost("dripl.it") || IsIPPHost("ipp-dripl.it") {
		t.Error("IsIPPHost")
	}
}

// The shared front door end to end: a tenant with no zone of its own prints
// through `ipp.<suffix>/p/<handle>/<printer>`. The handle picks the tenant
// (through the hostname the tenant has under the suffix), the credential is
// THAT tenant's, the URIs the client gets back keep the handle, and the
// stack still sees only the printer label.
func TestSharedFrontDoor(t *testing.T) {
	h := newHarness(t, config.Config{StructuredHostSuffix: ".stacks.example"})
	h.ctrl.Start()
	const door = "ipp.stacks.example"
	// The harness's tenant lookup is keyed by the hostname that decides the
	// tenant: for the shared door that is <handle>.<suffix>.
	h.zones["core-abc123.stacks.example"] = tenant
	h.zones["web-zzz999.stacks.example"] = "other"

	_, m := h.do(request{host: door, path: "/p/core-abc123/paris", op: goipp.OpGetPrinterAttributes})
	if m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("Get-Printer-Attributes through the door: %v", m)
	}
	if u, _ := attr(m.Printer, "printer-uri-supported"); u.Values[0].V.String() != "ipps://"+door+"/p/core-abc123/paris" {
		t.Fatalf("printer-uri-supported = %v", u.Values)
	}
	if n, _ := attr(m.Printer, "printer-name"); n.Values[0].V.String() != "paris" {
		t.Fatalf("printer-name = %v (the handle is not part of the printer's name)", n.Values)
	}

	_, m = h.do(request{host: door, path: "/p/core-abc123/paris", op: goipp.OpPrintJob, doc: pdfDoc,
		opAttrs: []goipp.Attribute{pdfFormat(), str("job-name", goipp.TagName, "Quarterly report.pdf")}})
	if m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("Print-Job through the door: %v", m)
	}
	jobNo := intOf(t, m.Job, "job-id")
	if u, _ := attr(m.Job, "job-uri"); u.Values[0].V.String() != "ipps://"+door+"/p/core-abc123/paris/jobs/1" {
		t.Fatalf("job-uri = %v", u.Values)
	}
	// The job URL the client was handed works as an address.
	if _, m = h.do(request{host: door, path: "/p/core-abc123/paris/jobs/1", op: goipp.OpGetJobAttributes}); m == nil || status(m) != goipp.StatusOk || intOf(t, m.Job, "job-id") != jobNo {
		t.Fatalf("job by its shared-door URL: %v", m)
	}

	waitFor(t, "delivery", func() bool { return len(h.envelopes()) == 1 })
	env := h.envelopes()[0]
	for path, want := range map[string]string{
		"_txc.ipp.tenant":      tenant,
		"_txc.ipp.printer":     "paris", // the stack never sees the handle
		"_txc.ipp.host":        door,
		"_txc.ipp.printer_uri": "ipps://" + door + "/p/core-abc123/paris",
	} {
		if got := gjson.Get(env, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}

	// Every way the door can be wrong is the SAME 404, before any credential.
	for name, rq := range map[string]request{
		"no handle (the suffix is nobody's zone)": {path: "/p/paris"},
		"unknown handle":    {path: "/p/nobody-000000/paris"},
		"handle with a dot": {path: "/p/core-abc123.stacks/paris"},
	} {
		rq.host, rq.op = door, goipp.OpGetJobs
		if resp, _ := h.do(rq); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: HTTP %d, want 404", name, resp.StatusCode)
		}
	}
	// A handle selects its OWN tenant: this tenant's password opens nothing
	// behind another tenant's handle.
	if resp, _ := h.do(request{host: door, path: "/p/web-zzz999/paris", op: goipp.OpGetJobs}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("another tenant's handle with our password: %d, want 401", resp.StatusCode)
	}
	if _, m := h.do(request{host: door, path: "/p/web-zzz999/paris", op: goipp.OpGetJobs, pass: otherPassword}); m == nil || status(m) != goipp.StatusOk {
		t.Errorf("the other tenant's own password was refused: %v", m)
	}
	// …and its jobs are invisible across handles.
	if _, m := h.do(request{host: door, path: "/p/web-zzz999/paris", op: goipp.OpGetJobAttributes, pass: otherPassword,
		opAttrs: []goipp.Attribute{goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobNo))}}); status(m) != goipp.StatusErrorNotFound {
		t.Errorf("a job seen through another tenant's handle: %v", status(m))
	}
}

// Under `txco dev` the structured suffix is `localhost`, so `ipp.localhost`
// is BOTH the shared door and the ordinary front door of the bound hostname
// `localhost`. The path shape tells them apart; both must work.
func TestSharedDoorCoexistsWithPlainHost(t *testing.T) {
	h := newHarness(t, config.Config{StructuredHostSuffix: ".localhost"})
	h.zones["localhost"] = tenant              // `txco dev` auto-binds localhost
	h.zones["demo-abc123.localhost"] = "other" // a minted structured host

	if _, m := h.do(request{host: "ipp.localhost:8443", path: "/p/research", op: goipp.OpGetJobs}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("plain form on the dev host: %v", m)
	}
	if _, m := h.do(request{host: "ipp.localhost:8443", path: "/p/demo-abc123/research", op: goipp.OpGetJobs, pass: otherPassword}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("handle form on the dev host: %v", m)
	}
}

// fakeResolver is the ingress resolver's hostname → (tenant, verified) table.
type fakeResolver map[string]ingress.RouteTarget

func (f fakeResolver) ResolveErr(key ingress.RouteKey) (ingress.RouteTarget, bool, error) {
	if key.Src != "http" {
		return ingress.RouteTarget{}, false, nil
	}
	t, ok := f[key.Hostname]
	return t, ok, nil
}

// The REAL lookup (the other tests replace it with a table): a handle on the
// shared door reaches its tenant only through a VERIFIED hostname row —
// the same strictness tls-ask applies — and a name with no row is nobody's.
func TestLookupTenantThroughHostnameRows(t *testing.T) {
	pu := &processor.Unit{Conf: config.Config{Personalities: "web,ipp", StructuredHostSuffix: ".stacks.example"}, Logger: zap.NewNop()}
	c := NewController(context.Background(), pu, nil, fakeResolver{
		"core-abc123.stacks.example": {Tenant: "onepony", Stack: "core", Verified: true},
		"unproven.stacks.example":    {Tenant: "sneaky", Stack: "web", Verified: false},
	})
	ctx := context.Background()

	slug, key, ok, err := c.lookupTenant(ctx, "core-abc123.stacks.example")
	if err != nil || !ok || slug != "onepony" || key != "host:core-abc123.stacks.example" {
		t.Fatalf("verified row: %q %q %v %v", slug, key, ok, err)
	}
	if _, _, ok, _ := c.lookupTenant(ctx, "unproven.stacks.example"); ok {
		t.Fatal("an UNVERIFIED hostname row routed a print job")
	}
	if _, _, ok, _ := c.lookupTenant(ctx, "nobody.stacks.example"); ok {
		t.Fatal("a hostname with no row named a tenant")
	}
	// The suffix itself is nobody's: the plain form on the shared host finds
	// no tenant (in production its zone belongs to the system tenant, which
	// site() refuses as well).
	if _, _, ok, _ := c.lookupTenant(ctx, "stacks.example"); ok {
		t.Fatal("the shared zone itself named a tenant")
	}
	if c.sharedZone != "stacks.example" {
		t.Fatalf("sharedZone = %q", c.sharedZone)
	}
}
