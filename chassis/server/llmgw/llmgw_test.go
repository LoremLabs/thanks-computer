package llmgw

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	_ "github.com/loremlabs/thanks-computer/chassis/egress/open" // registers the "open" policy
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
	"github.com/loremlabs/thanks-computer/chassis/usage"
)

type fakeUsage struct {
	mu     sync.Mutex
	events []usage.UsageEvent
}

func (f *fakeUsage) WriteEvent(ev usage.UsageEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}
func (f *fakeUsage) Name() string                { return "fake" }
func (f *fakeUsage) Close(context.Context) error { return nil }
func (f *fakeUsage) snapshot() []usage.UsageEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]usage.UsageEvent(nil), f.events...)
}

// echoStack is the do-nothing _llm stack: the final envelope is the
// request envelope unchanged.
func echoStack(env *event.Envelope) {
	env.ResCh <- event.Payload{Raw: env.Payload.Raw, Type: event.JSON}
}

type gwEnv struct {
	g           *Gateway
	usage       *fakeUsage
	completions chan string // raw completion-phase payloads, as seen on the bus
}

// newTestGateway wires a Gateway against a fake bus. stackFn plays the
// request-phase pipeline; completion-phase envelopes are captured on the
// completions channel and acked. Seams default to a routed, opted-in
// tenant "acme" with no secrets (passthrough); tests override as needed.
func newTestGateway(t *testing.T, upstreamURL string, stackFn func(env *event.Envelope)) *gwEnv {
	t.Helper()
	guard, err := egress.Open("open", egress.Config{})
	if err != nil {
		t.Fatalf("egress.Open: %v", err)
	}
	bus := make(chan *event.Envelope)
	fu := &fakeUsage{}
	pu := &processor.Unit{
		Conf: config.Config{
			LLMUpstreamURL:      upstreamURL,
			LLMContextMaxTokens: 2000,
			LLMContextMaxItems:  8,
			OpTimeoutMax:        "5s",
			WebWriteTimeout:     5,
			WebMaxBodyBytes:     1 << 20,
		},
		Logger: zap.NewNop(),
		Usage:  fu,
	}
	pu.Bus = bus
	g, err := New(context.Background(), pu, nil, guard)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.resolveHost = func(host string) (ingress.RouteTarget, bool, error) {
		return ingress.RouteTarget{Tenant: "acme", Stack: "app", Ingress: "host:" + host, Verified: true}, true, nil
	}
	g.stackExists = func(ctx context.Context, tenant string) (bool, error) { return true, nil }
	g.secret = func(ctx context.Context, tenant, name string) ([]byte, error) {
		return nil, secrets.ErrSecretNotFound
	}
	completions := make(chan string, 4)
	go func() {
		for env := range bus {
			if gjson.Get(env.Payload.Raw, "_txc.llm.phase").String() == phaseCompleted {
				completions <- env.Payload.Raw
				env.ResCh <- event.Payload{Raw: env.Payload.Raw, Type: event.JSON}
				continue
			}
			stackFn(env)
		}
	}()
	return &gwEnv{g: g, usage: fu, completions: completions}
}

func doReq(t *testing.T, g *Gateway, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return doReqPath(t, g.HandleMessages, "/v1/messages", body, hdr)
}

func doReqPath(t *testing.T, h http.HandlerFunc, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://gw.example"+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h(rr, req)
	return rr
}

func assertAnthropicError(t *testing.T, rr *httptest.ResponseRecorder, status int, errType string) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status = %d, want %d (body %s)", rr.Code, status, rr.Body.String())
	}
	body := rr.Body.String()
	if got := gjson.Get(body, "type").String(); got != "error" {
		t.Errorf("body.type = %q, want error (body %s)", got, body)
	}
	if got := gjson.Get(body, "error.type").String(); got != errType {
		t.Errorf("body.error.type = %q, want %q", got, errType)
	}
	if gjson.Get(body, "error.message").String() == "" {
		t.Errorf("body.error.message empty")
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
}

func waitCompletion(t *testing.T, env *gwEnv) string {
	t.Helper()
	select {
	case raw := <-env.completions:
		return raw
	case <-time.After(3 * time.Second):
		t.Fatalf("no completion envelope arrived")
		return ""
	}
}

