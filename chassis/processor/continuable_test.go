package processor

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
)

// TestIsContinuableOpClassification — gate function unit. Matches
// http(s)://, mcp+http(s)://, with `mode = "continuable"`; rejects others.
func TestIsContinuableOpClassification(t *testing.T) {
	cases := []struct {
		exec string
		meta string
		want bool
	}{
		{"https://example.com/api", `{"mode":"continuable"}`, true},
		{"http://example.com/api", `{"mode":"continuable"}`, true},
		{"mcp+http://example.com/x#tool", `{"mode":"continuable"}`, true},
		{"mcp+https://example.com/x#tool", `{"mode":"continuable"}`, true},
		{"https://example.com/api", `{"mode":"async"}`, false}, // wrong mode
		{"https://example.com/api", `{}`, false},               // no mode
		{"txco://noop", `{"mode":"continuable"}`, false},       // unsupported scheme
	}
	for _, tc := range cases {
		op := operation.Operation{
			Resonator: &resonator.Resonator{Exec: tc.exec},
			Meta:      tc.meta,
		}
		if got := isContinuableOp(op); got != tc.want {
			t.Errorf("isContinuableOp(exec=%q meta=%q) = %v, want %v", tc.exec, tc.meta, got, tc.want)
		}
	}
}

// delayedStub returns a stub HTTP server that sleeps for `delay` then
// responds with `body` and status 200. Tests use it to dial the
// upstream-response timing across the continue_after deadline.
func delayedStub(t *testing.T, delay time.Duration, body string) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

// TestContinuableFastResponseStaysSync — upstream beats the
// continue_after timer. Client receives the actual response body inline,
// NO 202, NO continuation token, NO durable run records.
func TestContinuableFastResponseStaysSync(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	stub, hits := delayedStub(t, 50*time.Millisecond, `{"upstream":"ok"}`)

	rule := fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "1s", timeout = "5s"`, stub.URL)
	seedOp(t, pu, "acme", 100, "research", rule)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	select {
	case p := <-resCh:
		// Status should be the default 200 (vanilla sync return), not 202.
		if got := gjson.Get(p.Raw, "_txc.web.res.status").Int(); got != 0 && got != 200 {
			t.Errorf("status = %d, want 0 or 200 (sync inline)", got)
		}
		// The upstream's body should have merged into the envelope.
		if got := gjson.Get(p.Raw, "upstream").String(); got != "ok" {
			t.Errorf("upstream field missing from envelope; got body=%s", p.Raw)
		}
		// No continuation token in the body.
		body := gjson.Get(p.Raw, "_txc.web.res.body").String()
		if body != "" {
			if decoded, derr := base64.StdEncoding.DecodeString(body); derr == nil {
				if cn := gjson.GetBytes(decoded, "continuation").String(); cn != "" {
					t.Errorf("unexpected continuation token in sync return: %q", cn)
				}
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no response within 3s (continuable sync path stuck)")
	}

	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}
}

// TestContinuablePromotesAfterDeadline — upstream sleeps PAST
// continue_after. Client receives 202 + continuation token at ~deadline;
// the detached goroutine then records the terminal when the upstream
// eventually answers, and Resume drives the run to StateCompleted.
func TestContinuablePromotesAfterDeadline(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	// Upstream answers AFTER continue_after fires.
	stub, hits := delayedStub(t, 500*time.Millisecond, `{"upstream":"late"}`)

	rule := fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "10s"`, stub.URL)
	seedOp(t, pu, "acme", 100, "research", rule)
	// Downstream scope so we can verify the rest of the stack runs post-resume.
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	rcid, loc := waitFor202(t, resCh)
	if rcid == "" || loc == "" {
		t.Fatalf("bad 202 promotion: rcid=%q loc=%q", rcid, loc)
	}

	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("post-resume state = %q, want completed", st)
	}

	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("upstream hits = %d, want 1", got)
	}

	// Verify the upstream body AND the downstream EMIT both ended up in
	// the resumed result.json — closing the loop on Q1 (stack continues
	// past the suspended scope).
	res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok {
		t.Fatal("no result.json after resume")
	}
	if got := gjson.GetBytes(res, "upstream").String(); got != "late" {
		t.Errorf("result missing upstream body; got %s", res)
	}
	if !gjson.GetBytes(res, "resumed").Bool() {
		t.Errorf("result missing downstream EMIT (acme/200 .resumed=true); got %s", res)
	}
}

// TestContinuableTimeoutFailsPostPromotion — promote, then upstream
// sleeps past `timeout`. Expect the detached goroutine to record a
// failed terminal and the run to reach StateFailed.
func TestContinuableTimeoutFailsPostPromotion(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	// Upstream sleeps WAY longer than timeout.
	stub, _ := delayedStub(t, 5*time.Second, `{"upstream":"never"}`)
	rule := fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "100ms", timeout = "300ms"`, stub.URL)
	seedOp(t, pu, "acme", 100, "research", rule)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)

	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateFailed {
		t.Errorf("expected failed terminal after timeout; got %q", st)
	}
}

// TestContinuableSoloScopeOnly — a mixed scope (continuable + sync) is
// rejected with an error payload. v1 constraint.
func TestContinuableSoloScopeOnly(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	stub, _ := delayedStub(t, 0, `{"x":1}`)
	seedOp(t, pu, "acme", 100, "research",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable"`, stub.URL))
	// Second op in the same scope makes it mixed.
	seedOp(t, pu, "acme", 100, "tag", `EMIT .tagged = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	select {
	case p := <-resCh:
		body := gjson.Get(p.Raw, "_txc.web.res.body").String()
		// failContinuableInline returns ErrorStr payload, not a 202.
		if p.Type != event.ErrorStr && body == "" {
			t.Errorf("expected an error payload for mixed scope; got %+v", p)
		}
		if !gjsonContains(p.Raw, "solo scope") {
			t.Errorf("error should mention solo-scope requirement; got %s", p.Raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no response within 2s for mixed-scope rejection")
	}
}

// TestContinuableInvalidConfig — continue_after >= timeout is a bad
// config; surface as an error payload rather than silently never
// promoting.
func TestContinuableInvalidConfig(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	stub, _ := delayedStub(t, 0, `{"x":1}`)
	seedOp(t, pu, "acme", 100, "research",
		fmt.Sprintf(`EXEC "%s" WITH mode = "continuable", continue_after = "5s", timeout = "3s"`, stub.URL))

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()

	select {
	case p := <-resCh:
		if !gjsonContains(p.Raw, "must be <") {
			t.Errorf("error should explain continue_after must be < timeout; got %s", p.Raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no error payload within 2s")
	}
}

// gjsonContains is a small substring check on raw payload — handles
// the structured failPayload shape (error.message) AND raw error
// strings, so tests don't need to know which path the error took.
func gjsonContains(raw, needle string) bool {
	return contains(raw, needle) ||
		contains(gjson.Get(raw, "error.message").String(), needle)
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub ||
		len(s) > len(sub) && (indexOf(s, sub) >= 0)))
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
