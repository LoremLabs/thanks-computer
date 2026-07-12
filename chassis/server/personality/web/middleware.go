package web

import (
	"context"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
)

type LogEntry struct {
	Host        string
	UserIp      string
	Method      string
	URL         string
	Status      int
	Size        int
	UserAgent   string
	RequestTime int
	Rid         string
}

type contextResponseWriter struct {
	http.ResponseWriter
	status int
	length int
	start  time.Time
	ctx    context.Context
}

func (w *contextResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		ms := time.Since(w.start).Milliseconds()
		w.ResponseWriter.Header().Set("Server-Timing", "total;dur="+strconv.FormatInt(ms, 10))
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *contextResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.length += n
	return n, err
}

// func (web *WebController) BasicAuthMiddleware(next http.Handler) http.Handler {
// 	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
// 		user, pass, _ := r.BasicAuth()
//
// 		// web.logger.Debug("basic auth", zap.String("u", user),zap.String("p", pass), )
// 		if len(web.pu.Conf.WebUser) != 0 || len(web.pu.Conf.WebPass) != 0 {
// 			if web.pu.Conf.WebUser != user || web.pu.Conf.WebPass != pass {
// 				// crw := contextResponseWriter{ResponseWriter: w}
// 				crw := w.(*contextResponseWriter)
//
// 				crw.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
// 				http.Error(crw, "Unauthorized", http.StatusUnauthorized)
// 				web.LogFromRequest(crw, r, 0)
// 				return
// 			}
// 		}
//
// 		next.ServeHTTP(w, r)
// 	})
// }

// add ctx to pass things to logging
func (web *WebController) CtxMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Identify the serving chassis on every response, e.g.
		//   X-Txco-Served-By: eu-hel1-worker-03; sid=CbGE5aVLZxLW3eZuErQXj
		// host is the short hostname (first label of Fqdn) — stable across
		// restarts, no domain-suffix leakage; sid is the per-process id that
		// ties the response to that boot's log lines. Set before
		// next.ServeHTTP so the header is on the wire even for 404s and
		// other early returns.
		host := web.pu.Conf.Fqdn
		if i := strings.IndexByte(host, '.'); i > 0 {
			host = host[:i]
		}
		sid := web.pu.Conf.ServerId
		switch {
		case host != "" && sid != "":
			w.Header().Set("X-Txco-Served-By", host+"; sid="+sid)
		case sid != "":
			w.Header().Set("X-Txco-Served-By", "sid="+sid)
		case host != "":
			w.Header().Set("X-Txco-Served-By", host)
		}
		crw := contextResponseWriter{ResponseWriter: w, ctx: web.ctx, start: time.Now()}
		next.ServeHTTP(&crw, r)
	})
}

// simple logging
func (web *WebController) LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		crw := w.(*contextResponseWriter)
		// crw := contextResponseWriter{ResponseWriter: w, ctx: web.ctx}
		next.ServeHTTP(crw, r)
		rt := int(time.Since(start) / time.Millisecond)
		web.LogFromRequest(crw, r, rt)
	})
}

func (web *WebController) LogFromRequest(crw *contextResponseWriter, r *http.Request, rt int) {
	userIp := r.RemoteAddr
	fwdAddress := r.Header.Get("X-Forwarded-For") // capitalisation doesn't matter
	if fwdAddress != "" {
		// Got X-Forwarded-For
		userIp = fwdAddress // If it's a single IP, then awesome!

		// If we got an array... grab the first IP
		ips := strings.Split(fwdAddress, ", ")
		if len(ips) > 1 {
			userIp = ips[0]
		}
	}

	if strings.ContainsRune(userIp, ':') {
		userIp, _, _ = net.SplitHostPort(userIp)
	}

	rid := crw.Header().Get("X-Request-Id")
	if rid == "" {
		rid = "unset"
	}

	web.Log(LogEntry{
		Host:        r.Host,
		UserIp:      userIp,
		Method:      r.Method,
		URL:         r.RequestURI,
		Status:      crw.status,
		Size:        crw.length,
		UserAgent:   r.Header.Get("User-Agent"),
		RequestTime: rt,
		Rid:         rid,
	})

	// // TODO: add stats
	// ctx, _ = tag.New(ctx, tag.Upsert(metrics.KeyWebServerStatus, strconv.Itoa(crw.status)))
	// ctx, _ = tag.New(ctx, tag.Upsert(metrics.KeyWebSubsys, subsys))
	// stats.Record(ctx, metrics.WebRequest.M(1))
}

func (web *WebController) Log(entry LogEntry) {
	// "host" is the node's own FQDN (logger base field); the request's
	// Host header rides "web.host" so multi-tenant traffic stays
	// attributable per site.
	fields := []zap.Field{
		zap.String("web.host", entry.Host),
		zap.String("url", entry.URL),
		zap.String("rid", entry.Rid),
		zap.String("userIp", entry.UserIp),
		zap.Int("status", entry.Status),
		zap.Int("size", entry.Size),
		zap.Int("rt", entry.RequestTime),
		zap.String("method", strings.ToUpper(entry.Method)),
		zap.String("ua", entry.UserAgent),
	}
	// Health probes (container/LB healthcheck, every ~15s) are not
	// real traffic — keep them out of the prod access log. Still
	// visible at --log-level=debug.
	if isHealthProbePath(entry.URL) {
		web.pu.Logger.Debug("web", fields...)
		return
	}
	web.pu.Logger.Info("web", fields...)
}

// isHealthProbePath reports whether a request-URI is one of the
// chassis health endpoints (the pipeline `/_txc/healthz` boot rule or
// the static `/healthz`), ignoring any query string.
func isHealthProbePath(uri string) bool {
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		uri = uri[:i]
	}
	return uri == "/_txc/healthz" || uri == "/healthz"
}
