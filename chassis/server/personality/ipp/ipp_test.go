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
	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	chipp "github.com/loremlabs/thanks-computer/chassis/ipp"
	"github.com/loremlabs/thanks-computer/chassis/processor"
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
	password = "five-correct-horse-battery-staples"
)

type harness struct {
	t      *testing.T
	ctrl   *Controller
	store  *chipp.Store
	fcas   *filestore.FileStore
	ix     *memIndex
	srv    *httptest.Server // TLS
	plain  *httptest.Server
	bus    chan *event.Envelope
	mu     sync.Mutex
	seen   []string // envelopes the "bus" accepted
	zones  map[string]string
	subs   map[string]bool
	secret map[string]string // tenant + "/" + name
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
	if conf.IPPAuthRate == 0 {
		conf.IPPAuthRate = 1000
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
		bus:    make(chan *event.Envelope, 16),
		zones:  map[string]string{"dripl.example": tenant, "other.example": "other", "quiet.example": "quiet", "nokey.example": "nokey", "broke.example": "suspended"},
		subs:   map[string]bool{tenant: true, "other": true, "nokey": true, "suspended": true},
		secret: map[string]string{tenant + "/" + SecretPassword: password, "other/" + SecretPassword: "others-password", "suspended/" + SecretPassword: password},
	}
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Bus: h.bus, Admission: fakeAdmission{suspended: "suspended"}}
	ctx, cancel := context.WithCancel(context.Background())
	c := NewController(ctx, pu, store, nil)
	c.SetFileCAS(fs)
	c.SetBlobIndex(h.ix)
	c.SetSecretSource(func(_ context.Context, slug, name string) ([]byte, bool, error) {
		v, ok := h.secret[slug+"/"+name]
		return []byte(v), ok, nil
	})
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
			h.mu.Lock()
			h.seen = append(h.seen, env.Payload.Raw)
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
			user = DefaultUsername
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
// stack, or which have set a password leaks to a prober.
func TestNotFoundIsOneAnswer(t *testing.T) {
	h := newHarness(t, config.Config{})
	var first string
	for name, rq := range map[string]request{
		"unknown zone":             {host: "ipp.nobody.example"},
		"subdomain of a real zone": {host: "ipp.sub.dripl.example"},
		"tenant without _ipp":      {host: "ipp.quiet.example"},
		"tenant without a secret":  {host: "ipp.nokey.example"},
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

func TestTransportGates(t *testing.T) {
	h := newHarness(t, config.Config{})

	// GET is not IPP.
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+"/p/research", nil)
	req.Host = zoneHost
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET: %d Allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}

	// Wrong content type.
	req, _ = http.NewRequest(http.MethodPost, h.srv.URL+"/p/research", strings.NewReader("{}"))
	req.Host = zoneHost
	req.Header.Set("Content-Type", "application/json")
	resp, _ = h.srv.Client().Do(req)
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
	// Wrong password / wrong user / another tenant's password: all 401.
	for name, rq := range map[string]request{
		"wrong password":        {pass: "nope"},
		"wrong user":            {user: "admin"},
		"other tenant password": {pass: "others-password"},
	} {
		rq.op = goipp.OpGetJobs
		if resp, _ := h.do(rq); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, resp.StatusCode)
		}
	}
	// The right one works — and a tenant-chosen username replaces the default.
	if _, m := h.do(request{op: goipp.OpGetJobs}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("valid credential refused: %v", m)
	}
	h.secret[tenant+"/"+SecretUsername] = "matt"
	if resp, _ := h.do(request{op: goipp.OpGetJobs}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("default username still accepted after IPP_USERNAME was set: %d", resp.StatusCode)
	}
	if _, m := h.do(request{op: goipp.OpGetJobs, user: "matt"}); m == nil || status(m) != goipp.StatusOk {
		t.Fatalf("custom username refused: %v", m)
	}
	delete(h.secret, tenant+"/"+SecretUsername)

	// Rotating the secret invalidates the cached login at once.
	h.secret[tenant+"/"+SecretPassword] = "a-brand-new-password"
	if resp, _ := h.do(request{op: goipp.OpGetJobs}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password survived a rotation: %d", resp.StatusCode)
	}
	h.secret[tenant+"/"+SecretPassword] = password

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
// point of counting every cache MISS rather than every failure.
func TestAuthThrottle(t *testing.T) {
	h := newHarness(t, config.Config{IPPAuthRate: 3})
	for i := 0; i < 3; i++ {
		if resp, _ := h.do(request{op: goipp.OpGetJobs, pass: "guess"}); resp.StatusCode != http.StatusUnauthorized {
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
	h := newHarness(t, config.Config{IPPAuthRate: 2})
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
	rec = serve("wrong password", func(r *http.Request) { r.SetBasicAuth(DefaultUsername, "nope") })
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

	waitFor(t, "job completed", func() bool {
		_, m := h.do(request{op: goipp.OpGetJobAttributes, opAttrs: []goipp.Attribute{goipp.MakeAttribute("job-id", goipp.TagInteger, goipp.Integer(jobNo))}})
		return m != nil && status(m) == goipp.StatusOk && intOf(t, m.Job, "job-state") == 9
	})
	j, _ := h.store.GetJob(context.Background(), tenant, "research", 1)
	if j.State != chipp.StateDelivered || j.Rid == "" {
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
	if _, m = h.do(request{op: goipp.OpGetJobAttributes, host: "ipp.other.example", pass: "others-password", opAttrs: jobID(n)}); status(m) != goipp.StatusErrorNotFound {
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
	ok := map[[2]string]target{
		{"ipp.dripl.it", "/p/research"}:           {x: "dripl.it", host: "ipp.dripl.it", printer: "research"},
		{"IPP.Dripl.IT:443", "/p/research/"}:      {x: "dripl.it", host: "ipp.dripl.it", printer: "research"},
		{"ipp.dripl.it", "/p/research/jobs/42"}:   {x: "dripl.it", host: "ipp.dripl.it", printer: "research", job: 42},
		{"ipp.localhost:8443", "/p/a.b-c_1"}:      {x: "localhost", host: "ipp.localhost", printer: "a.b-c_1"},
		{"ipp.pony.acme.example", "/p/summarize"}: {x: "pony.acme.example", host: "ipp.pony.acme.example", printer: "summarize"},
	}
	for in, want := range ok {
		got, valid := ippTarget(in[0], in[1])
		if !valid || got != want {
			t.Errorf("ippTarget(%q,%q) = %+v,%v want %+v", in[0], in[1], got, valid, want)
		}
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
		{"ipp.dripl.it", "/p/a/b"},             //
		{"ipp.dripl.it", "/p/a/jobs/0"},        //
		{"ipp.dripl.it", "/p/a/jobs/x"},        //
		{"ipp.dripl.it", "/p/../etc"},          //
		{"ipp.dripl.it", "/printers/research"}, //
	} {
		if got, valid := ippTarget(in[0], in[1]); valid {
			t.Errorf("ippTarget(%q,%q) accepted: %+v", in[0], in[1], got)
		}
	}
	if !IsIPPHost("ipp.dripl.it:443") || IsIPPHost("dripl.it") || IsIPPHost("ipp-dripl.it") {
		t.Error("IsIPPHost")
	}
}
