package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/auth"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// asSuperAdmin attaches a super-admin auth.Context so a request exercises the
// flat (chassis-wide) trace path past the traceTenantScope gate.
func asSuperAdmin(r *http.Request) *http.Request {
	return r.WithContext(auth.WithContext(r.Context(), &auth.Context{Source: "signed", SuperAdmin: true}))
}

// asTenant attaches a tenant-owner auth.Context (as resolveTenantMiddleware
// would for /v1/tenants/{slug}/…), confining the request to `slug`.
func asTenant(r *http.Request, slug string) *http.Request {
	return r.WithContext(auth.WithContext(r.Context(), &auth.Context{
		Source: "signed", TenantSlug: slug, Capabilities: []string{"opstack:*:*"},
	}))
}

// memArmable is an in-memory trace.Armable for unit tests. Subscribe
// pre-loads any events whose cursor is strictly greater than
// sinceCursor (lexicographic) into the events channel, then yields
// any subsequent events pushed via Push.
type memArmable struct {
	preloaded []trace.ClosedTrace
}

func (m *memArmable) Subscribe(ctx context.Context, sinceCursor string, buf int) (trace.Subscription, error) {
	if buf < 1 {
		buf = 16
	}
	ch := make(chan trace.ClosedTrace, buf)
	for _, ev := range m.preloaded {
		if sinceCursor == "" || ev.Cursor > sinceCursor {
			ch <- ev
		}
	}
	return &memSub{ch: ch, ctx: ctx}, nil
}

func (m *memArmable) Close(ctx context.Context) error { return nil }

type memSub struct {
	ch  chan trace.ClosedTrace
	ctx context.Context
}

func (s *memSub) Events() <-chan trace.ClosedTrace { return s.ch }
func (s *memSub) Close()                           {}

func newStreamControllerForTest(t *testing.T, arm trace.Armable, longPollMS int) *Controller {
	t.Helper()
	c := newTestController(t, config.Config{
		Personalities:         "admin",
		TraceStreamLongPollMS: longPollMS,
		TraceStreamRingSize:   16,
	})
	c.traceArmable = arm
	return c
}

func TestTraceStream_NotFoundWhenNoArmable(t *testing.T) {
	c := newTestController(t, config.Config{Personalities: "admin"})
	// traceArmable left nil.
	req := httptest.NewRequest(http.MethodGet, "/traces/stream", nil)
	w := httptest.NewRecorder()
	c.handleTraceStream(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestTraceStream_TimeoutReturns202(t *testing.T) {
	c := newStreamControllerForTest(t, &memArmable{}, 50)
	req := asSuperAdmin(httptest.NewRequest(http.MethodGet, "/traces/stream?wait=50", nil))
	w := httptest.NewRecorder()
	start := time.Now()
	c.handleTraceStream(w, req)
	elapsed := time.Since(start)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("202 response should have empty body, got %d bytes", w.Body.Len())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on 202")
	}
	// Sanity: should have actually waited (at least most of) the
	// budget — not returned instantly. Allow generous slack.
	if elapsed < 30*time.Millisecond {
		t.Errorf("handler returned in %v, expected to wait ~50ms", elapsed)
	}
}

func TestTraceStream_DeliversEvents(t *testing.T) {
	in := []trace.ClosedTrace{
		{RequestDetail: trace.RequestDetail{RID: "rid-a", Status: "ok"}, Cursor: "1"},
		{RequestDetail: trace.RequestDetail{RID: "rid-b", Status: "ok"}, Cursor: "2"},
		{RequestDetail: trace.RequestDetail{RID: "rid-c", Status: "error"}, Cursor: "3"},
	}
	c := newStreamControllerForTest(t, &memArmable{preloaded: in}, 1000)
	req := asSuperAdmin(httptest.NewRequest(http.MethodGet, "/traces/stream?wait=500", nil))
	w := httptest.NewRecorder()
	c.handleTraceStream(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp traceStreamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(resp.Events))
	}
	want := []string{"rid-a", "rid-b", "rid-c"}
	for i, ev := range resp.Events {
		if ev.RID != want[i] {
			t.Errorf("events[%d].rid = %q, want %q", i, ev.RID, want[i])
		}
	}
	if resp.NextCursor != "3" {
		t.Errorf("NextCursor = %q, want 3", resp.NextCursor)
	}
	if resp.Events[2].Status != "error" {
		t.Errorf("status passthrough broken: got %q", resp.Events[2].Status)
	}
}

