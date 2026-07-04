package admin

import (
	"bytes"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

// errorLoggingMiddleware wraps every admin response writer so that
// any non-2xx response gets surfaced in the chassis log alongside the
// response body. Without this, a handler that calls writeJSONError
// (chassis/server/admin/ops.go) sends a descriptive payload to the
// client — `{"error":"lookup_stack","detail":{"err":"database table
// is locked: stacks"}}` — but the chassis log itself is silent on the
// failure path. Operators chasing a 500 had to read DevTools' network
// panel to learn anything; this middleware shortcuts that.
//
// What's logged:
//
//   - 5xx → zap.Warn  (server-side fault; demands operator awareness)
//   - 4xx → zap.Debug (client-side issue; not worth WARN noise but
//     surfaces at --verbose for the same debugging
//     flow)
//   - 2xx/3xx → not logged
//
// What's NOT touched:
//
//   - Static asset paths (`/admin/`, `/favicon`, etc.) — these can
//     legitimately 404 when the SPA bundle isn't built; not worth
//     spamming logs over.
//   - /healthz — readiness probes hammer it; 2xx silent anyway, and a
//     5xx would already be caught by the supervisor.
//
// Response bodies are captured into a small bounded buffer (8 KiB,
// generous for typical admin error payloads). Bigger payloads still
// flow through to the client unchanged; only the prefix lands in the
// log. The wrapped writer also implements http.Flusher so streaming
// endpoints (like /traces/...) keep working.
const errorLogBodyMax = 8 << 10

func (c *Controller) errorLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bypass static-asset and probe paths — see header above.
		if skipErrorLog(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		erw := &errorLoggingWriter{ResponseWriter: w}
		next.ServeHTTP(erw, r)
		if erw.status < 400 || erw.status == 0 {
			return
		}
		// Choose log level by class. 5xx warrants WARN; 4xx is informational.
		fields := []zap.Field{
			zap.Int("status", erw.status),
			zap.String("method", r.Method),
			zap.String("path", r.URL.Path),
			zap.String("body", strings.TrimSpace(erw.body.String())),
		}
		if erw.status >= 500 {
			c.pu.Logger.Warn("admin response: server error", fields...)
		} else {
			c.pu.Logger.Debug("admin response: client error", fields...)
		}
	})
}

// skipErrorLog returns true for paths the middleware shouldn't bother
// logging. The admin static SPA bundle legitimately 404s on a fresh
// checkout that hasn't built the UI yet; we don't want every page-load
// 404 spamming logs.
func skipErrorLog(p string) bool {
	switch {
	case p == "/healthz":
		return true
	case strings.HasPrefix(p, "/admin/"):
		return true
	case p == "/favicon.ico" || p == "/favicon.png":
		return true
	}
	return false
}

// errorLoggingWriter captures the status code and the first
// errorLogBodyMax bytes of the response body. It transparently
// forwards both to the underlying ResponseWriter. Hijacker / Flusher
// are passed through where the underlying writer supports them, so
// streaming endpoints (e.g. /traces/) keep working.
type errorLoggingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *errorLoggingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *errorLoggingWriter) Write(p []byte) (int, error) {
	// Implicit 200 when a handler writes without calling WriteHeader.
	if w.status == 0 {
		w.status = http.StatusOK
	}
	// Capture into the log buffer only for error responses, and only
	// up to the cap. Avoid the buffer cost on the common 2xx path.
	if w.status >= 400 {
		remaining := errorLogBodyMax - w.body.Len()
		if remaining > 0 {
			n := len(p)
			if n > remaining {
				n = remaining
			}
			w.body.Write(p[:n])
		}
	}
	return w.ResponseWriter.Write(p)
}

// Flush passes through so streaming handlers (e.g. /traces/{rid}.json
// with a tailing iterator) keep working through this middleware.
func (w *errorLoggingWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer, so
// per-request deadline extensions (the blob plane's multi-GB uploads and
// downloads escape the small-JSON Read/WriteTimeout this way) work through
// this middleware instead of failing with ErrNotSupported.
func (w *errorLoggingWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
