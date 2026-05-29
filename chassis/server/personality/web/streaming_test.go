package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// TestWriteStreamWritesHeadThenChunks verifies the outlet's streaming
// path: the StreamHead establishes status + headers, each StreamChunk is
// written and flushed, and StreamEnd returns. The concatenated chunk
// bytes are the response body.
func TestWriteStreamWritesHeadThenChunks(t *testing.T) {
	web := NewController(context.Background(),
		&processor.Unit{Logger: zap.NewNop()}, trace.NoopSink{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resCh := make(chan event.Payload)
	head := event.Payload{
		Type: event.StreamHead,
		Raw:  `{"_txc":{"web":{"res":{"status":201,"headers":{"content-type":["text/plain"],"x-demo":["1"]}}}}}`,
	}

	done := make(chan struct{})
	go func() {
		web.writeStream(ctx, cancel, rec, req, head, resCh)
		close(done)
	}()

	resCh <- event.Payload{Type: event.StreamChunk, Raw: "hello "}
	resCh <- event.Payload{Type: event.StreamChunk, Raw: "world"}
	resCh <- event.Payload{Type: event.StreamEnd}
	<-done

	if rec.Code != 201 {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain" {
		t.Errorf("content-type = %q, want text/plain", got)
	}
	if got := rec.Header().Get("X-Demo"); got != "1" {
		t.Errorf("x-demo = %q, want 1", got)
	}
	if got := rec.Body.String(); got != "hello world" {
		t.Errorf("body = %q, want %q", got, "hello world")
	}
	if !rec.Flushed {
		t.Errorf("expected the response to have been flushed incrementally")
	}
}

// TestWriteStreamHeadRequestOmitsBody confirms a HEAD request gets the
// head (status + headers) but no body bytes, while still draining chunks
// so the processor's blocking sends unblock.
func TestWriteStreamHeadRequestOmitsBody(t *testing.T) {
	web := NewController(context.Background(),
		&processor.Unit{Logger: zap.NewNop()}, trace.NoopSink{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resCh := make(chan event.Payload)
	head := event.Payload{
		Type: event.StreamHead,
		Raw:  `{"_txc":{"web":{"res":{"status":200,"headers":{"content-type":["text/plain"]}}}}}`,
	}

	done := make(chan struct{})
	go func() {
		web.writeStream(ctx, cancel, rec, req, head, resCh)
		close(done)
	}()

	resCh <- event.Payload{Type: event.StreamChunk, Raw: "should-not-appear"}
	resCh <- event.Payload{Type: event.StreamEnd}
	<-done

	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "" {
		t.Errorf("HEAD body = %q, want empty", got)
	}
}