// TestPassthroughTransparent: no tenant secrets ⇒ the client's own
// credentials and headers ride through verbatim, and the upstream's
// status/headers/body come back untouched. Completion metadata records
// the transfer.
func TestPassthroughTransparent(t *testing.T) {
	var gotKey, gotVersion, gotPath atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("x-api-key"))
		gotVersion.Store(r.Header.Get("anthropic-version"))
		gotPath.Store(r.URL.Path)
		w.Header().Set("Request-Id", "req_up_1")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "99")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"hi"}]}`))
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, echoStack)
	rr := doReq(t, env.g, `{"model":"claude-sonnet-4-5","max_tokens":8,"messages":[]}`, map[string]string{
		"x-api-key":         "sk-client-key",
		"anthropic-version": "2023-06-01",
		"x-request-id":      "rid-test-1",
	})

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"id":"msg_1","content":[{"type":"text","text":"hi"}]}` {
		t.Errorf("body = %s", got)
	}
	if got := rr.Header().Get("Request-Id"); got != "req_up_1" {
		t.Errorf("upstream header lost: request-id = %q", got)
	}
	if got := rr.Header().Get("Anthropic-Ratelimit-Requests-Remaining"); got != "99" {
		t.Errorf("upstream header lost: ratelimit = %q", got)
	}
	if got := gotKey.Load(); got != "sk-client-key" {
		t.Errorf("upstream x-api-key = %v, want client key (passthrough)", got)
	}
	if got := gotVersion.Load(); got != "2023-06-01" {
		t.Errorf("upstream anthropic-version = %v", got)
	}
	if got := gotPath.Load(); got != "/v1/messages" {
		t.Errorf("upstream path = %v", got)
	}

	raw := waitCompletion(t, env)
	if got := gjson.Get(raw, "_txc.llm.completion.status").Int(); got != 200 {
		t.Errorf("completion.status = %d", got)
	}
	if got := gjson.Get(raw, "_txc.llm.completion.bytes_out").Int(); got == 0 {
		t.Errorf("completion.bytes_out = 0, want > 0")
	}
	if got := gjson.Get(raw, "_txc.llm.completion.requested_model").String(); got != "claude-sonnet-4-5" {
		t.Errorf("completion.requested_model = %q", got)
	}
	if got := gjson.Get(raw, "_txc.llm.completion.effective_request_model").String(); got != "claude-sonnet-4-5" {
		t.Errorf("completion.effective_request_model = %q", got)
	}
	// The completion envelope runs under its OWN rid — the trace store
	// keys a run by rid, and sharing the request's rid overwrote the
	// request-phase trace. Correlation rides _txc.llm.request_id.
	if got := gjson.Get(raw, "_txc.llm.request_id").String(); got != "rid-test-1" {
		t.Errorf("completion request_id = %q, want the request rid", got)
	}
	if crid := gjson.Get(raw, "_txc.rid").String(); crid == "" || crid == "rid-test-1" {
		t.Errorf("completion _txc.rid = %q, want a fresh rid distinct from the request's", crid)
	}
	evs := env.usage.snapshot()
	if len(evs) != 1 {
		t.Fatalf("usage events = %d, want 1", len(evs))
	}
	if evs[0].Src != "llm" || evs[0].Stack != "_llm" || evs[0].Tenant != "acme" || evs[0].Status != "ok" {
		t.Errorf("usage event = %+v", evs[0])
	}
	if evs[0].RID != "rid-test-1" {
		t.Errorf("transfer usage RID = %q, want the request rid", evs[0].RID)
	}
}

