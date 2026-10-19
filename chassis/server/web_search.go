package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/fetch"
	"github.com/loremlabs/thanks-computer/chassis/metrics"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/trace"
	"github.com/loremlabs/thanks-computer/chassis/websearch"
	"github.com/loremlabs/thanks-computer/chassis/websearch/brave"
)

// txco://web/search asks a public search engine one question under the
// tenant's own key and returns titles, addresses and snippets
// (docs/advanced/web-search.md).
//
//	WITH query, count?, country?, language?, freshness?, safesearch?, into? (default _websearch)
//
// The key is the tenant's, found the way the ai:// ops find theirs: the
// provider's RequiredSecrets() (BRAVE_SEARCH_KEY) in the tenant's secret
// store, a stack-scoped value beating a tenant-wide one, then the
// environment when --ai-chat-env-fallback allows. It goes into the one
// header the provider reads it from, on a request built by the provider
// and sent through chassis/fetch: the egress guard on every address, no
// redirects, 1 MiB, --web-search-timeout. Per-node slots, a per-tenant
// in-flight cap and a per-tenant rate bound it (the --web-search-*
// flags). Like EXEC "https://…", any rule may call it.
//
// The answer is in-band at `into`, with a nil Go error either way:
//
//	{ok: true, query, provider, results: [{title, url, description, page_date, source}…], count, truncated}
//	{ok: false, query?, provider?, error: {code: "txco_web_<code>", message, status?, retry_after?}}
type webSearchDeps struct {
	enabled  bool
	provider websearch.Provider
	fetcher  interface {
		Get(context.Context, fetch.Request) (*fetch.Result, error)
	}
	// secrets is the tenant store; nil when the chassis has none. env is
	// --ai-chat-env-fallback.
	secrets webSearchSecrets
	env     bool
	mc      *metrics.Metrics

	slots     chan struct{} // requests in flight on this node
	tenantMax int
	rate      *throttle.Throttle

	mu       sync.Mutex
	inflight map[string]int
}

// webSearchSecrets is the part of secrets.Resolver the op uses.
type webSearchSecrets interface {
	MaterializeForOpSlug(ctx context.Context, tenantSlug, stack, name string) ([]byte, *secrets.SecretMetadata, error)
}

// webSearchMaxBytes caps the provider's answer. A page of twenty results
// is a few tens of KiB.
const webSearchMaxBytes = 1 << 20

func newWebSearchDeps(conf config.Config, guard egress.Guard, sec *secrets.Resolver, mc *metrics.Metrics) (*webSearchDeps, error) {
	timeout, err := time.ParseDuration(conf.WebSearchTimeout)
	if err != nil {
		return nil, fmt.Errorf("web-search-timeout: %w", err)
	}
	p, err := websearch.Open(brave.Name, websearch.Config{BraveURL: conf.WebSearchBraveURL})
	if err != nil {
		return nil, err
	}
	f, err := fetch.New(fetch.Options{
		Guard:        guard,
		MaxBytes:     webSearchMaxBytes,
		MaxRedirects: -1, // a provider redirect is an error, and the key never travels
		Timeout:      timeout,
		MediaTypes:   []string{"application/json"},
		Accept:       "application/json",
	})
	if err != nil {
		return nil, err
	}
	d := &webSearchDeps{
		enabled:   conf.WebSearch,
		provider:  p,
		fetcher:   f,
		env:       conf.AIChatEnvFallback,
		mc:        mc,
		slots:     make(chan struct{}, conf.WebSearchConcurrency),
		tenantMax: conf.WebSearchTenantConcurrency,
		rate:      throttle.New(conf.WebSearchRatePerMin, time.Minute),
		inflight:  map[string]int{},
	}
	if sec != nil {
		d.secrets = sec
	}
	return d, nil
}

// webSearchName is the one name the handler answers to. Builtins resolve
// by longest prefix, so without this check txco://web/searchx would reach
// it too.
const webSearchName = "txco://web/search"

// webSearchRun is one call's bookkeeping, for the completion event.
type webSearchRun struct {
	start   time.Time
	status  int
	results int
	bytes   int
}

