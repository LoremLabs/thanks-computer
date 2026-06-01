package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// stubServer redirects the package-level `endpoint` const to an
// httptest.Server. The backend reads `endpoint` at request-build time;
// tests substitute via a wrapper httptest.Server URL and a per-test
// override using a small indirection.
//
// We can't override the package-level `endpoint` const in tests, so the
// tests run with a custom http.Client whose Transport rewrites every
// request URL to the stub server. This keeps the production code
// unchanged.

type rewriteTransport struct {
	target string
	base   http.RoundTripper
	hits   int32
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	atomic.AddInt32(&t.hits, 1)
	// Rewrite to point at the stub server, preserving body.
	stubURL := t.target + req.URL.Path
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, stubURL, req.Body)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	return t.base.RoundTrip(newReq)
}

func newStubClient(t *testing.T, handler http.HandlerFunc) (*http.Client, *httptest.Server, *rewriteTransport) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	rt := &rewriteTransport{target: srv.URL, base: http.DefaultTransport}
	return &http.Client{Transport: rt, Timeout: 5 * time.Second}, srv, rt
}

func newBackend(t *testing.T, client *http.Client) chat.Backend {
	t.Helper()
	return &backend{httpClient: client}
}

func newBag(t *testing.T, key string) *secrets.SecretBag {
	t.Helper()
	bag := &secrets.SecretBag{}
	bag.Set("OPENROUTER_KEY", []byte(key))
	return bag
}

// --- happy path ---

func TestOpenRouterHappyPath(t *testing.T) {
	var capturedAuth, capturedBody string
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		capturedBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "openai/gpt-4o-mini",
			"choices": [{"message": {"role":"assistant", "content":"hello back"}}],
			"usage": {"prompt_tokens": 12, "completion_tokens": 5}
		}`))
	})

	b := newBackend(t, client)
	bag := newBag(t, "sk-or-v1-test-xyz")

	resp, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "hello"}},
		Model:    "openai/gpt-4o-mini",
	}, bag)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Text != "hello back" {
		t.Errorf("Text = %q, want %q", resp.Text, "hello back")
	}
	if resp.TokensIn != 12 || resp.TokensOut != 5 {
		t.Errorf("tokens = (%d, %d), want (12, 5)", resp.TokensIn, resp.TokensOut)
	}
	if resp.Retries != 0 {
		t.Errorf("Retries = %d, want 0", resp.Retries)
	}
	if resp.LatencyMS < 0 {
		t.Errorf("LatencyMS should be non-negative, got %d", resp.LatencyMS)
	}
	if !strings.HasPrefix(capturedAuth, "Bearer sk-or-v1-test-xyz") {
		t.Errorf("Authorization header missing or malformed: %q", capturedAuth)
	}
	if !strings.Contains(capturedBody, `"hello"`) {
		t.Errorf("outbound body missing user content: %s", capturedBody)
	}
}

// --- 401 sanitization (leak guard 3) ---

func TestOpenRouter401SanitizesAndOmitsBody(t *testing.T) {
	// Provider quotes a key prefix in the error body — this MUST NOT
	// reach the chassis-facing error.
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"sk-or-v1-test-xyz is invalid"}}`))
	})

	b := newBackend(t, client)
	bag := newBag(t, "sk-or-v1-test-xyz")

	resp, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "hi"}},
	}, bag)
	if !errors.Is(err, chat.ErrAuthFailed) {
		t.Fatalf("expected ErrAuthFailed; got %T: %v", err, err)
	}
	// Error message must not contain the key.
	if strings.Contains(err.Error(), "sk-or-v1") {
		t.Errorf("error must not quote key prefix: %v", err)
	}
	// Response observability metadata is still present.
	if resp.Provider != "openrouter" {
		t.Errorf("Provider = %q, want openrouter", resp.Provider)
	}
}

// --- retry on 5xx ---

func TestOpenRouterRetriesOn5xx(t *testing.T) {
	var calls int32
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`upstream timeout`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}], "usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})

	b := newBackend(t, client)
	bag := newBag(t, "key")

	resp, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "x"}},
	}, bag)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if resp.Retries != 1 {
		t.Errorf("Retries = %d, want 1", resp.Retries)
	}
	if calls != 2 {
		t.Errorf("server hit = %d, want 2", calls)
	}
}

