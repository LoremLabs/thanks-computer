package web

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// TestReadCappedRequestBody pins the H4 contract for the web inlet's
// request-body cap: a body at/under the limit reads through, a body over
// the limit is rejected with 413 (not buffered), and a cap of 0 disables
// the limit entirely.
func TestReadCappedRequestBody(t *testing.T) {
	log := zap.NewNop()

	t.Run("under cap reads through", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", strings.NewReader("hello"))
		body, ok := readCappedRequestBody(w, r, 1024, log)
		if !ok {
			t.Fatalf("ok=false for a body under the cap (status %d)", w.Code)
		}
		if string(body) != "hello" {
			t.Fatalf("body = %q, want %q", body, "hello")
		}
	})

	t.Run("over cap → 413", func(t *testing.T) {
		w := httptest.NewRecorder()
		big := strings.Repeat("x", 2048)
		r := httptest.NewRequest("POST", "/", strings.NewReader(big))
		body, ok := readCappedRequestBody(w, r, 1024, log)
		if ok {
			t.Fatalf("ok=true for a body over the cap")
		}
		if w.Code != 413 {
			t.Fatalf("status = %d, want 413", w.Code)
		}
		if body != nil {
			t.Fatalf("body must be nil on rejection, got %d bytes", len(body))
		}
	})

	t.Run("at cap boundary reads through", func(t *testing.T) {
		w := httptest.NewRecorder()
		exact := strings.Repeat("y", 1024)
		r := httptest.NewRequest("POST", "/", strings.NewReader(exact))
		body, ok := readCappedRequestBody(w, r, 1024, log)
		if !ok {
			t.Fatalf("ok=false for a body exactly at the cap (status %d)", w.Code)
		}
		if len(body) != 1024 {
			t.Fatalf("body len = %d, want 1024", len(body))
		}
	})

	t.Run("cap 0 disables the limit", func(t *testing.T) {
		w := httptest.NewRecorder()
		big := strings.Repeat("z", 1<<20) // 1 MiB, no cap
		r := httptest.NewRequest("POST", "/", strings.NewReader(big))
		body, ok := readCappedRequestBody(w, r, 0, log)
		if !ok {
			t.Fatalf("ok=false with cap disabled (status %d)", w.Code)
		}
		if len(body) != 1<<20 {
			t.Fatalf("body len = %d, want %d", len(body), 1<<20)
		}
	})

	// Guard against a regression where a read error other than overflow
	// is mis-mapped to 413.
	t.Run("non-overflow read error → 500", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", errReader{})
		_, ok := readCappedRequestBody(w, r, 1024, log)
		if ok {
			t.Fatalf("ok=true on a read error")
		}
		if w.Code != 500 {
			t.Fatalf("status = %d, want 500", w.Code)
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (errReader) Close() error             { return nil }