func webSearch(ctx context.Context, d *webSearchDeps, opName string, in []byte) (event.Payload, error) {
	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, websearch.DefaultInto)
	run := &webSearchRun{start: time.Now()}
	tenant := processor.TenantScope(ctx)
	var req websearch.Request
	fail := func(e *websearch.Error) (event.Payload, error) {
		e.Code = "txco_web_" + e.Code
		run.status = e.Status
		emitWebSearchEvent(ctx, d, tenant, req.Query, run, false, e.Code)
		return webSearchPayload(into, websearch.Failure{Query: req.Query, Provider: d.provider.Name(), Error: e})
	}
	failf := func(code, format string, args ...any) (event.Payload, error) {
		return fail(websearch.Errorf(code, format, args...))
	}

	if opName != webSearchName {
		return failf(websearch.CodeInvalidRequest, "there is no operation %q; did you mean %s?", opName, webSearchName)
	}
	if tenant == "" {
		return failf(websearch.CodeNoTenant, "no tenant in request scope")
	}
	if !d.enabled {
		return failf(websearch.CodeDisabled, "web search is turned off on this chassis")
	}
	var perr *websearch.Error
	req, perr = websearch.Parse(meta)
	if perr != nil {
		return fail(perr)
	}

	if ok, _ := d.rate.Allow(tenant); !ok {
		return failf(websearch.CodeRateLimited, "this tenant has started too many searches on this node in the last minute")
	}
	if !d.enter(tenant) {
		return failf(websearch.CodeBusy, "this tenant already has %d searches in flight on this node", d.tenantMax)
	}
	defer d.leave(tenant)

	stage := gjson.GetBytes(in, "_txc.op").String()
	key, kerr := d.key(ctx, tenant, processor.StackScope(ctx), stage)
	if kerr != nil {
		var miss *webSearchMissing
		if errors.As(kerr, &miss) {
			return failf(websearch.CodeMissingSecret, "the secret %s is not set for this tenant; `txco auth tenant secrets set %s`", miss.name, miss.name)
		}
		return failf(websearch.CodeInternal, "the tenant's key could not be read")
	}
	defer secrets.Zero(key)

	select {
	case d.slots <- struct{}{}:
	case <-ctx.Done():
		return failf(websearch.CodeBusy, "every search slot on this node stayed busy until the op's deadline")
	}
	_ = processor.AddFuel(ctx, processor.FuelCostWebSearch, stage)
	freq, berr := d.provider.Build(req, key)
	if berr != nil {
		<-d.slots
		return failf(websearch.CodeInternal, "the search request could not be built")
	}
	freq.UserAgent = webSearchUserAgent(ctx)
	res, err := d.fetcher.Get(ctx, freq)
	<-d.slots
	if err != nil {
		var fe *fetch.Error
		if !errors.As(err, &fe) {
			return failf(websearch.CodeInternal, "the search failed unexpectedly")
		}
		return fail(webSearchFetchError(d, fe))
	}
	run.status, run.bytes = res.Status, len(res.Body)
	raw, err := d.provider.Parse(res.Body)
	if err != nil {
		return failf(websearch.CodeInvalidResponse, "the search provider's answer was not what this op reads")
	}
	results, truncated := websearch.Normalize(raw)
	run.results = len(results)
	ans := websearch.Answer{OK: true, Query: req.Query, Provider: d.provider.Name(), Results: results, Count: len(results), Truncated: truncated}
	emitWebSearchEvent(ctx, d, tenant, req.Query, run, truncated, "")
	return webSearchPayload(into, ans)
}

// webSearchFetchError maps the fetcher's error to the op's. A provider
// redirect is refused outright (MaxRedirects -1), so it reads as the
// provider's failure, not the fetcher's.
func webSearchFetchError(d *webSearchDeps, fe *fetch.Error) *websearch.Error {
	switch fe.Code {
	case fetch.CodeHTTPError:
		return d.provider.Failure(fe.Status, fe.Header)
	case fetch.CodeDestinationDenied, fetch.CodeDNSFailed, fetch.CodeConnectionFailed, fetch.CodeTimeout, fetch.CodeResponseTooLarge:
		return &websearch.Error{Code: fe.Code, Message: fe.Message}
	case fetch.CodeRedirectLimit:
		return &websearch.Error{Code: websearch.CodeProviderError, Message: "the search provider redirected the request"}
	case fetch.CodeUnsupportedType, fetch.CodeUnsupportedEncoding:
		return &websearch.Error{Code: websearch.CodeInvalidResponse, Message: "the search provider's answer was not JSON", Status: fe.Status}
	}
	return &websearch.Error{Code: websearch.CodeInternal, Message: "the search failed unexpectedly"}
}

