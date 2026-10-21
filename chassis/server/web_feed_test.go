package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	radix "github.com/hashicorp/go-immutable-radix"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/fetch"
	"github.com/loremlabs/thanks-computer/chassis/metrics"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/registry"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

const feedBody = `<?xml version="1.0"?><rss version="2.0"><channel><title>Example news</title><link>https://example.test/</link>
<item><title>Older</title><link>/posts/older</link><pubDate>Sun, 18 Oct 2026 10:00:00 GMT</pubDate><description>old</description></item>
<item><title>Newest &amp; best</title><link>/posts/newest?s3cret=1</link><pubDate>Mon, 19 Oct 2026 10:00:00 GMT</pubDate><description>&lt;p&gt;Fresh&lt;/p&gt;</description></item>
</channel></rss>`

func feedResult(body, contentType string) *fetch.Result {
	return &fetch.Result{URL: "https://example.test/feed", FinalURL: "https://feeds.example.test/rss", Status: 200,
		ContentType: contentType, Body: []byte(body), Bytes: int64(len(body))}
}

func newWFDeps(f *stubFetcher, mod func(*webFeedDeps)) *webFeedDeps {
	d := &webFeedDeps{
		enabled:   true,
		fetcher:   f,
		slots:     make(chan struct{}, 4),
		tenantMax: 4,
		rate:      throttle.New(0, time.Minute),
		inflight:  map[string]int{},
	}
	if mod != nil {
		mod(d)
	}
	return d
}

func callWF(t *testing.T, d *webFeedDeps, tenant, meta string) string {
	t.Helper()
	out, _ := callWFCtx(t, context.Background(), d, webFeedName, tenant, meta)
	return out
}