// TestModelRewrite: the stack rewrites request.model and adds an
// upstream header; the upstream sees both, and the completion records
// the model actually forwarded.
func TestModelRewrite(t *testing.T) {
	var gotModel, gotPolicy atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotModel.Store(gjson.GetBytes(b, "model").String())
		gotPolicy.Store(r.Header.Get("x-txco-llm-policy"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "request.model", "claude-3-5-haiku-latest")
		raw, _ = sjson.Set(raw, "_txc.llm.headers.x-txco-llm-policy", "model-pinned")
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"claude-sonnet-4-5","messages":[]}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if got := gotModel.Load(); got != "claude-3-5-haiku-latest" {
		t.Errorf("upstream model = %v, want rewritten", got)
	}
	if got := gotPolicy.Load(); got != "model-pinned" {
		t.Errorf("upstream policy header = %v", got)
	}
	raw := waitCompletion(t, env)
	if got := gjson.Get(raw, "_txc.llm.completion.requested_model").String(); got != "claude-sonnet-4-5" {
		t.Errorf("completion.requested_model = %q, want the client's ask", got)
	}
	if got := gjson.Get(raw, "_txc.llm.completion.effective_request_model").String(); got != "claude-3-5-haiku-latest" {
		t.Errorf("completion.effective_request_model = %q, want the forwarded model", got)
	}
}

