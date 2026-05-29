package processor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// --- Unit tests on the cache itself --------------------------------------

func TestSessionCacheGetMissOnEmpty(t *testing.T) {
	c := newMCPSessionCache(5 * time.Minute)
	if sid, ok := c.get("any|http://x"); ok || sid != "" {
		t.Errorf("empty cache: get returned (%q, %v); want ('', false)", sid, ok)
	}
}

func TestSessionCachePutThenGet(t *testing.T) {
	c := newMCPSessionCache(5 * time.Minute)
	c.put("t1|https://x/mcp", "sid-abc")
	if sid, ok := c.get("t1|https://x/mcp"); !ok || sid != "sid-abc" {
		t.Errorf("get after put = (%q, %v); want ('sid-abc', true)", sid, ok)
	}
}

func TestSessionCacheTTLExpiry(t *testing.T) {
	c := newMCPSessionCache(1 * time.Second)
	// Inject a controllable clock.
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }

	c.put("k", "sid-1")
	if _, ok := c.get("k"); !ok {
		t.Fatal("get immediately after put: miss")
	}
	// Within TTL — still a hit.
	clock = clock.Add(500 * time.Millisecond)
	if _, ok := c.get("k"); !ok {
		t.Fatal("get at 500ms: miss (within 1s TTL)")
	}
	// Past TTL — should be a miss AND evicted as a side-effect.
	clock = clock.Add(2 * time.Second)
	if _, ok := c.get("k"); ok {
		t.Error("get after TTL expiry: hit (should be miss)")
	}
	if got := c.len(); got != 0 {
		t.Errorf("expired entry not auto-evicted; len=%d, want 0", got)
	}
}

func TestSessionCacheLastUsedRefresh(t *testing.T) {
	// Each hit should refresh lastUsed so an active session stays
	// cached even past the original TTL window.
	c := newMCPSessionCache(1 * time.Second)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }

	c.put("k", "sid-active")
	clock = clock.Add(800 * time.Millisecond)
	if _, ok := c.get("k"); !ok {
		t.Fatal("hit at 800ms: miss")
	}
	// Another 800ms — 1.6s from put but only 800ms from last get.
	clock = clock.Add(800 * time.Millisecond)
	if _, ok := c.get("k"); !ok {
		t.Errorf("hit at 1.6s: miss (lastUsed should have refreshed)")
	}
}

func TestSessionCacheEvict(t *testing.T) {
	c := newMCPSessionCache(5 * time.Minute)
	c.put("k", "sid-1")
	c.evict("k")
	if _, ok := c.get("k"); ok {
		t.Error("get after evict: hit (should be miss)")
	}
}

func TestSessionCacheNilSafe(t *testing.T) {
	// All ops on a nil cache must be no-ops, never panic. ExecMCPHTTP
	// relies on this for the test-default path (Unit constructed
	// without EnableMCPSessionCache).
	var c *mcpSessionCache
	if sid, ok := c.get("k"); ok || sid != "" {
		t.Errorf("nil.get returned (%q, %v); want ('', false)", sid, ok)
	}
	c.put("k", "sid")
	c.evict("k")
	if got := c.len(); got != 0 {
		t.Errorf("nil.len = %d, want 0", got)
	}
}

func TestSessionCacheConcurrentAccess(t *testing.T) {
	// sync.RWMutex protects the map; concurrent get/put/evict from
	// many goroutines should not race or panic. (`go test -race`
	// catches lock-ordering or unprotected-write regressions.)
	c := newMCPSessionCache(5 * time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("t%d|http://x", i%5)
			c.put(key, fmt.Sprintf("sid-%d", i))
			c.get(key)
			if i%7 == 0 {
				c.evict(key)
			}
		}(i)
	}
	wg.Wait()
}

func TestSessionCacheEmptySIDPutIgnored(t *testing.T) {
	// Some MCP servers reply without an Mcp-Session-Id header.
	// Caching an empty string would mask the real "no session"
	// state on the next call. put("k", "") must be a no-op.
	c := newMCPSessionCache(5 * time.Minute)
	c.put("k", "")
	if got := c.len(); got != 0 {
		t.Errorf("put(\"\") cached an empty session; len=%d, want 0", got)
	}
}

// --- isSessionInvalidated ------------------------------------------------

