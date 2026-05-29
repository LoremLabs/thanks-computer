package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

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
	req := httptest.NewRequest(http.MethodGet, "/traces/stream?wait=50", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/traces/stream?wait=500", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/traces/stream?cursor=3&wait=500", nil)
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