// webSearchMissing is the not-found error with the secret's name on it.
type webSearchMissing struct{ name string }

func (e *webSearchMissing) Error() string { return "websearch: secret " + e.name + " not found" }
func (e *webSearchMissing) Is(target error) bool {
	return target == secrets.ErrSecretNotFound
}

// key materializes the provider's first required secret for the tenant,
// the ai:// chain: the store (stack scope beats tenant-wide), then the
// environment when --ai-chat-env-fallback allows. Found, it is paid for
// and counted as any materialized secret is. The caller zeroes it.
func (d *webSearchDeps) key(ctx context.Context, tenant, stack, stage string) ([]byte, error) {
	names := d.provider.RequiredSecrets()
	if len(names) == 0 {
		return nil, nil
	}
	name := names[0]
	var key []byte
	if d.secrets != nil {
		cleartext, _, err := d.secrets.MaterializeForOpSlug(ctx, tenant, stack, name)
		switch {
		case err == nil:
			key = cleartext
		case !errors.Is(err, secrets.ErrSecretNotFound):
			return nil, err
		}
	}
	if key == nil && d.env {
		if v := os.Getenv(name); v != "" {
			key = []byte(v)
		}
	}
	if key == nil {
		return nil, &webSearchMissing{name: name}
	}
	_ = processor.AddFuel(ctx, processor.FuelCostSecretMaterialize, stage)
	d.mc.RecordSecretMaterialize(ctx, tenant, name)
	return key, nil
}

func (d *webSearchDeps) enter(tenant string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[tenant] >= d.tenantMax {
		return false
	}
	d.inflight[tenant]++
	return true
}

func (d *webSearchDeps) leave(tenant string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inflight[tenant] <= 1 {
		delete(d.inflight, tenant)
		return
	}
	d.inflight[tenant]--
}

func webSearchPayload(into string, v any) (event.Payload, error) {
	b, _ := json.Marshal(v)
	raw, _ := sjson.SetRaw(`{}`, into, string(b))
	return event.Payload{Raw: raw, Type: event.JSON}, nil
}

func webSearchUserAgent(ctx context.Context) string {
	v, _ := ctx.Value(config.CtxKeyVersion).(string)
	if v == "" {
		v = "dev"
	}
	return "txco-search/" + v + " (+https://www.thanks.computer)"
}

// emitWebSearchEvent records one call on the trace timeline and the
// websearch.requests counter. Timeline events are not redacted, so the
// event carries a short hash of the query, never the query or a result.
func emitWebSearchEvent(ctx context.Context, d *webSearchDeps, tenant, query string, run *webSearchRun, truncated bool, errCode string) {
	outcome := "ok"
	if errCode != "" {
		outcome = errCode[len("txco_web_"):]
	}
	d.mc.RecordWebSearch(ctx, tenant, outcome)
	tr := trace.FromContext(ctx)
	if tr == nil {
		return
	}
	fields := map[string]any{
		"provider":    d.provider.Name(),
		"duration_ms": time.Since(run.start).Milliseconds(),
	}
	if query != "" {
		sum := sha256.Sum256([]byte(query))
		fields["query_sha256"] = hex.EncodeToString(sum[:8])
	}
	if run.status != 0 {
		fields["status"] = run.status
	}
	if errCode != "" {
		fields["error_code"] = errCode
	} else {
		fields["results"] = run.results
		fields["output_bytes"] = run.bytes
		if truncated {
			fields["truncated"] = true
		}
	}
	tr.Event(trace.TimelineEvent{Ts: time.Now(), Event: "web.search.completion", Fields: fields})
}