// TestStackReject: a _txc.llm.reject verdict returns the stack's shaped
// error and the upstream is never contacted.
func TestStackReject(t *testing.T) {
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "_txc.llm.reject.status", 403)
		raw, _ = sjson.Set(raw, "_txc.llm.reject.type", "permission_error")
		raw, _ = sjson.Set(raw, "_txc.llm.reject.message", "blocked by _llm policy")
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"m","messages":[]}`, nil)
	assertAnthropicError(t, rr, http.StatusForbidden, "permission_error")
	if got := gjson.Get(rr.Body.String(), "error.message").String(); got != "blocked by _llm policy" {
		t.Errorf("error.message = %q", got)
	}
	if n := upstreamCalls.Load(); n != 0 {
		t.Errorf("upstream called %d times on a rejected request", n)
	}
}

// TestAuthSwap: with ANTHROPIC_KEY configured, the inbound key must
// match LLM_GATEWAY_KEY; upstream sees the real key, never the gateway
// key, and a client Authorization header is dropped.
func TestAuthSwap(t *testing.T) {
	var gotKey, gotAuthz atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey.Store(r.Header.Get("x-api-key"))
		gotAuthz.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, echoStack)
	env.g.secret = func(ctx context.Context, tenant, name string) ([]byte, error) {
		switch name {
		case secretUpstreamKey:
			return []byte("sk-real-anthropic"), nil
		case secretGatewayKey:
			return []byte("gw-key-1"), nil
		}
		return nil, secrets.ErrSecretNotFound
	}

	rr := doReq(t, env.g, `{"model":"m"}`, map[string]string{
		"x-api-key":     "gw-key-1",
		"Authorization": "Bearer sk-client-oauth",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if got := gotKey.Load(); got != "sk-real-anthropic" {
		t.Errorf("upstream x-api-key = %v, want the swapped real key", got)
	}
	if got := gotAuthz.Load(); got != "" {
		t.Errorf("upstream Authorization = %v, want dropped in swap mode", got)
	}
	waitCompletion(t, env)

	// Wrong inbound key: 401, upstream untouched.
	before := gotKey.Load()
	rr = doReq(t, env.g, `{"model":"m"}`, map[string]string{"x-api-key": "wrong"})
	assertAnthropicError(t, rr, http.StatusUnauthorized, "authentication_error")
	if gotKey.Load() != before {
		t.Errorf("upstream was called on a 401 request")
	}

	// Missing inbound key entirely: 401.
	rr = doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusUnauthorized, "authentication_error")
}

// TestAuthSwapMisconfig: ANTHROPIC_KEY without LLM_GATEWAY_KEY fails
// closed (401) — the real key is never spent on an unauthenticated
// request.
func TestAuthSwapMisconfig(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", echoStack)
	env.g.secret = func(ctx context.Context, tenant, name string) ([]byte, error) {
		if name == secretUpstreamKey {
			return []byte("sk-real"), nil
		}
		return nil, secrets.ErrSecretNotFound
	}
	rr := doReq(t, env.g, `{"model":"m"}`, map[string]string{"x-api-key": "anything"})
	assertAnthropicError(t, rr, http.StatusUnauthorized, "authentication_error")
}

// TestSecretStoreFailure: a store failure (not absence) is a 503 — the
// gateway cannot know which auth mode applies, so it serves neither.
func TestSecretStoreFailure(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", echoStack)
	env.g.secret = func(ctx context.Context, tenant, name string) ([]byte, error) {
		return nil, errors.New("store timeout")
	}
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusServiceUnavailable, "api_error")
}

func TestNoTenant404(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", echoStack)
	env.g.resolveHost = func(host string) (ingress.RouteTarget, bool, error) {
		return ingress.RouteTarget{}, false, nil
	}
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusNotFound, "not_found_error")
}

func TestNoLLMStack404(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", echoStack)
	env.g.stackExists = func(ctx context.Context, tenant string) (bool, error) { return false, nil }
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusNotFound, "not_found_error")
}

func TestResolverFailure503(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", echoStack)
	env.g.resolveHost = func(host string) (ingress.RouteTarget, bool, error) {
		return ingress.RouteTarget{}, false, errors.New("mirror saturated")
	}
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusServiceUnavailable, "api_error")
	if rr.Header().Get("Retry-After") == "" {
		t.Errorf("503 should carry Retry-After")
	}
}

func TestBadJSON400(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", echoStack)
	rr := doReq(t, env.g, `{"model":`, nil)
	assertAnthropicError(t, rr, http.StatusBadRequest, "invalid_request_error")
	rr = doReq(t, env.g, `[1,2,3]`, nil)
	assertAnthropicError(t, rr, http.StatusBadRequest, "invalid_request_error")
}

func TestBodyTooLarge413(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", echoStack)
	env.g.pu.Conf.WebMaxBodyBytes = 16
	rr := doReq(t, env.g, `{"model":"a-very-long-model-name-that-overflows"}`, nil)
	assertAnthropicError(t, rr, http.StatusRequestEntityTooLarge, "request_too_large")
}

// TestAdmissionDenied: chassis admission denials (stamped on the
// pipeline result) are rendered Anthropic-shaped with Retry-After.
func TestAdmissionDenied(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "_txc.admission.denied", true)
		raw, _ = sjson.Set(raw, "_txc.admission.status", 429)
		raw, _ = sjson.Set(raw, "_txc.admission.reason", "rate_limited")
		raw, _ = sjson.Set(raw, "_txc.admission.retry_after", 2)
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusTooManyRequests, "rate_limit_error")
	if got := rr.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want 2", got)
	}
}

// TestStackStreamed500: a rule that streams (StreamHead/Chunk/End) is a
// misuse of the request phase — the chunks are drained so the processor
// unblocks, and the client gets a 500.
func TestStackStreamed500(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", func(e *event.Envelope) {
		e.ResCh <- event.Payload{Raw: `{"_txc":{"web":{"res":{"status":200}}}}`, Type: event.StreamHead}
		e.ResCh <- event.Payload{Raw: "chunk", Type: event.StreamChunk}
		e.ResCh <- event.Payload{Type: event.StreamEnd}
	})
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusInternalServerError, "api_error")
}

// TestPipelineError500 covers an ErrorStr pipeline result.
func TestPipelineError500(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", func(e *event.Envelope) {
		e.ResCh <- event.Payload{Raw: "boom", Type: event.ErrorStr}
	})
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusInternalServerError, "api_error")
}

// TestStackDroppedRequest500: the stack deleted/corrupted the `request`
// object; forwarding nothing would not be what the client asked for.
func TestStackDroppedRequest500(t *testing.T) {
	env := newTestGateway(t, "http://127.0.0.1:0", func(e *event.Envelope) {
		raw, _ := sjson.Delete(e.Payload.Raw, "request")
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusInternalServerError, "api_error")
}

// TestUpstreamErrorPassthrough: an upstream 4xx body/headers pass
// through verbatim — the gateway never rewrites what the real endpoint
// said.
func TestUpstreamErrorPassthrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"from upstream"}}`))
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, echoStack)
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if got := gjson.Get(rr.Body.String(), "error.message").String(); got != "from upstream" {
		t.Errorf("upstream error body rewritten: %s", rr.Body.String())
	}
	raw := waitCompletion(t, env)
	if got := gjson.Get(raw, "_txc.llm.completion.status").Int(); got != 429 {
		t.Errorf("completion.status = %d, want 429", got)
	}
	evs := env.usage.snapshot()
	if len(evs) != 1 || evs[0].Status != "error" {
		t.Errorf("usage = %+v, want one error event", evs)
	}
}