func TestIsSessionInvalidated(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"200_not_invalidated", 200, "ok", false},
		{"5xx_not_invalidated", 503, "server overloaded; session abandoned", false}, // 5xx never fires; substring irrelevant
		{"4xx_no_session_word", 400, "bad request", false},
		{"4xx_with_session", 400, `{"error":"unknown_session"}`, true},
		{"4xx_case_insensitive", 400, "Session expired", true},
		{"404_session_id_not_found", 404, "session id not found", true},
		{"empty_body", 400, "", false},
		{"network_error_no_body", 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSessionInvalidated(tc.status, []byte(tc.body)); got != tc.want {
				t.Errorf("status=%d body=%q → %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// --- End-to-end ExecMCPHTTP behavior with the cache enabled --------------

// withCachedTenant wires a fixed tenant onto ctx so the cache key is
// stable across calls (newTestUnit's bare context has no tenant set).
func withCachedTenant(ctx context.Context, tenant string) context.Context {
	return WithTenant(ctx, tenant)
}

// TestSessionCacheHotPath — second call against the same (tenant,
// endpoint) skips initialize + notifications/initialized, going
// straight to tools/call. Two calls → 4 wire methods, not 6.
func TestSessionCacheHotPath(t *testing.T) {
	stub := newMCPStub(t)
	stub.SessionID = "sid-hot"
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}

	pu, _ := newTestUnit(t)
	pu.EnableMCPSessionCache(5 * time.Minute)

	ctx := withCachedTenant(context.Background(), "tenant-a")
	for i := 0; i < 2; i++ {
		payload, err := pu.ExecMCPHTTP(ctx, mcpOp(stub.URL, "ask", `{}`, ""))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got := gjson.Get(payload.Raw, "text").String(); got != "hi" {
			t.Errorf("call %d: text=%q, want hi", i, got)
		}
	}

	want := []string{"initialize", "notifications/initialized", "tools/call", "tools/call"}
	if got := stub.recordedMethods(); !reflect.DeepEqual(got, want) {
		t.Errorf("method sequence = %v, want %v (cache failed to skip init on 2nd call)", got, want)
	}
}

// TestSessionCacheTenantIsolation — two calls with different tenant
// scopes each get their own init lifecycle. Sessions don't bleed
// across tenants.
func TestSessionCacheTenantIsolation(t *testing.T) {
	stub := newMCPStub(t)
	stub.SessionID = "sid-iso"
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"x"}]}}`)
	}

	pu, _ := newTestUnit(t)
	pu.EnableMCPSessionCache(5 * time.Minute)

	op := mcpOp(stub.URL, "ask", `{}`, "")
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		if _, err := pu.ExecMCPHTTP(withCachedTenant(context.Background(), tenant), op); err != nil {
			t.Fatalf("%s: %v", tenant, err)
		}
	}

	want := []string{
		"initialize", "notifications/initialized", "tools/call", // tenant-a (cold)
		"initialize", "notifications/initialized", "tools/call", // tenant-b (cold; separate cache key)
	}
	if got := stub.recordedMethods(); !reflect.DeepEqual(got, want) {
		t.Errorf("method sequence = %v, want %v (tenants must not share cache)", got, want)
	}
}

// TestSessionCacheInvalidationRetry — server invalidates the session
// on a subsequent tools/call (returns 400 with "session" in the body
// on call #2). Chassis should evict, re-init, retry, and the retry's
// tools/call should succeed transparently.
func TestSessionCacheInvalidationRetry(t *testing.T) {
	var callCount int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := gjson.GetBytes(body, "method").String()
		switch method {
		case "initialize":
			w.Header().Set(mcpSessionHeader, fmt.Sprintf("sid-gen-%d", atomic.LoadInt32(&callCount)))
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q}}`, mcpProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			n := atomic.AddInt32(&callCount, 1)
			// First tools/call after first init succeeds. After the
			// next init we ALSO succeed (so the retry returns clean).
			// In between, on the SECOND tools/call (= ExecMCPHTTP's
			// second invocation), return 400 saying session is gone.
			if n == 2 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"unknown_session"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi-%d"}]}}`, n)
		default:
			http.Error(w, "unknown", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	pu.EnableMCPSessionCache(5 * time.Minute)
	ctx := withCachedTenant(context.Background(), "tenant-a")
	op := mcpOp(srv.URL, "ask", `{}`, "")

	// Call 1: cold cache → init + call. Should succeed (counter=1).
	payload1, err := pu.ExecMCPHTTP(ctx, op)
	if err != nil {
		t.Fatalf("call 1: %v", err)
	}
	if got := gjson.Get(payload1.Raw, "text").String(); got != "hi-1" {
		t.Fatalf("call 1: text=%q, want hi-1", got)
	}

	// Call 2: hot cache → call only, returns 400 unknown_session.
	// Chassis must evict + re-init + retry call. Retry succeeds
	// (counter=3 — n=2 was the failure, n=3 is the retry).
	payload2, err := pu.ExecMCPHTTP(ctx, op)
	if err != nil {
		t.Fatalf("call 2 (after retry): %v", err)
	}
	if got := gjson.Get(payload2.Raw, "text").String(); got != "hi-3" {
		t.Errorf("retry text=%q, want hi-3 (retry should reach the server's third tools/call)", got)
	}
}