func callWFCtx(t *testing.T, ctx context.Context, d *webFeedDeps, name, tenant, meta string) (string, *captureTracer) {
	t.Helper()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	tr := &captureTracer{}
	ctx = trace.WithContext(ctx, tr)
	ctx = operation.WithMeta(ctx, meta)
	pl, err := webFeed(ctx, d, name, []byte(`{"_txc":{"op":"web/3000/feed"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw, tr
}

func TestWebFeedAnswers(t *testing.T) {
	f := &stubFetcher{res: feedResult(feedBody, "application/rss+xml")}
	out, tr := callWFCtx(t, context.Background(), newWFDeps(f, nil), webFeedName, "acme", `{"url": "https://example.test/feed"}`)
	want := map[string]string{
		"_feed.ok":              "true",
		"_feed.url":             "https://example.test/feed",
		"_feed.final_url":       "https://feeds.example.test/rss",
		"_feed.status":          "200",
		"_feed.format":          "rss",
		"_feed.feed.title":      "Example news",
		"_feed.feed.link":       "https://example.test/",
		"_feed.count":           "2",
		"_feed.total":           "2",
		"_feed.truncated":       "false",
		"_feed.items.0.title":   "Newest & best",
		"_feed.items.0.url":     "https://feeds.example.test/posts/newest?s3cret=1",
		"_feed.items.0.source":  "feeds.example.test",
		"_feed.items.0.date":    "2026-10-19T10:00:00Z",
		"_feed.items.0.summary": "Fresh",
		"_feed.items.1.title":   "Older",
	}
	for path, v := range want {
		if got := gjson.Get(out, path).String(); got != v {
			t.Errorf("%s = %q, want %q (%s)", path, got, v, out)
		}
	}
	if f.urls[0] != "https://example.test/feed" {
		t.Errorf("fetched %q", f.urls[0])
	}
	ev := tr.last()
	if ev.Event != "web.feed.completion" || ev.Fields["host"] != "example.test" || ev.Fields["final_host"] != "feeds.example.test" || ev.Fields["format"] != "rss" || ev.Fields["items"] != 2 || ev.Fields["total"] != 2 || ev.Fields["status"] != 200 || ev.Fields["url_sha256"] == "" {
		t.Errorf("event %+v", ev)
	}
	for k, v := range ev.Fields {
		if s := fmt.Sprint(v); strings.Contains(s, "s3cret") || strings.Contains(s, "Newest") || strings.Contains(s, "/posts") {
			t.Errorf("field %s = %q leaks", k, s)
		}
	}
}

func TestWebFeedOptions(t *testing.T) {
	f := &stubFetcher{res: feedResult(feedBody, "text/html")} // the wrong type: the bytes decide
	d := newWFDeps(f, nil)
	out := callWF(t, d, "acme", `{"url": "https://example.test/feed", "into": "_news", "count": 1}`)
	if gjson.Get(out, "_news.count").Int() != 1 || gjson.Get(out, "_news.total").Int() != 2 || gjson.Get(out, "_news.items.0.title").String() != "Newest & best" {
		t.Fatalf("%s", out)
	}
	out = callWF(t, d, "acme", `{"url": "https://example.test/feed", "since": "2026-10-19"}`)
	if gjson.Get(out, "_feed.count").Int() != 1 || gjson.Get(out, "_feed.items.0.title").String() != "Newest & best" {
		t.Fatalf("%s", out)
	}
	out = callWF(t, d, "acme", `{"url": "https://example.test/feed", "since": "2026-10-20T00:00:00Z"}`)
	if gjson.Get(out, "_feed.ok").Bool() != true || gjson.Get(out, "_feed.count").Int() != 0 || !gjson.Get(out, "_feed.items").IsArray() {
		t.Fatalf("%s", out)
	}
}

func TestWebFeedRefusesBeforeFetching(t *testing.T) {
	f := &stubFetcher{res: feedResult(feedBody, "application/rss+xml")}
	for _, c := range []struct {
		name, tenant, meta, code string
		mod                      func(*webFeedDeps)
	}{
		{webFeedName, "", `{"url": "https://example.test/feed"}`, "txco_feed_no_tenant", nil},
		{webFeedName, "acme", `{"url": "https://example.test/feed"}`, "txco_feed_disabled", func(d *webFeedDeps) { d.enabled = false }},
		{webFeedName, "acme", `{"url": "https://example.test/feed", "freshness": "day"}`, "txco_feed_invalid_request", nil},
		{webFeedName, "acme", `{"url": "https://example.test/feed", "count": 0}`, "txco_feed_invalid_request", nil},
		{webFeedName, "acme", `{"url": "https://example.test/feed", "since": "soon"}`, "txco_feed_invalid_request", nil},
		{webFeedName, "acme", `{"url": "ftp://example.test/feed"}`, "txco_feed_invalid_url", nil},
		{webFeedName, "acme", `{"url": "https://user:pw@example.test/feed"}`, "txco_feed_invalid_url", nil},
		{webFeedName, "acme", `{}`, "txco_feed_invalid_request", nil},
		{"txco://web/feeds", "acme", `{"url": "https://example.test/feed"}`, "txco_feed_invalid_request", nil},
	} {
		calls := f.calls
		out, _ := callWFCtx(t, context.Background(), newWFDeps(f, c.mod), c.name, c.tenant, c.meta)
		if gjson.Get(out, "_feed.ok").Bool() || gjson.Get(out, "_feed.error.code").String() != c.code {
			t.Errorf("%s %s: %s", c.name, c.meta, out)
		}
		if f.calls != calls {
			t.Errorf("%s fetched", c.meta)
		}
	}
}

func TestWebFeedFetchErrors(t *testing.T) {
	for _, c := range []struct {
		err         *fetch.Error
		code        string
		status      int
		contentType string
	}{
		{&fetch.Error{Code: fetch.CodeHTTPError, Message: "the server answered 404", Status: 404, ContentType: "text/html"}, "txco_feed_http_error", 404, "text/html"},
		{&fetch.Error{Code: fetch.CodeTimeout, Message: "timed out"}, "txco_feed_timeout", 0, ""},
		{&fetch.Error{Code: fetch.CodeDestinationDenied, Message: "denied"}, "txco_feed_destination_denied", 0, ""},
		{&fetch.Error{Code: fetch.CodeResponseTooLarge, Message: "too large", Bytes: 4 << 20}, "txco_feed_response_too_large", 0, ""},
	} {
		f := &stubFetcher{err: c.err}
		out, tr := callWFCtx(t, context.Background(), newWFDeps(f, nil), webFeedName, "acme", `{"url": "https://example.test/feed"}`)
		if gjson.Get(out, "_feed.ok").Bool() || gjson.Get(out, "_feed.error.code").String() != c.code || gjson.Get(out, "_feed.error.status").Int() != int64(c.status) || gjson.Get(out, "_feed.content_type").String() != c.contentType {
			t.Errorf("%s: %s", c.code, out)
		}
		if ev := tr.last(); ev.Fields["error_code"] != c.code {
			t.Errorf("%s: event %+v", c.code, ev.Fields)
		}
	}
}

func TestWebFeedNotAFeedNamesTheFeeds(t *testing.T) {
	page := `<html><head><link rel="alternate" type="application/rss+xml" href="/feed.xml"><link rel="alternate" type="application/atom+xml" href="https://feeds.example.test/atom"></head><body></body></html>`
	f := &stubFetcher{res: feedResult(page, "text/html; charset=utf-8")}
	out := callWF(t, newWFDeps(f, nil), "acme", `{"url": "https://example.test/"}`)
	if gjson.Get(out, "_feed.ok").Bool() || gjson.Get(out, "_feed.error.code").String() != "txco_feed_not_a_feed" {
		t.Fatalf("%s", out)
	}
	if feeds := gjson.Get(out, "_feed.error.feeds").Array(); len(feeds) != 2 || feeds[0].String() != "https://feeds.example.test/feed.xml" || feeds[1].String() != "https://feeds.example.test/atom" {
		t.Fatalf("%s", out)
	}
	if gjson.Get(out, "_feed.final_url").String() != "https://feeds.example.test/rss" || gjson.Get(out, "_feed.content_type").String() != "text/html; charset=utf-8" {
		t.Fatalf("%s", out)
	}
	f = &stubFetcher{res: feedResult(`<rss><channel><title>x</title><item><title>a</title></ite`, "text/xml")}
	if out := callWF(t, newWFDeps(f, nil), "acme", `{"url": "https://example.test/feed"}`); gjson.Get(out, "_feed.error.code").String() != "txco_feed_invalid_feed" {
		t.Fatalf("%s", out)
	}
}

func TestWebFeedRateLimit(t *testing.T) {
	f := &stubFetcher{res: feedResult(feedBody, "application/rss+xml")}
	d := newWFDeps(f, func(d *webFeedDeps) { d.rate = throttle.New(2, time.Minute) })
	for i := 0; i < 2; i++ {
		if out := callWF(t, d, "acme", `{"url": "https://example.test/feed"}`); !gjson.Get(out, "_feed.ok").Bool() {
			t.Fatal(out)
		}
	}
	if out := callWF(t, d, "acme", `{"url": "https://example.test/feed"}`); gjson.Get(out, "_feed.error.code").String() != "txco_feed_rate_limited" {
		t.Fatal(out)
	}
	if out := callWF(t, d, "other", `{"url": "https://example.test/feed"}`); !gjson.Get(out, "_feed.ok").Bool() {
		t.Fatal(out)
	}
}

func TestWebFeedTenantInFlightCap(t *testing.T) {
	f := &stubFetcher{res: feedResult(feedBody, "application/rss+xml"), hold: make(chan struct{})}
	d := newWFDeps(f, func(d *webFeedDeps) { d.tenantMax = 1 })
	done := make(chan string, 1)
	go func() { done <- callWF(t, d, "acme", `{"url": "https://example.test/feed"}`) }()
	for {
		f.mu.Lock()
		n := f.calls
		f.mu.Unlock()
		if n == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if out := callWF(t, d, "acme", `{"url": "https://example.test/feed"}`); gjson.Get(out, "_feed.error.code").String() != "txco_feed_busy" {
		t.Fatal(out)
	}
	// Another tenant's allowance is its own: the held call only blocks
	// its tenant. (The stub holds every call, so release it first.)
	close(f.hold)
	if out := <-done; !gjson.Get(out, "_feed.ok").Bool() {
		t.Fatal(out)
	}
	for _, tenant := range []string{"acme", "other"} {
		if out := callWF(t, d, tenant, `{"url": "https://example.test/feed"}`); !gjson.Get(out, "_feed.ok").Bool() {
			t.Fatalf("%s after release: %s", tenant, out)
		}
	}
}

// newWFUnit builds a processor with an in-memory ops table and the
// web/feed handler registered, for running real rules.
func newWFUnit(t *testing.T, d *webFeedDeps) *processor.Unit {
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
	pu.Handle([]byte("txco://web/feed"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return webFeed(ctx, d, opName, in)
		}))
	return pu
}

// A rule calls the op through real dispatch: WITH from txcl, the answer
// merged into the envelope, the fuel on the request: the scope entry
// (10), the op's 25, the fetch's 50, one started MiB (100) and the read's
// started milliseconds (10 each).
func TestWebFeedThroughDispatch(t *testing.T) {
	f := &stubFetcher{res: feedResult(feedBody, "application/rss+xml")}
	pu := newWFUnit(t, newWFDeps(f, nil))
	rule := `WHEN .go == true
  WITH url = &concat("https://example.test/", .path),
       count = 1,
       into = "_news"
  EXEC "txco://web/feed"`
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO tenants (tenant_id, slug) VALUES ('t_acme', 'acme')`, nil},
		{`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res, tenant_id) VALUES ('news', 0, 'r', ?, '', '', 't_acme')`, []any{rule}},
	} {
		if _, err := pu.Dbc.Db.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), `{"go": true, "path": "feed", "_txc": {"tenant": "acme", "src": "http"}}`, "news/0", resCh); err != nil {
		t.Fatal(err)
	}
	out := (<-resCh).Raw
	if !gjson.Get(out, "_news.ok").Bool() || gjson.Get(out, "_news.count").Int() != 1 || gjson.Get(out, "_news.items.0.source").String() != "feeds.example.test" {
		t.Fatalf("envelope: %s", out)
	}
	if f.urls[0] != "https://example.test/feed" {
		t.Fatalf("fetched %q", f.urls[0])
	}
	if fuel := processor.FuelUsedFromEnvelope(out); fuel < 10+25+50+100+10 || fuel > 10+25+50+100+10*500 {
		t.Fatalf("fuel used = %d, want 195 plus the read's milliseconds: %s", fuel, out)
	}
}