// TestUpstreamDialFailure502: nothing listening upstream ⇒ 502 with a
// completion record (an upstream attempt happened).
func TestUpstreamDialFailure502(t *testing.T) {
	// A closed port: grab one with a listener, then close it.
	l := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := l.URL
	l.Close()

	env := newTestGateway(t, deadURL, echoStack)
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusBadGateway, "api_error")
	raw := waitCompletion(t, env)
	if got := gjson.Get(raw, "_txc.llm.completion.status").Int(); got != 0 {
		t.Errorf("completion.status = %d, want 0 (no upstream response)", got)
	}
	if gjson.Get(raw, "_txc.llm.completion.error").String() == "" {
		t.Errorf("completion.error empty on dial failure")
	}
}

// TestStackUpstreamOverride: _txc.llm.upstream.url redirects the
// forward; an invalid override is a 500, not a silent fallback.
func TestStackUpstreamOverride(t *testing.T) {
	var altCalled atomic.Int64
	alt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		altCalled.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer alt.Close()

	env := newTestGateway(t, "http://127.0.0.1:0", func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "_txc.llm.upstream.url", alt.URL)
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if altCalled.Load() != 1 {
		t.Errorf("override upstream called %d times, want 1", altCalled.Load())
	}
	raw := waitCompletion(t, env)
	if got := gjson.Get(raw, "_txc.llm.completion.upstream").String(); got != alt.URL {
		t.Errorf("completion.upstream = %q, want %q", got, alt.URL)
	}

	env2 := newTestGateway(t, "http://127.0.0.1:0", func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "_txc.llm.upstream.url", "file:///etc/passwd")
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr = doReq(t, env2.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusInternalServerError, "api_error")
}

// readSSEEvent reads one blank-line-terminated SSE frame.
func readSSEEvent(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE frame: %v (so far %q)", err, sb.String())
		}
		if line == "\n" {
			return sb.String()
		}
		sb.WriteString(line)
	}
}

