package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	radix "github.com/hashicorp/go-immutable-radix"
	_ "github.com/mattn/go-sqlite3"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/fetch"
	"github.com/loremlabs/thanks-computer/chassis/htmlextract"
	"github.com/loremlabs/thanks-computer/chassis/metrics"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/registry"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// stubFetcher answers every fetch with one result or error, and can hold
// a call until released (for the concurrency limits).
type stubFetcher struct {
	mu    sync.Mutex
	res   *fetch.Result
	err   error
	calls int
	hold  chan struct{}
	urls  []string
}

func (s *stubFetcher) Get(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	s.mu.Lock()
	s.calls++
	s.urls = append(s.urls, r.URL)
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, &fetch.Error{Code: fetch.CodeTimeout, Message: "held"}
		}
	}
	return s.res, s.err
}

func page(body string) *fetch.Result {
	return &fetch.Result{
		URL: "https://example.test/a", FinalURL: "https://example.test/b", Status: 200,
		ContentType: "text/html; charset=utf-8", Body: []byte(body), Bytes: int64(len(body)),
	}
}

func newHXDeps(f *stubFetcher, mod func(*htmlExtractDeps)) *htmlExtractDeps {
	d := &htmlExtractDeps{
		fetcher:    f,
		limits:     htmlextract.DefaultLimits(),
		fetchSlots: make(chan struct{}, 4),
		parseSlots: make(chan struct{}, 2),
		tenantMax:  4,
		rate:       throttle.New(0, time.Minute),
		inflight:   map[string]int{},
	}
	if mod != nil {
		mod(d)
	}
	return d
}

type captureTracer struct {
	mu     sync.Mutex
	events []trace.TimelineEvent
}

func (c *captureTracer) Step(trace.StepInfo)        {}
func (c *captureTracer) End(string, string, []byte) {}
func (c *captureTracer) Event(ev trace.TimelineEvent) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}
func (c *captureTracer) last() trace.TimelineEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events[len(c.events)-1]
}
func callHX(t *testing.T, d *htmlExtractDeps, tenant, meta string) string {
	t.Helper()
	out, _ := callHXCtx(t, context.Background(), d, tenant, meta)
	return out
}