func TestTraceStream_CursorFilters(t *testing.T) {
	in := []trace.ClosedTrace{
		{RequestDetail: trace.RequestDetail{RID: "rid-a"}, Cursor: "1"},
		{RequestDetail: trace.RequestDetail{RID: "rid-b"}, Cursor: "2"},
		{RequestDetail: trace.RequestDetail{RID: "rid-c"}, Cursor: "3"},
		{RequestDetail: trace.RequestDetail{RID: "rid-d"}, Cursor: "4"},
		{RequestDetail: trace.RequestDetail{RID: "rid-e"}, Cursor: "5"},
	}
	c := newStreamControllerForTest(t, &memArmable{preloaded: in}, 1000)
	// Reconnect with cursor=3 ⇒ should receive only 4 and 5.
	req := asSuperAdmin(httptest.NewRequest(http.MethodGet, "/traces/stream?cursor=3&wait=500", nil))
	w := httptest.NewRecorder()
	c.handleTraceStream(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp traceStreamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("got %d events, want 2 (cursors 4,5)", len(resp.Events))
	}
	if resp.Events[0].RID != "rid-d" || resp.Events[1].RID != "rid-e" {
		t.Errorf("got rids %q,%q; want rid-d,rid-e",
			resp.Events[0].RID, resp.Events[1].RID)
	}
	if resp.NextCursor != "5" {
		t.Errorf("NextCursor = %q, want 5", resp.NextCursor)
	}
}

// TestTraceStream_TenantScopedFilters: a tenant-scoped subscriber sees only
// its own tenant's events, but the cursor still advances past foreign/_sys
// events so a quiet tenant doesn't re-poll them forever.
func TestTraceStream_TenantScopedFilters(t *testing.T) {
	in := []trace.ClosedTrace{
		{RequestDetail: trace.RequestDetail{RID: "a", Tenant: "prod-mankins", Status: "ok"}, Cursor: "1"},
		{RequestDetail: trace.RequestDetail{RID: "b", Tenant: "acme", Status: "ok"}, Cursor: "2"},
		{RequestDetail: trace.RequestDetail{RID: "c", Tenant: "prod-mankins", Status: "ok"}, Cursor: "3"},
		{RequestDetail: trace.RequestDetail{RID: "d", Tenant: "_sys", Status: "ok"}, Cursor: "4"},
	}
	c := newStreamControllerForTest(t, &memArmable{preloaded: in}, 1000)
	req := asTenant(httptest.NewRequest(http.MethodGet, "/traces/stream?wait=500", nil), "prod-mankins")
	w := httptest.NewRecorder()
	c.handleTraceStream(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp traceStreamResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 2 {
		t.Fatalf("got %d events, want 2 (only prod-mankins)", len(resp.Events))
	}
	for _, ev := range resp.Events {
		if ev.Tenant != "prod-mankins" {
			t.Errorf("leaked %s (tenant %s) to prod-mankins", ev.RID, ev.Tenant)
		}
	}
	if resp.NextCursor != "4" {
		t.Errorf("NextCursor = %q, want 4 (advanced past foreign/_sys events)", resp.NextCursor)
	}
}

// TestTraceStream_FlatRequiresSuperAdmin: a tenant-owner (opstack:*:*, not
// super-admin) hitting the flat /traces/stream is denied — the leak fix.
func TestTraceStream_FlatRequiresSuperAdmin(t *testing.T) {
	c := newStreamControllerForTest(t, &memArmable{}, 50)
	req := httptest.NewRequest(http.MethodGet, "/traces/stream?wait=50", nil)
	req = req.WithContext(auth.WithContext(req.Context(), &auth.Context{
		Source: "signed", Capabilities: []string{"opstack:*:*"}, // no TenantSlug, not super-admin
	}))
	w := httptest.NewRecorder()
	c.handleTraceStream(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (flat stream is super-admin only)", w.Code)
	}
}