// --- no retry on 4xx ---

func TestOpenRouterNoRetryOn4xx(t *testing.T) {
	var calls int32
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	})

	b := newBackend(t, client)
	bag := newBag(t, "key")

	_, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "x"}},
	}, bag)
	var phe *chat.ProviderHTTPError
	if !errors.As(err, &phe) {
		t.Fatalf("expected ProviderHTTPError, got %T: %v", err, err)
	}
	if phe.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", phe.StatusCode)
	}
	if calls != 1 {
		t.Errorf("server hit = %d, want 1 (no retry on 4xx)", calls)
	}
}

// --- missing key (defensive) ---

func TestOpenRouterMissingKeyReturnsMissingSecretError(t *testing.T) {
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("backend must not hit the network when key is missing")
	})
	b := newBackend(t, client)
	bag := &secrets.SecretBag{} // empty

	_, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "x"}},
	}, bag)
	var mse *chat.MissingSecretError
	if !errors.As(err, &mse) {
		t.Fatalf("expected MissingSecretError, got %T: %v", err, err)
	}
	if mse.Secret != "OPENROUTER_KEY" {
		t.Errorf("Secret = %q, want OPENROUTER_KEY", mse.Secret)
	}
}

// --- key NEVER leaks via marshaling (leak guard 1) ---

func TestSecretBagInChatRequestPanicsOnMarshal(t *testing.T) {
	// Smoke test: ensure the bag's marshal panic actually fires when an
	// implementer accidentally embeds it.
	type misuse struct {
		Req chat.Request
		Bag *secrets.SecretBag
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic from SecretBag MarshalJSON; got none")
		}
	}()
	bag := &secrets.SecretBag{}
	bag.Set("OPENROUTER_KEY", []byte("secret-value"))
	x := misuse{Req: chat.Request{Messages: []chat.Message{{Role: "user", Content: "x"}}}, Bag: bag}
	_, _ = json.Marshal(x) // must panic
}

// --- empty 200 body (free-tier rate limit shape) ---

func TestOpenRouterEmptyBodyIsParseError(t *testing.T) {
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// no body written — mimics the ":free quota exhausted" shape
		// some providers return instead of a clean 429
	})
	b := newBackend(t, client)
	bag := newBag(t, "k")

	resp, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "x"}},
	}, bag)
	var pe *chat.ProviderParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderParseError, got %T: %v", err, err)
	}
	if pe.Code() != chat.CodeProviderParse {
		t.Errorf("Code = %q, want %q", pe.Code(), chat.CodeProviderParse)
	}
	if pe.BodyLen != 0 {
		t.Errorf("BodyLen = %d, want 0 (empty body)", pe.BodyLen)
	}
	// Partial Response metadata still carries provider name + latency.
	if resp.Provider != "openrouter" {
		t.Errorf("Provider = %q, want openrouter", resp.Provider)
	}
}

// --- malformed 200 body ---

func TestOpenRouterMalformedBodyIsParseError(t *testing.T) {
	client, _, _ := newStubClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not json`))
	})
	b := newBackend(t, client)
	bag := newBag(t, "k")

	_, err := b.Run(context.Background(), chat.Request{
		Messages: []chat.Message{{Role: "user", Content: "x"}},
	}, bag)
	var pe *chat.ProviderParseError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderParseError, got %T: %v", err, err)
	}
	if pe.BodyLen == 0 {
		t.Errorf("BodyLen = 0; want positive for malformed (non-empty) body")
	}
	// The Reason carries the json error string — useful for trace, but
	// must not have a code prefix the response envelope already adds.
	if pe.Reason == "" {
		t.Errorf("Reason should be populated with the json error")
	}
}

// --- registration roundtrip ---

func TestOpenRouterRegistersUnderName(t *testing.T) {
	// init() ran on package load. The constructor must be openable
	// against a non-nil HTTPClient.
	b, err := chat.Open("openrouter", chat.Config{HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("Open openrouter: %v", err)
	}
	if b.Name() != "openrouter" {
		t.Errorf("Name = %q, want openrouter", b.Name())
	}
	if got := b.RequiredSecrets(); len(got) != 1 || got[0] != "OPENROUTER_KEY" {
		t.Errorf("RequiredSecrets = %v, want [OPENROUTER_KEY]", got)
	}
}