// TestSSEStreamsIncrementally proves no full-response buffering: the
// client receives the first SSE event while the upstream is still
// holding the stream open, gated on an explicit handshake.
func TestSSEStreamsIncrementally(t *testing.T) {
	firstEventRead := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		fl.Flush()
		select {
		case <-firstEventRead:
		case <-time.After(5 * time.Second):
			t.Error("client never read the first event — response is being buffered")
			return
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, echoStack)
	gw := httptest.NewServer(http.HandlerFunc(env.g.HandleMessages))
	defer gw.Close()

	resp, err := http.Post(gw.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	br := bufio.NewReader(resp.Body)
	if ev := readSSEEvent(t, br); !strings.Contains(ev, "message_start") {
		t.Fatalf("first frame = %q", ev)
	}
	close(firstEventRead)
	if ev := readSSEEvent(t, br); !strings.Contains(ev, "message_stop") {
		t.Fatalf("second frame = %q", ev)
	}
	raw := waitCompletion(t, env)
	if got := gjson.Get(raw, "_txc.llm.completion.stream").Bool(); !got {
		t.Errorf("completion.stream = false, want true")
	}
}

// TestClientCancelPropagates: the client walking away cancels the
// upstream request; the completion still fires, marked disconnected.
func TestClientCancelPropagates(t *testing.T) {
	upstreamSawCancel := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {}\n\n")
		fl.Flush()
		select {
		case <-r.Context().Done():
			close(upstreamSawCancel)
		case <-time.After(5 * time.Second):
			t.Error("upstream never saw the cancel")
		}
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, echoStack)
	gw := httptest.NewServer(http.HandlerFunc(env.g.HandleMessages))
	defer gw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/messages",
		strings.NewReader(`{"model":"m","stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	br := bufio.NewReader(resp.Body)
	readSSEEvent(t, br) // first event arrived; the stream is live
	cancel()
	resp.Body.Close()

	select {
	case <-upstreamSawCancel:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not propagate to the upstream")
	}
	raw := waitCompletion(t, env)
	if !gjson.Get(raw, "_txc.llm.completion.client_disconnected").Bool() {
		t.Errorf("completion.client_disconnected = false, want true; raw %s", raw)
	}
}

// TestRequestPayloadShape: chassis stamps under _txc; the client's
// bytes — including any _txc it tried to send — stay confined under
// `request`.
func TestRequestPayloadShape(t *testing.T) {
	hdr := http.Header{}
	hdr.Set("User-Agent", "claude-cli/1.0")
	hdr.Set("anthropic-version", "2023-06-01")
	body := []byte(`{"model":"m","_txc":{"tenant":"evil"},"stream":true}`)
	raw := requestPayload("rid1", "acme", true, "gw.example", body, true, authModePassthrough, hdr)

	checks := map[string]string{
		"_txc.src":                       "llm",
		"_txc.rid":                       "rid1",
		"_txc.llm.phase":                 "request",
		"_txc.llm.protocol":              "anthropic.messages",
		"_txc.llm.tenant":                "acme",
		"_txc.llm.host":                  "gw.example",
		"_txc.llm.auth_mode":             "passthrough",
		"_txc.llm.req.user_agent":        "claude-cli/1.0",
		"_txc.llm.req.anthropic_version": "2023-06-01",
		"request.model":                  "m",
		"request._txc.tenant":            "evil", // confined, harmless
	}
	for path, want := range checks {
		if got := gjson.Get(raw, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if gjson.Get(raw, "_txc.tenant").Exists() {
		t.Errorf("top-level _txc.tenant must not exist (client injection)")
	}
	if !gjson.Get(raw, "_txc.llm.hostname_verified").Bool() {
		t.Errorf("hostname_verified lost")
	}
}

// TestQueryStringPreserved: Claude Code appends ?beta=true — the
// forward must carry the client's query verbatim.
func TestQueryStringPreserved(t *testing.T) {
	var gotPath, gotQuery atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotQuery.Store(r.URL.RawQuery)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, echoStack)
	rr := doReqPath(t, env.g.HandleMessages, "/v1/messages?beta=true", `{"model":"m"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if got := gotPath.Load(); got != "/v1/messages" {
		t.Errorf("upstream path = %v", got)
	}
	if got := gotQuery.Load(); got != "beta=true" {
		t.Errorf("upstream query = %v, want beta=true", got)
	}
	waitCompletion(t, env)
}

// TestCountTokensPassthrough: the count_tokens forward never consults
// the _llm stack (a stack that rejects everything must not fire), hits
// the count_tokens path upstream, and records no completion — but auth
// still applies.
func TestCountTokensPassthrough(t *testing.T) {
	var gotPath, gotQuery atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotQuery.Store(r.URL.RawQuery)
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	}))
	defer upstream.Close()

	// A stack that rejects everything: if count_tokens consulted it,
	// the request would 403 instead of forwarding.
	env := newTestGateway(t, upstream.URL, func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "_txc.llm.reject.message", "should never fire")
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReqPath(t, env.g.HandleCountTokens, "/v1/messages/count_tokens?beta=true", `{"model":"m","messages":[]}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != `{"input_tokens":42}` {
		t.Errorf("body = %s", got)
	}
	if got := gotPath.Load(); got != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %v", got)
	}
	if got := gotQuery.Load(); got != "beta=true" {
		t.Errorf("upstream query = %v", got)
	}
	select {
	case raw := <-env.completions:
		t.Errorf("count_tokens recorded a completion: %s", raw)
	case <-time.After(150 * time.Millisecond):
	}
	if evs := env.usage.snapshot(); len(evs) != 0 {
		t.Errorf("count_tokens wrote usage events: %+v", evs)
	}

	// Auth still gates the stackless path: swap mode + wrong key ⇒ 401.
	env.g.secret = func(ctx context.Context, tenant, name string) ([]byte, error) {
		switch name {
		case secretUpstreamKey:
			return []byte("sk-real"), nil
		case secretGatewayKey:
			return []byte("gw-key"), nil
		}
		return nil, secrets.ErrSecretNotFound
	}
	rr = doReqPath(t, env.g.HandleCountTokens, "/v1/messages/count_tokens", `{"model":"m"}`, map[string]string{"x-api-key": "wrong"})
	assertAnthropicError(t, rr, http.StatusUnauthorized, "authentication_error")
}

// TestContextInjectionEndToEnd: the stack emits @llm.context; the
// upstream must RECEIVE original-system → guard → delimited blocks, and
// the completion must carry the context_result ground truth (sha256,
// never content).
func TestContextInjectionEndToEnd(t *testing.T) {
	var gotSystem atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotSystem.Store(gjson.GetBytes(b, "system").Raw)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, func(e *event.Envelope) {
		raw, _ := sjson.SetRaw(e.Payload.Raw, "_txc.llm.context",
			`[{"source":"kv:decisions/adr-001","title":"Why BoltDB","content":"we chose boltdb"}]`)
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"m","system":"be terse","messages":[]}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rr.Code, rr.Body.String())
	}

	sysRaw, _ := gotSystem.Load().(string)
	sys := gjson.Parse(sysRaw)
	if !sys.IsArray() || len(sys.Array()) != 3 {
		t.Fatalf("upstream system = %s, want 3 blocks", sysRaw)
	}
	blocks := sys.Array()
	if blocks[0].Get("text").String() != "be terse" {
		t.Errorf("original system not first: %s", blocks[0].Raw)
	}
	if blocks[1].Get("text").String() != contextGuardText {
		t.Errorf("guard not second: %s", blocks[1].Raw)
	}
	if txt := blocks[2].Get("text").String(); !strings.Contains(txt, "Source: kv:decisions/adr-001") ||
		!strings.Contains(txt, "we chose boltdb") || !strings.Contains(txt, "Length: 15 bytes") {
		t.Errorf("context block wrong:\n%s", txt)
	}

	raw := waitCompletion(t, env)
	rows := gjson.Get(raw, "_txc.llm.context_result")
	if !rows.IsArray() || len(rows.Array()) != 2 {
		t.Fatalf("context_result = %s", rows.Raw)
	}
	if !rows.Array()[0].Get("synthetic").Bool() {
		t.Errorf("first row not the guard: %s", rows.Array()[0].Raw)
	}
	item := rows.Array()[1]
	if item.Get("status").String() != "injected" || item.Get("sha256").String() == "" {
		t.Errorf("item row = %s", item.Raw)
	}
	if strings.Contains(rows.Raw, "we chose boltdb") {
		t.Errorf("context_result leaked content: %s", rows.Raw)
	}
}

// TestContextInjectionDisabledByConfig: either knob at 0 disables
// injection entirely — the upstream sees the untouched request.
func TestContextInjectionDisabledByConfig(t *testing.T) {
	var gotSystem atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotSystem.Store(gjson.GetBytes(b, "system").Raw)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, func(e *event.Envelope) {
		raw, _ := sjson.SetRaw(e.Payload.Raw, "_txc.llm.context", `[{"content":"x"}]`)
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	env.g.pu.Conf.LLMContextMaxTokens = 0
	rr := doReq(t, env.g, `{"model":"m","system":"s"}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got, _ := gotSystem.Load().(string); got != `"s"` {
		t.Errorf("system modified despite disabled injection: %s", got)
	}
	raw := waitCompletion(t, env)
	if gjson.Get(raw, "_txc.llm.context_result").Exists() {
		t.Errorf("context_result present when disabled")
	}
}