// TestSessionCacheRetryCapsAtOne — if the server keeps rejecting the
// session on every retry, the chassis must surface the error rather
// than retrying forever.
func TestSessionCacheRetryCapsAtOne(t *testing.T) {
	var initCount int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := gjson.GetBytes(body, "method").String()
		switch method {
		case "initialize":
			atomic.AddInt32(&initCount, 1)
			w.Header().Set(mcpSessionHeader, "sid-x")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q}}`, mcpProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			// Always 400 unknown_session.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unknown_session"}`))
		default:
			http.Error(w, "unknown", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	pu.EnableMCPSessionCache(5 * time.Minute)
	ctx := withCachedTenant(context.Background(), "tenant-a")

	_, err := pu.ExecMCPHTTP(ctx, mcpOp(srv.URL, "ask", `{}`, ""))
	if err == nil {
		t.Fatal("ExecMCPHTTP returned nil err despite repeated unknown_session")
	}

	// Exactly two initializes: the cold-cache one and the one-shot
	// retry after eviction. Not three, not infinite.
	if got := atomic.LoadInt32(&initCount); got != 2 {
		t.Errorf("init fired %d times, want 2 (initial + one-shot retry)", got)
	}
}

// TestSessionCacheNilUnitCacheStillWorks — Unit constructed without
// EnableMCPSessionCache (the bare newTestUnit default) keeps the
// pre-cache behavior: every call runs the full lifecycle. Backwards-
// compatibility check.
func TestSessionCacheNilUnitCacheStillWorks(t *testing.T) {
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	pu, _ := newTestUnit(t)
	// Note: NOT calling EnableMCPSessionCache. pu.MCPSessions is nil.

	for i := 0; i < 2; i++ {
		if _, err := pu.ExecMCPHTTP(context.Background(), mcpOp(stub.URL, "ask", `{}`, "")); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	// Without a cache, every call runs the full lifecycle.
	want := []string{
		"initialize", "notifications/initialized", "tools/call",
		"initialize", "notifications/initialized", "tools/call",
	}
	if got := stub.recordedMethods(); !reflect.DeepEqual(got, want) {
		t.Errorf("nil-cache method sequence = %v, want %v", got, want)
	}
}

// TestSessionCacheRefreshesOnNewSessionHeader — when the server
// rotates the session id on a tools/call response (sets a NEW
// Mcp-Session-Id header), the cache picks up the new id. Otherwise
// the next call would echo a stale id back.
func TestSessionCacheRefreshesOnNewSessionHeader(t *testing.T) {
	var callCount int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := gjson.GetBytes(body, "method").String()
		switch method {
		case "initialize":
			w.Header().Set(mcpSessionHeader, "sid-original")
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":%q}}`, mcpProtocolVersion)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			n := atomic.AddInt32(&callCount, 1)
			// On the first call, rotate the session id.
			if n == 1 {
				w.Header().Set(mcpSessionHeader, "sid-rotated")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
		default:
			http.Error(w, "unknown", http.StatusBadRequest)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	pu, _ := newTestUnit(t)
	pu.EnableMCPSessionCache(5 * time.Minute)
	ctx := withCachedTenant(context.Background(), "tenant-a")
	op := mcpOp(srv.URL, "ask", `{}`, "")

	if _, err := pu.ExecMCPHTTP(ctx, op); err != nil {
		t.Fatalf("call 1: %v", err)
	}

	// Cache should now hold the ROTATED id, not the initial one.
	sid, ok := pu.MCPSessions.get(sessionCacheKey("tenant-a", srv.URL))
	if !ok {
		t.Fatal("cache miss after first call")
	}
	if sid != "sid-rotated" {
		t.Errorf("cached sid = %q, want sid-rotated (server rotated mid-call)", sid)
	}
}
