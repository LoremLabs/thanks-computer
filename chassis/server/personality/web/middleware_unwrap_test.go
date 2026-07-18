package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// flushCountingRecorder wraps a ResponseRecorder and counts Flush calls,
// standing in for the real server ResponseWriter under the middleware.
type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (f *flushCountingRecorder) Flush() { f.flushes++ }

// TestContextResponseWriterUnwrapFlush: http.NewResponseController must
// reach the underlying Flusher THROUGH the contextResponseWriter
// middleware wrapper. Before Unwrap existed, rc.Flush() returned
// ErrNotSupported and every streamed response silently buffered.
func TestContextResponseWriterUnwrapFlush(t *testing.T) {
	inner := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	crw := &contextResponseWriter{
		ResponseWriter: inner,
		start:          time.Now(),
		ctx:            context.Background(),
	}
	rc := http.NewResponseController(crw)
	if _, err := crw.Write([]byte("chunk")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through contextResponseWriter = %v, want nil", err)
	}
	if inner.flushes != 1 {
		t.Fatalf("inner flushes = %d, want 1", inner.flushes)
	}
	// The wrapper's own accounting must keep working through the same path.
	if inner.Code != http.StatusOK {
		t.Fatalf("status = %d, want implicit 200", inner.Code)
	}
}