// TestUsageCaptureSSEEndToEnd: a streaming upstream's usage lands on the
// completion — with explicit model provenance (requested ≠ effective ≠
// response).
func TestUsageCaptureSSEEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, canonicalStream)
	}))
	defer upstream.Close()

	env := newTestGateway(t, upstream.URL, func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "request.model", "rewritten-model")
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"client-model","stream":true,"messages":[]}`, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "message_stop") {
		t.Errorf("stream not passed through: %q", rr.Body.String())
	}

	raw := waitCompletion(t, env)
	checks := map[string]string{
		"_txc.llm.completion.requested_model":         "client-model",
		"_txc.llm.completion.effective_request_model": "rewritten-model",
		"_txc.llm.completion.response_model":          "claude-haiku-4-5-20251001",
		"_txc.llm.completion.stop_reason":             "end_turn",
	}
	for path, want := range checks {
		if got := gjson.Get(raw, path).String(); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if got := gjson.Get(raw, "_txc.llm.completion.usage.input_tokens").Int(); got != 100 {
		t.Errorf("usage.input_tokens = %d", got)
	}
	if got := gjson.Get(raw, "_txc.llm.completion.usage.output_tokens").Int(); got != 42 {
		t.Errorf("usage.output_tokens = %d (final delta must win)", got)
	}
	if got := gjson.Get(raw, "_txc.llm.completion.usage.cache_read_input_tokens").Int(); got != 50 {
		t.Errorf("usage.cache_read = %d", got)
	}
}

// TestStreamingForcesIdentityEncoding: streaming policy requests ask the
// upstream for an unencoded stream (field finding: clients advertise
// gzip/br/zstd, edges honor it on SSE, and an encoded stream blinds the
// usage capture). Non-stream requests keep the client's encoding.
func TestStreamingForcesIdentityEncoding(t *testing.T) {
	var gotEncoding atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding.Store(r.Header.Get("Accept-Encoding"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, echoStack)

	rr := doReq(t, env.g, `{"model":"m","stream":true}`, map[string]string{"Accept-Encoding": "gzip, br, zstd"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := gotEncoding.Load(); got != "identity" {
		t.Errorf("streaming Accept-Encoding = %v, want identity", got)
	}
	waitCompletion(t, env)

	rr = doReq(t, env.g, `{"model":"m"}`, map[string]string{"Accept-Encoding": "gzip, br, zstd"})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if got := gotEncoding.Load(); got != "gzip, br, zstd" {
		t.Errorf("non-stream Accept-Encoding = %v, want client's verbatim", got)
	}
}

// TestUsageAbsentOnErrorBody: an upstream JSON error body has no usage —
// the completion must omit the fields, not zero them.
func TestUsageAbsentOnErrorBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"nope"}}`))
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, echoStack)
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
	raw := waitCompletion(t, env)
	if gjson.Get(raw, "_txc.llm.completion.usage").Exists() {
		t.Errorf("usage present on an error body: %s", gjson.Get(raw, "_txc.llm.completion.usage").Raw)
	}
	if gjson.Get(raw, "_txc.llm.completion.response_model").Exists() {
		t.Errorf("response_model invented on an error body")
	}
}

