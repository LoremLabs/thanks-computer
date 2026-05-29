package admin

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
)

// TestThrottleMiddleware429 — once the per-IP budget is exhausted,
// further requests get 429 with a parseable Retry-After header.
// Uses a tiny window so the test runs quickly without flake.
func TestThrottleMiddleware429(t *testing.T) {
	c := &Controller{}
	tr := throttle.New(3, 500*time.Millisecond)
	wrapped := c.throttleMiddleware(tr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(remote string) (int, http.Header) {
		req := httptest.NewRequest(http.MethodPost, "/auth/invitations/consume", nil)
		req.RemoteAddr = remote
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, req)
		return rr.Code, rr.Header()
	}

	// First three: 200.
	for i := 0; i < 3; i++ {
		if code, _ := do("10.0.0.1:1234"); code != http.StatusOK {
			t.Fatalf("attempt %d: status=%d, want 200", i+1, code)
		}
	}
	// Fourth: 429 with Retry-After.
	code, hdr := do("10.0.0.1:1234")
	if code != http.StatusTooManyRequests {
		t.Fatalf("4th attempt: status=%d, want 429", code)
	}
	retry := hdr.Get("Retry-After")
	if retry == "" {
		t.Fatalf("Retry-After header missing")
	}
	if n, err := strconv.Atoi(retry); err != nil || n <= 0 {
		t.Errorf("Retry-After should be a positive integer; got %q", retry)
	}

	// Wait for the window to roll over; a fresh attempt passes.
	time.Sleep(550 * time.Millisecond)
	if code, _ := do("10.0.0.1:1234"); code != http.StatusOK {
		t.Errorf("after window: status=%d, want 200", code)
	}
}

// TestThrottleSharedAcrossEndpoints — single shared Throttle means
// an attacker alternating between /auth/dev/enroll and
// /auth/invitations/consume doesn't double their budget. We simulate
// this by wrapping two handlers with the same throttle instance.
func TestThrottleSharedAcrossEndpoints(t *testing.T) {
	c := &Controller{}
	tr := throttle.New(4, time.Second)
	wrap := c.throttleMiddleware(tr)
	handlerA := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	handlerB := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(h http.Handler, remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.RemoteAddr = remote
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}

	// 2 hits on handlerA + 2 hits on handlerB = budget exhausted.
	for i := 0; i < 2; i++ {
		if code := do(handlerA, "10.0.0.2:1"); code != http.StatusOK {
			t.Fatalf("handlerA #%d: %d", i+1, code)
		}
		if code := do(handlerB, "10.0.0.2:1"); code != http.StatusOK {
			t.Fatalf("handlerB #%d: %d", i+1, code)
		}
	}
	// 5th request to EITHER handler from same IP is 429.
	if code := do(handlerA, "10.0.0.2:1"); code != http.StatusTooManyRequests {
		t.Errorf("handlerA past budget: status=%d, want 429", code)
	}
	if code := do(handlerB, "10.0.0.2:1"); code != http.StatusTooManyRequests {
		t.Errorf("handlerB past budget: status=%d, want 429", code)
	}
}

// TestThrottleDifferentIPsIndependent — distinct caller IPs get
// independent buckets through the middleware. Confirms clientIP
// (port-stripped) is the key, not full RemoteAddr.
func TestThrottleDifferentIPsIndependent(t *testing.T) {
	c := &Controller{}
	tr := throttle.New(1, time.Second)
	wrapped := c.throttleMiddleware(tr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	do := func(remote string) int {
		req := httptest.NewRequest(http.MethodPost, "/x", nil)
		req.RemoteAddr = remote
		rr := httptest.NewRecorder()
		wrapped.ServeHTTP(rr, req)
		return rr.Code
	}
	if do("10.0.0.3:9001") != http.StatusOK {
		t.Fatalf("ip3 first attempt should pass")
	}
	if do("10.0.0.3:9999") != http.StatusTooManyRequests {
		t.Errorf("same ip (different port) should be throttled")
	}
	if do("10.0.0.4:1") != http.StatusOK {
		t.Errorf("different ip should be admitted")
	}
}

// TestThrottleResponseShape — the 429 body matches the rest of the
// admin error responses ({error, detail}) and includes a numeric
// retry_after_seconds for programmatic consumers.
func TestThrottleResponseShape(t *testing.T) {
	c := &Controller{}
	tr := throttle.New(1, time.Second)
	wrapped := c.throttleMiddleware(tr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.RemoteAddr = "10.0.0.5:1"
	wrapped.ServeHTTP(httptest.NewRecorder(), req) // exhaust budget

	req = httptest.NewRequest(http.MethodPost, "/x", nil)
	req.RemoteAddr = "10.0.0.5:1"
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "\"throttled\"") {
		t.Errorf("body should carry error=throttled; got %s", body)
	}
	if !strings.Contains(body, "retry_after_seconds") {
		t.Errorf("body should carry retry_after_seconds; got %s", body)
	}
}
