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
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/websearch"
	"github.com/loremlabs/thanks-computer/chassis/websearch/brave"
)

// wsFetch answers every fetch with one result or error, records the
// requests it saw, and can hold a call until released.
type wsFetch struct {
	mu   sync.Mutex
	res  *fetch.Result
	err  error
	reqs []fetch.Request
	hold chan struct{}
}

func (s *wsFetch) Get(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, r)
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

func (s *wsFetch) last() fetch.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[len(s.reqs)-1]
}

// wsSecrets is a tenant store with one value per (tenant, stack, name).
// "" as the stack is the tenant-wide value; a stack-scoped value wins,
// as the real resolver's does.
type wsSecrets struct {
	mu     sync.Mutex
	values map[string][]byte
	asked  []string // "tenant/stack/name"
	err    error
}

func (s *wsSecrets) MaterializeForOpSlug(_ context.Context, tenant, stack, name string) ([]byte, *secrets.SecretMetadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, tenant+"/"+stack+"/"+name)
	if s.err != nil {
		return nil, nil, s.err
	}
	if v, ok := s.values[tenant+"/"+stack+"/"+name]; ok && stack != "" {
		return append([]byte(nil), v...), nil, nil
	}
	if v, ok := s.values[tenant+"//"+name]; ok {
		return append([]byte(nil), v...), nil, nil
	}
	return nil, nil, secrets.ErrSecretNotFound
}

const braveBody = `{"type": "search", "query": {"original": "french employment law"}, "web": {"results": [
	{"title": "Ce qui change au 1er octobre", "url": "https://example.fr/actualites/octobre", "description": "Les <strong>nouvelles</strong> r&egrave;gles", "page_age": "2026-10-01T00:00:00"},
	{"title": "Second", "url": "https://two.test/x", "description": "d2"}
]}}`

func braveAnswer(body string) *fetch.Result {
	return &fetch.Result{URL: "https://brave.test/res/v1/web/search", FinalURL: "https://brave.test/res/v1/web/search", Status: 200,
		ContentType: "application/json", Body: []byte(body), Bytes: int64(len(body))}
}

