package web

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hijackRecorder stands in for the server ResponseWriter under the
// middleware: it implements http.Hijacker and records the call.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, c2 := net.Pipe()
	_ = c2
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}

// TestContextResponseWriterHijack: a WebSocket upgrade must reach the
// underlying Hijacker THROUGH the contextResponseWriter wrapper, both by a
// direct http.Hijacker assertion (what the WebSocket library does first)
// and through http.NewResponseController (the Unwrap path).
func TestContextResponseWriterHijack(t *testing.T) {
	inner := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	crw := &contextResponseWriter{
		ResponseWriter: inner,
		start:          time.Now(),
		ctx:            context.Background(),
	}

	hj, ok := interface{}(crw).(http.Hijacker)
	if !ok {
		t.Fatal("contextResponseWriter does not satisfy http.Hijacker")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		t.Fatalf("direct Hijack: %v", err)
	}
	if conn == nil || rw == nil || !inner.hijacked {
		t.Fatal("direct Hijack did not reach the underlying writer")
	}
	_ = conn.Close()

	inner.hijacked = false
	conn, rw, err = http.NewResponseController(crw).Hijack()
	if err != nil {
		t.Fatalf("Hijack via ResponseController: %v", err)
	}
	if conn == nil || rw == nil || !inner.hijacked {
		t.Fatal("ResponseController Hijack did not reach the underlying writer")
	}
	_ = conn.Close()
}