func callHXCtx(t *testing.T, ctx context.Context, d *htmlExtractDeps, tenant, meta string) (string, *captureTracer) {
	t.Helper()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	tr := &captureTracer{}
	ctx = trace.WithContext(ctx, tr)
	ctx = operation.WithMeta(ctx, meta)
	pl, err := htmlExtract(ctx, d, []byte(`{"_txc":{"op":"web/3000/unfurl"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw, tr
}

const previewMeta = `{"url": "https://example.test/a?token=s3cret", "selectors": {
	"title": {"selector": "title", "text": true},
	"image": {"selector": "meta[property='og:image']", "attr": "content"},
	"links": {"selector": "a[href]", "attr": "href", "all": true}
}}`

const previewPage = `<html><head><title> Hello  world </title><meta property="og:image" content="/i.png"></head>
<body><a href="/x">x</a><a href="/y">y</a></body></html>`

func TestHTMLExtractAnswers(t *testing.T) {
	f := &stubFetcher{res: page(previewPage)}
	out := callHX(t, newHXDeps(f, nil), "acme", previewMeta)
	want := map[string]string{
		"_html.ok":           "true",
		"_html.url":          "https://example.test/a?token=s3cret",
		"_html.final_url":    "https://example.test/b",
		"_html.status":       "200",
		"_html.content_type": "text/html; charset=utf-8",
		"_html.data.title":   "Hello world",
		"_html.data.image":   "/i.png",
		"_html.data.links":   `["/x","/y"]`,
	}
	for path, v := range want {
		if got := gjson.Get(out, path).String(); got != v {
			t.Errorf("%s = %q, want %q (%s)", path, got, v, out)
		}
	}
	if gjson.Get(out, "_html.truncated").Exists() || gjson.Get(out, "_html.error").Exists() {
		t.Fatalf("unexpected fields: %s", out)
	}
	// Field order is the caller's.
	if keys := gjson.Get(out, "_html.data|@keys").String(); keys != `["title","image","links"]` {
		t.Fatalf("data keys = %s", keys)
	}
}

func TestHTMLExtractInto(t *testing.T) {
	f := &stubFetcher{res: page(previewPage)}
	d := newHXDeps(f, nil)
	meta := `{"url": "https://example.test/", "into": "_page", "selectors": {"t": {"selector": "title", "text": true}}}`
	if out := callHX(t, d, "acme", meta); gjson.Get(out, "_page.data.t").String() != "Hello world" {
		t.Fatalf("into _page: %s", out)
	}
	// A reserved target falls back to the default, like every builtin.
	for _, into := range []string{"@tenant", "_txc.tenant", "@principal"} {
		meta = `{"url": "https://example.test/", "into": "` + into + `", "selectors": {"t": {"selector": "title", "text": true}}}`
		if out := callHX(t, d, "acme", meta); gjson.Get(out, "_txc").Exists() || !gjson.Get(out, "_html.ok").Bool() {
			t.Fatalf("reserved into %s: %s", into, out)
		}
	}
}

func TestHTMLExtractRefusesBeforeFetching(t *testing.T) {
	f := &stubFetcher{res: page(previewPage)}
	d := newHXDeps(f, nil)
	cases := map[string]string{
		`{"url": "ftp://example.test/", "selectors": {"t": {"selector": "title", "text": true}}}`:            "txco_html_invalid_url",
		`{"url": "http://u:p@example.test/", "selectors": {"t": {"selector": "title", "text": true}}}`:       "txco_html_invalid_url",
		`{"url": "https://example.test:8443/", "selectors": {"t": {"selector": "title", "text": true}}}`:     "txco_html_invalid_url",
		`{"selectors": {"t": {"selector": "title", "text": true}}}`:                                          "txco_html_invalid_url",
		`{"url": "https://example.test/"}`:                                                                   "txco_html_invalid_selectors",
		`{"url": "https://example.test/", "selectors": {"t": {"selector": "div:has(p)", "text": true}}}`:     "txco_html_invalid_selectors",
		`{"url": "https://example.test/", "selectors": {"t": {"selector": "a", "text": true, "attr": "x"}}}`: "txco_html_invalid_selectors",
	}
	for meta, code := range cases {
		out := callHX(t, d, "acme", meta)
		if got := gjson.Get(out, "_html.error.code").String(); got != code || gjson.Get(out, "_html.ok").Bool() {
			t.Errorf("%s: %s, want %s", meta, out, code)
		}
	}
	if out := callHX(t, d, "", previewMeta); gjson.Get(out, "_html.error.code").String() != "txco_html_no_tenant" {
		t.Errorf("no tenant: %s", out)
	}
	if f.calls != 0 {
		t.Fatalf("fetched %d times, want none", f.calls)
	}
}

func TestHTMLExtractFetchErrors(t *testing.T) {
	cases := []struct {
		err         *fetch.Error
		code        string
		status      int
		contentType string
	}{
		{&fetch.Error{Code: fetch.CodeDestinationDenied, Message: "the destination is not permitted"}, "txco_html_destination_denied", 0, ""},
		{&fetch.Error{Code: fetch.CodeDNSFailed, Message: "x"}, "txco_html_dns_failed", 0, ""},
		{&fetch.Error{Code: fetch.CodeHTTPError, Message: "the server answered 404", Status: 404, ContentType: "text/html"}, "txco_html_http_error", 404, "text/html"},
		{&fetch.Error{Code: fetch.CodeUnsupportedType, Message: "x", Status: 200, ContentType: "application/json"}, "txco_html_unsupported_content_type", 0, "application/json"},
		{&fetch.Error{Code: fetch.CodeResponseTooLarge, Message: "x", Status: 200, ContentType: "text/html", Bytes: 3<<20 + 1}, "txco_html_response_too_large", 0, "text/html"},
	}
	for _, c := range cases {
		f := &stubFetcher{err: c.err}
		out := callHX(t, newHXDeps(f, nil), "acme", previewMeta)
		if gjson.Get(out, "_html.ok").Bool() || gjson.Get(out, "_html.error.code").String() != c.code ||
			int(gjson.Get(out, "_html.error.status").Int()) != c.status || gjson.Get(out, "_html.content_type").String() != c.contentType {
			t.Errorf("%s: %s", c.code, out)
		}
		if gjson.Get(out, "_html.data").Exists() {
			t.Errorf("%s: an error carries no data: %s", c.code, out)
		}
	}
}

func TestHTMLExtractDocumentErrors(t *testing.T) {
	var flood strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&flood, " a%d", i)
	}
	f := &stubFetcher{res: page("<div" + flood.String() + ">x</div>")}
	out := callHX(t, newHXDeps(f, nil), "acme", previewMeta)
	if gjson.Get(out, "_html.error.code").String() != "txco_html_document_too_complex" {
		t.Fatalf("attribute flood: %s", out)
	}
	// The response arrived, so its facts are in the answer.
	if gjson.Get(out, "_html.status").Int() != 200 || gjson.Get(out, "_html.final_url").String() == "" {
		t.Fatalf("response facts missing: %s", out)
	}
}

func TestHTMLExtractTruncatedFlag(t *testing.T) {
	f := &stubFetcher{res: page("<title>" + strings.Repeat("x", 5000) + "</title>")}
	out := callHX(t, newHXDeps(f, nil), "acme", `{"url": "https://example.test/", "selectors": {"t": {"selector": "title", "text": true}}}`)
	if !gjson.Get(out, "_html.truncated").Bool() || len(gjson.Get(out, "_html.data.t").String()) != 4096 {
		t.Fatalf("truncated: %s", out)
	}
}

func TestHTMLExtractRateLimit(t *testing.T) {
	f := &stubFetcher{res: page(previewPage)}
	d := newHXDeps(f, func(d *htmlExtractDeps) { d.rate = throttle.New(2, time.Minute) })
	for i := 0; i < 2; i++ {
		if out := callHX(t, d, "acme", previewMeta); !gjson.Get(out, "_html.ok").Bool() {
			t.Fatalf("call %d: %s", i, out)
		}
	}
	if out := callHX(t, d, "acme", previewMeta); gjson.Get(out, "_html.error.code").String() != "txco_html_rate_limited" {
		t.Fatalf("third call: %s", out)
	}
	// Another tenant has its own window.
	if out := callHX(t, d, "other", previewMeta); !gjson.Get(out, "_html.ok").Bool() {
		t.Fatalf("other tenant: %s", out)
	}
}

func TestHTMLExtractTenantInFlightCap(t *testing.T) {
	f := &stubFetcher{res: page(previewPage), hold: make(chan struct{})}
	d := newHXDeps(f, func(d *htmlExtractDeps) { d.tenantMax = 1 })
	done := make(chan string)
	go func() { done <- callHX(t, d, "acme", previewMeta) }()
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.calls == 1 })
	if out := callHX(t, d, "acme", previewMeta); gjson.Get(out, "_html.error.code").String() != "txco_html_busy" {
		t.Fatalf("second call while the first is in flight: %s", out)
	}
	close(f.hold)
	if out := <-done; !gjson.Get(out, "_html.ok").Bool() {
		t.Fatalf("first call: %s", out)
	}
	// The slot is free again.
	if out := callHX(t, d, "acme", previewMeta); !gjson.Get(out, "_html.ok").Bool() {
		t.Fatalf("after: %s", out)
	}
	if len(d.inflight) != 0 {
		t.Fatalf("inflight = %v, want empty", d.inflight)
	}
}

func TestHTMLExtractNodeFetchSlotsWaitUnderTheDeadline(t *testing.T) {
	f := &stubFetcher{res: page(previewPage), hold: make(chan struct{})}
	defer close(f.hold)
	d := newHXDeps(f, func(d *htmlExtractDeps) { d.fetchSlots = make(chan struct{}, 1) })
	go callHX(t, d, "acme", previewMeta)
	waitFor(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.calls == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, _ := callHXCtx(t, ctx, d, "other", previewMeta)
	if gjson.Get(out, "_html.error.code").String() != "txco_html_busy" {
		t.Fatalf("waiting for a node slot: %s", out)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The timeline event names the host and a hash, never the path or query.
func TestHTMLExtractCompletionEventCarriesNoURL(t *testing.T) {
	f := &stubFetcher{res: page(previewPage)}
	out, tr := callHXCtx(t, context.Background(), newHXDeps(f, nil), "acme", previewMeta)
	if !gjson.Get(out, "_html.ok").Bool() {
		t.Fatal(out)
	}
	ev := tr.last()
	if ev.Event != "html.completion" {
		t.Fatalf("event = %s", ev.Event)
	}
	if ev.Fields["host"] != "example.test" || ev.Fields["url_sha256"] == "" || ev.Fields["status"] != 200 {
		t.Fatalf("fields = %v", ev.Fields)
	}
	for k, v := range ev.Fields {
		if s := fmt.Sprint(v); strings.Contains(s, "token") || strings.Contains(s, "s3cret") || strings.Contains(s, "/a") {
			t.Fatalf("field %s = %q leaks the URL", k, s)
		}
	}
	f.err, f.res = &fetch.Error{Code: fetch.CodeDestinationDenied, Message: "x"}, nil
	_, tr = callHXCtx(t, context.Background(), newHXDeps(f, nil), "acme", previewMeta)
	if tr.last().Fields["error_code"] != "txco_html_destination_denied" {
		t.Fatalf("error event = %v", tr.last().Fields)
	}
}

// newHXUnit builds a processor with an in-memory ops table and the
// html/extract handler registered, for running real rules.
func newHXUnit(t *testing.T, f *stubFetcher) *processor.Unit {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE ops (stack TEXT, scope INTEGER, name TEXT NOT NULL DEFAULT '', txcl TEXT, mock_req TEXT, mock_res TEXT, tenant_id TEXT, UNIQUE(stack, scope, txcl, tenant_id))`,
		`CREATE TABLE tenants (tenant_id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, name TEXT, created_at TEXT, revoked_at TEXT)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	conf := config.Config{
		Environment: "test", OpTimeout: "2s", OpTimeoutMax: "10m", DialTimeout: "100ms",
		AsyncAckTimeout: "5s", AsyncRuntimeDefault: "10m", DeferredJoinSlack: "60s",
		MaxFuelPerRequest: 100000,
	}
	logger := zap.NewNop()
	pu := &processor.Unit{
		Conf: conf, Logger: logger, RuntimeDB: db, AuthDB: db,
		Dbc:        &dbcache.DbCache{Db: db, Source: db, Logger: logger},
		Mc:         &metrics.Metrics{Tracer: noop.NewTracerProvider().Tracer("test")},
		Reg:        registry.New(conf, logger),
		Bus:        make(chan *event.Envelope, 1),
		Mux:        radix.New(),
		HTTPClient: &http.Client{Timeout: time.Second},
	}
	d := newHXDeps(f, nil)
	pu.Handle([]byte("txco://html/extract"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return htmlExtract(ctx, d, in)
		}))
	return pu
}

// A rule calls the op through real dispatch: WITH from txcl, the answer
// merged into the envelope, the fuel on the request.
func TestHTMLExtractThroughDispatch(t *testing.T) {
	f := &stubFetcher{res: page(previewPage)}
	pu := newHXUnit(t, f)
	rule := `WHEN .go == true
  WITH url = .link,
       selectors = &object(
         "title", &object("selector", "title", "text", true),
         "image", &object("selector", "meta[property='og:image']", "attr", "content")),
       into = "_page"
  EXEC "txco://html/extract"`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tenants (tenant_id, slug) VALUES ('t_acme', 'acme')`, nil},
		{`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES ('unfurl', 0, 'r', ?, '', '', 't_acme')`, []any{rule}},
	} {
		if _, err := pu.Dbc.Db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	resCh := make(chan event.Payload, 1)
	envelope := `{"go": true, "link": "https://example.test/a", "_txc": {"tenant": "acme", "src": "http"}}`
	if err := pu.Run(context.Background(), envelope, "unfurl/0", resCh); err != nil {
		t.Fatal(err)
	}
	out := (<-resCh).Raw
	if gjson.Get(out, "_page.data.title").String() != "Hello world" || gjson.Get(out, "_page.data.image").String() != "/i.png" {
		t.Fatalf("envelope: %s", out)
	}
	if len(f.urls) != 1 || f.urls[0] != "https://example.test/a" {
		t.Fatalf("fetched %v", f.urls)
	}
	// Dispatch (25) + the fetch (50) + 1 MiB started (100) + parse time (≥10).
	if fuel := processor.FuelUsedFromEnvelope(out); fuel < 25+50+100+10 {
		t.Fatalf("fuel used = %d: %s", fuel, out)
	}
}