// TestRejectWithContextEmitted: a reject verdict wins even when the same
// stack run also emitted context — nothing injects, upstream untouched.
func TestRejectWithContextEmitted(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	env := newTestGateway(t, upstream.URL, func(e *event.Envelope) {
		raw, _ := sjson.Set(e.Payload.Raw, "_txc.llm.reject.message", "no")
		raw, _ = sjson.SetRaw(raw, "_txc.llm.context", `[{"content":"x"}]`)
		e.ResCh <- event.Payload{Raw: raw, Type: event.JSON}
	})
	rr := doReq(t, env.g, `{"model":"m"}`, nil)
	assertAnthropicError(t, rr, http.StatusForbidden, "permission_error")
	if calls.Load() != 0 {
		t.Errorf("upstream called on reject")
	}
}

func TestParseVerdict(t *testing.T) {
	// Defaults on an untouched envelope.
	v := parseVerdict(`{"_txc":{"src":"llm"},"request":{"model":"m"}}`)
	if v.reject || v.admission || v.unavailable {
		t.Errorf("clean envelope produced a denial: %+v", v)
	}
	if v.request != `{"model":"m"}` {
		t.Errorf("request = %q", v.request)
	}

	// Reject defaults: bare object ⇒ 403 permission_error with a message.
	v = parseVerdict(`{"_txc":{"llm":{"reject":{}}}}`)
	if !v.reject || v.rejectStatus != 403 || v.rejectType != "permission_error" || v.rejectMsg == "" {
		t.Errorf("reject defaults: %+v", v)
	}

	// Out-of-range status clamps to 403.
	v = parseVerdict(`{"_txc":{"llm":{"reject":{"status":9999}}}}`)
	if v.rejectStatus != 403 {
		t.Errorf("clamped status = %d", v.rejectStatus)
	}

	// Headers: only string values survive.
	v = parseVerdict(`{"_txc":{"llm":{"headers":{"x-a":"1","x-b":{"nested":true},"x-c":"2"}}},"request":{}}`)
	if len(v.headers) != 2 || v.headers["x-a"] != "1" || v.headers["x-c"] != "2" {
		t.Errorf("headers = %+v", v.headers)
	}
}