func newWSDeps(t *testing.T, f *wsFetch, mod func(*webSearchDeps)) *webSearchDeps {
	t.Helper()
	p, err := websearch.Open(brave.Name, websearch.Config{BraveURL: "https://brave.test/res/v1"})
	if err != nil {
		t.Fatal(err)
	}
	d := &webSearchDeps{
		enabled:   true,
		provider:  p,
		fetcher:   f,
		secrets:   &wsSecrets{values: map[string][]byte{"acme//BRAVE_SEARCH_KEY": []byte("sk-acme")}},
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

func callWS(t *testing.T, d *webSearchDeps, tenant, meta string) string {
	t.Helper()
	out, _ := callWSCtx(t, context.Background(), d, webSearchName, tenant, meta)
	return out
}

func callWSCtx(t *testing.T, ctx context.Context, d *webSearchDeps, name, tenant, meta string) (string, *captureTracer) {
	t.Helper()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	tr := &captureTracer{}
	ctx = trace.WithContext(ctx, tr)
	ctx = operation.WithMeta(ctx, meta)
	pl, err := webSearch(ctx, d, name, []byte(`{"_txc":{"op":"web/3000/search"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw, tr
}

const searchMeta = `{"query": "french employment law", "count": 5, "country": "FR", "language": "fr", "freshness": "month"}`

func TestWebSearchAnswers(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	out := callWS(t, newWSDeps(t, f, nil), "acme", searchMeta)
	want := map[string]string{
		"_websearch.ok":                    "true",
		"_websearch.query":                 "french employment law",
		"_websearch.kind":                  "web",
		"_websearch.provider":              "brave",
		"_websearch.count":                 "2",
		"_websearch.truncated":             "false",
		"_websearch.results.0.title":       "Ce qui change au 1er octobre",
		"_websearch.results.0.url":         "https://example.fr/actualites/octobre",
		"_websearch.results.0.description": "Les nouvelles règles",
		"_websearch.results.0.page_date":   "2026-10-01T00:00:00Z",
		"_websearch.results.0.source":      "example.fr",
		"_websearch.results.1.source":      "two.test",
		"_websearch.results.1.description": "d2",
		"_websearch.results.1.title":       "Second",
		"_websearch.results.1.url":         "https://two.test/x",
	}
	for path, v := range want {
		if got := gjson.Get(out, path).String(); got != v {
			t.Fatalf("%s = %q, want %q: %s", path, got, v, out)
		}
	}
	// A page with no date says so, as null, not by leaving the field out.
	if gjson.Get(out, "_websearch.results.1.page_date").Raw != "null" {
		t.Fatalf("page_date: %s", gjson.Get(out, "_websearch.results.1").Raw)
	}
	// The key went into the one header, and nowhere into the answer.
	req := f.last()
	if req.Header.Get("X-Subscription-Token") != "sk-acme" || !strings.HasPrefix(req.URL, "https://brave.test/res/v1/web/search?") || !strings.Contains(req.URL, "country=FR") || !strings.Contains(req.URL, "search_lang=fr") || !strings.Contains(req.URL, "freshness=pm") || !strings.Contains(req.URL, "count=5") {
		t.Fatalf("request = %+v", req)
	}
	if !strings.HasPrefix(req.UserAgent, "txco-search/") {
		t.Fatalf("user agent = %q", req.UserAgent)
	}
	if strings.Contains(out, "sk-acme") {
		t.Fatalf("the key is in the answer: %s", out)
	}
}

// kind news asks the news index and answers in the same shape, saying so.
func TestWebSearchNews(t *testing.T) {
	f := &wsFetch{res: braveAnswer(`{"type": "news", "query": {"original": "site:nytimes.com"}, "results": [
		{"title": "Trump Rallies Red States", "url": "https://www.nytimes.com/2026/10/19/us/politics/trump.html", "description": "d", "age": "3 hours ago", "page_age": "2026-10-19T19:05:00"}
	]}`)}
	out, tr := callWSCtx(t, context.Background(), newWSDeps(t, f, nil), webSearchName, "acme", `{"query": "site:nytimes.com", "kind": "news", "freshness": "day", "count": 5}`)
	want := map[string]string{
		"_websearch.ok":                  "true",
		"_websearch.kind":                "news",
		"_websearch.count":               "1",
		"_websearch.results.0.title":     "Trump Rallies Red States",
		"_websearch.results.0.source":    "www.nytimes.com",
		"_websearch.results.0.page_date": "2026-10-19T19:05:00Z",
	}
	for path, v := range want {
		if got := gjson.Get(out, path).String(); got != v {
			t.Fatalf("%s = %q, want %q: %s", path, got, v, out)
		}
	}
	if req := f.last(); !strings.HasPrefix(req.URL, "https://brave.test/res/v1/news/search?") || !strings.Contains(req.URL, "freshness=pd") || strings.Contains(req.URL, "result_filter") {
		t.Fatalf("request = %s", req.URL)
	}
	if ev := tr.last(); ev.Fields["kind"] != "news" || ev.Fields["results"] != 1 {
		t.Fatalf("event = %v", ev.Fields)
	}
}

func TestWebSearchInto(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	out := callWS(t, newWSDeps(t, f, nil), "acme", `{"query": "q", "into": "_found"}`)
	if !gjson.Get(out, "_found.ok").Bool() || gjson.Get(out, "_websearch").Exists() {
		t.Fatal(out)
	}
	// A reserved target falls back to the default rather than failing.
	out = callWS(t, newWSDeps(t, f, nil), "acme", `{"query": "q", "into": "_txc.tenant"}`)
	if !gjson.Get(out, "_websearch.ok").Bool() {
		t.Fatal(out)
	}
}

func TestWebSearchEmptyResultsIsSuccess(t *testing.T) {
	f := &wsFetch{res: braveAnswer(`{"type": "search", "query": {"original": "q"}}`)}
	out := callWS(t, newWSDeps(t, f, nil), "acme", `{"query": "q"}`)
	if !gjson.Get(out, "_websearch.ok").Bool() || gjson.Get(out, "_websearch.results").Raw != "[]" || gjson.Get(out, "_websearch.count").Int() != 0 {
		t.Fatal(out)
	}
}

func TestWebSearchRefusesBeforeFetching(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	cases := []struct {
		name, tenant, meta, code string
		mod                      func(*webSearchDeps)
	}{
		{webSearchName, "", `{"query": "q"}`, "txco_web_no_tenant", nil},
		{"txco://web/searchx", "acme", `{"query": "q"}`, "txco_web_invalid_request", nil},
		{"txco://web/search/v2", "acme", `{"query": "q"}`, "txco_web_invalid_request", nil},
		{webSearchName, "acme", `{}`, "txco_web_invalid_request", nil},
		{webSearchName, "acme", `{"query": "q", "freshnes": "day"}`, "txco_web_invalid_request", nil},
		{webSearchName, "acme", `{"query": "q"}`, "txco_web_disabled", func(d *webSearchDeps) { d.enabled = false }},
		{webSearchName, "other", `{"query": "q"}`, "txco_web_missing_secret", nil},
		{webSearchName, "acme", `{"query": "q"}`, "txco_web_internal_error", func(d *webSearchDeps) {
			d.secrets = &wsSecrets{err: fmt.Errorf("store down")}
		}},
	}
	for _, c := range cases {
		f.reqs = nil
		out, _ := callWSCtx(t, context.Background(), newWSDeps(t, f, c.mod), c.name, c.tenant, c.meta)
		if gjson.Get(out, "_websearch.ok").Bool() || gjson.Get(out, "_websearch.error.code").String() != c.code {
			t.Fatalf("%s %s %s: %s", c.name, c.tenant, c.meta, out)
		}
		if len(f.reqs) != 0 {
			t.Fatalf("%s: fetched before refusing", c.code)
		}
	}
	// The missing-secret message tells the author what to run, and names
	// no value.
	out := callWS(t, newWSDeps(t, f, nil), "other", `{"query": "q"}`)
	if msg := gjson.Get(out, "_websearch.error.message").String(); !strings.Contains(msg, "txco auth tenant secrets set BRAVE_SEARCH_KEY") {
		t.Fatalf("message = %q", msg)
	}
}

func TestWebSearchKeyScopes(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	store := &wsSecrets{values: map[string][]byte{
		"acme//BRAVE_SEARCH_KEY":    []byte("sk-tenant"),
		"acme/web/BRAVE_SEARCH_KEY": []byte("sk-web"),
	}}
	d := newWSDeps(t, f, func(d *webSearchDeps) { d.secrets = store })

	// The stack the rule is deployed in is asked for first.
	ctx := processor.WithStack(context.Background(), "web")
	callWSCtx(t, ctx, d, webSearchName, "acme", `{"query": "q"}`)
	if f.last().Header.Get("X-Subscription-Token") != "sk-web" || store.asked[len(store.asked)-1] != "acme/web/BRAVE_SEARCH_KEY" {
		t.Fatalf("stack scope: header %q asked %v", f.last().Header.Get("X-Subscription-Token"), store.asked)
	}
	ctx = processor.WithStack(context.Background(), "mail")
	callWSCtx(t, ctx, d, webSearchName, "acme", `{"query": "q"}`)
	if f.last().Header.Get("X-Subscription-Token") != "sk-tenant" {
		t.Fatal("tenant-wide value should answer for a stack with no value of its own")
	}

	// The env fallback: only when the store has nothing, and only when on.
	t.Setenv("BRAVE_SEARCH_KEY", "sk-env")
	d.env = false
	out := callWS(t, d, "other", `{"query": "q"}`)
	if gjson.Get(out, "_websearch.error.code").String() != "txco_web_missing_secret" {
		t.Fatalf("env off: %s", out)
	}
	d.env = true
	out = callWS(t, d, "other", `{"query": "q"}`)
	if !gjson.Get(out, "_websearch.ok").Bool() || f.last().Header.Get("X-Subscription-Token") != "sk-env" {
		t.Fatalf("env on: %s", out)
	}
	callWS(t, d, "acme", `{"query": "q"}`)
	if f.last().Header.Get("X-Subscription-Token") != "sk-tenant" {
		t.Fatal("the store beats the environment")
	}
	// No store at all: the environment alone.
	d.secrets = nil
	callWS(t, d, "acme", `{"query": "q"}`)
	if f.last().Header.Get("X-Subscription-Token") != "sk-env" {
		t.Fatal("no store: the environment should answer")
	}
}

func TestWebSearchProviderErrors(t *testing.T) {
	cases := []struct {
		err        *fetch.Error
		code       string
		status     int
		retryAfter int
	}{
		{&fetch.Error{Code: fetch.CodeHTTPError, Status: 401}, "txco_web_provider_auth", 401, 0},
		{&fetch.Error{Code: fetch.CodeHTTPError, Status: 422}, "txco_web_invalid_request", 422, 0},
		{&fetch.Error{Code: fetch.CodeHTTPError, Status: 429, Header: http.Header{"X-Ratelimit-Remaining": {"0, 10"}, "X-Ratelimit-Reset": {"3, 100"}}}, "txco_web_rate_limited", 429, 3},
		{&fetch.Error{Code: fetch.CodeHTTPError, Status: 503}, "txco_web_provider_error", 503, 0},
		{&fetch.Error{Code: fetch.CodeRedirectLimit}, "txco_web_provider_error", 0, 0},
		{&fetch.Error{Code: fetch.CodeDestinationDenied, Message: "x"}, "txco_web_destination_denied", 0, 0},
		{&fetch.Error{Code: fetch.CodeDNSFailed, Message: "x"}, "txco_web_dns_failed", 0, 0},
		{&fetch.Error{Code: fetch.CodeTimeout, Message: "x"}, "txco_web_timeout", 0, 0},
		{&fetch.Error{Code: fetch.CodeResponseTooLarge, Message: "x"}, "txco_web_response_too_large", 0, 0},
		{&fetch.Error{Code: fetch.CodeUnsupportedType, Status: 200}, "txco_web_invalid_response", 200, 0},
	}
	for _, c := range cases {
		f := &wsFetch{err: c.err}
		out := callWS(t, newWSDeps(t, f, nil), "acme", `{"query": "q"}`)
		e := gjson.Get(out, "_websearch.error")
		if gjson.Get(out, "_websearch.ok").Bool() || e.Get("code").String() != c.code || int(e.Get("status").Int()) != c.status || int(e.Get("retry_after").Int()) != c.retryAfter {
			t.Fatalf("%s: %s", c.err.Code, out)
		}
		if gjson.Get(out, "_websearch.query").String() != "q" || gjson.Get(out, "_websearch.provider").String() != "brave" {
			t.Fatalf("%s: query and provider belong on a failure too: %s", c.err.Code, out)
		}
	}
	f := &wsFetch{res: braveAnswer(`<html>`)}
	out := callWS(t, newWSDeps(t, f, nil), "acme", `{"query": "q"}`)
	if gjson.Get(out, "_websearch.error.code").String() != "txco_web_invalid_response" {
		t.Fatal(out)
	}
}

func TestWebSearchRateLimit(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	d := newWSDeps(t, f, func(d *webSearchDeps) { d.rate = throttle.New(2, time.Minute) })
	for i := 0; i < 2; i++ {
		if out := callWS(t, d, "acme", `{"query": "q"}`); !gjson.Get(out, "_websearch.ok").Bool() {
			t.Fatal(out)
		}
	}
	out := callWS(t, d, "acme", `{"query": "q"}`)
	if gjson.Get(out, "_websearch.error.code").String() != "txco_web_rate_limited" {
		t.Fatal(out)
	}
	if out := callWS(t, d, "other-with-key", `{"query": "q"}`); gjson.Get(out, "_websearch.error.code").String() == "txco_web_rate_limited" {
		t.Fatal("the rate is per tenant")
	}
}

func TestWebSearchTenantInFlightCap(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody), hold: make(chan struct{})}
	d := newWSDeps(t, f, func(d *webSearchDeps) { d.tenantMax = 1; d.slots = make(chan struct{}, 8) })
	done := make(chan string, 1)
	go func() { done <- callWS(t, d, "acme", `{"query": "q"}`) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.reqs)
		f.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	out := callWS(t, d, "acme", `{"query": "q"}`)
	if gjson.Get(out, "_websearch.error.code").String() != "txco_web_busy" {
		t.Fatal(out)
	}
	close(f.hold)
	if out := <-done; !gjson.Get(out, "_websearch.ok").Bool() {
		t.Fatal(out)
	}
}

func TestWebSearchNodeSlotsWaitUnderTheDeadline(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	d := newWSDeps(t, f, func(d *webSearchDeps) { d.slots = make(chan struct{}, 1) })
	d.slots <- struct{}{} // the one slot is taken
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out, _ := callWSCtx(t, ctx, d, webSearchName, "acme", `{"query": "q"}`)
	if gjson.Get(out, "_websearch.error.code").String() != "txco_web_busy" || len(f.reqs) != 0 {
		t.Fatal(out)
	}
}

func TestWebSearchCompletionEventCarriesNoQuery(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	out, tr := callWSCtx(t, context.Background(), newWSDeps(t, f, nil), webSearchName, "acme", `{"query": "s3cret plans for acme"}`)
	if !gjson.Get(out, "_websearch.ok").Bool() {
		t.Fatal(out)
	}
	ev := tr.last()
	if ev.Event != "web.search.completion" {
		t.Fatalf("event = %s", ev.Event)
	}
	if ev.Fields["provider"] != "brave" || ev.Fields["status"] != 200 || ev.Fields["results"] != 2 || ev.Fields["query_sha256"] == "" || ev.Fields["kind"] != "web" {
		t.Fatalf("fields = %v", ev.Fields)
	}
	for k, v := range ev.Fields {
		s := fmt.Sprint(v)
		if strings.Contains(s, "s3cret") || strings.Contains(s, "acme") || strings.Contains(s, "example.fr") || strings.Contains(s, "sk-") {
			t.Fatalf("field %s = %q leaks", k, s)
		}
	}
	f.err, f.res = &fetch.Error{Code: fetch.CodeHTTPError, Status: 401}, nil
	_, tr = callWSCtx(t, context.Background(), newWSDeps(t, f, nil), webSearchName, "acme", `{"query": "q"}`)
	if ev := tr.last(); ev.Fields["error_code"] != "txco_web_provider_auth" || ev.Fields["status"] != 401 {
		t.Fatalf("error event = %v", ev.Fields)
	}
}

// newWSUnit builds a processor with an in-memory ops table and the
// web/search handler registered, for running real rules.
func newWSUnit(t *testing.T, d *webSearchDeps) *processor.Unit {
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
	pu.Handle([]byte("txco://web/search"), event.OpsHandlerFunc(
		func(ctx context.Context, opName string, in, out []byte) (event.Payload, error) {
			return webSearch(ctx, d, opName, in)
		}))
	return pu
}

// A rule calls the op through real dispatch: WITH from txcl, the answer
// merged into the envelope, the stack pinned for the key, the fuel on the
// request: the scope entry (10), then the op's own 25 + 100 + 50.
func TestWebSearchThroughDispatch(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	store := &wsSecrets{values: map[string][]byte{"acme/news/BRAVE_SEARCH_KEY": []byte("sk-news")}}
	pu := newWSUnit(t, newWSDeps(t, f, func(d *webSearchDeps) { d.secrets = store }))
	rule := `WHEN .go == true
  WITH query = &concat("French employment law ", .month),
       count = 8,
       country = "FR",
       language = "fr",
       freshness = "month",
       into = "_found"
  EXEC "txco://web/search"`
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
	envelope := `{"go": true, "month": "October", "_txc": {"tenant": "acme", "src": "http"}}`
	if err := pu.Run(context.Background(), envelope, "news/0", resCh); err != nil {
		t.Fatal(err)
	}
	out := (<-resCh).Raw
	if !gjson.Get(out, "_found.ok").Bool() || gjson.Get(out, "_found.query").String() != "French employment law October" || gjson.Get(out, "_found.results.0.source").String() != "example.fr" {
		t.Fatalf("envelope: %s", out)
	}
	if req := f.last(); req.Header.Get("X-Subscription-Token") != "sk-news" || !strings.Contains(req.URL, "count=8") {
		t.Fatalf("request = %+v (asked %v)", req, store.asked)
	}
	if fuel := processor.FuelUsedFromEnvelope(out); fuel != 10+25+100+50 {
		t.Fatalf("fuel used = %d, want 185 (10 for the scope, 175 for the search): %s", fuel, out)
	}
	if strings.Contains(out, "sk-news") {
		t.Fatalf("the key is in the envelope: %s", out)
	}
}

// A typo'd name is refused by the handler even though the prefix match
// reaches it.
func TestWebSearchPrefixNameRefused(t *testing.T) {
	f := &wsFetch{res: braveAnswer(braveBody)}
	pu := newWSUnit(t, newWSDeps(t, f, nil))
	rule := `WHEN .go == true
  WITH query = "q"
  EXEC "txco://web/searching"`
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
	if err := pu.Run(context.Background(), `{"go": true, "_txc": {"tenant": "acme", "src": "http"}}`, "news/0", resCh); err != nil {
		t.Fatal(err)
	}
	out := (<-resCh).Raw
	if gjson.Get(out, "_websearch.error.code").String() != "txco_web_invalid_request" || len(f.reqs) != 0 {
		t.Fatalf("envelope: %s", out)
	}
}
