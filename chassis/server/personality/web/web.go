package web

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

type WebController struct {
	ctx      context.Context
	pu       *processor.Unit
	sink     trace.Sink
	shutdown chan bool
	wg       sync.WaitGroup
	server   *http.Server

	// tlsConfig, when set (via SetTLSConfig), enables a second HTTPS
	// listener on --web-tls-addr that terminates TLS with the bundled cert
	// manager's certificates. Nil ⇒ HTTP-only (a front proxy terminates).
	tlsConfig *tls.Config
	tlsServer *http.Server
}

// SetTLSConfig wires the bundled cert manager's TLS config into the web
// head, enabling the HTTPS listener. Call before Start. No-op effect unless
// --web-tls-addr is also set.
func (web *WebController) SetTLSConfig(c *tls.Config) { web.tlsConfig = c }

// splitMocksHeader splits an X-Txco-Mocks header value into a clean
// pattern list: comma-separated, whitespace-trimmed, empties dropped.
// Returns nil for an empty / whitespace-only header so callers can
// skip the sjson.Set when there's nothing to store.
// isNoBodyStatus reports whether the response status forbids a body
// per RFC 7230 §3.3.3: 1xx informational, 204 No Content, 304 Not
// Modified. Writing a body in any of these cases is a Go stdlib error
// ("http: request method or response status code does not allow body")
// — harmless to the client (headers + status went out fine) but noisy
// in the log.
func isNoBodyStatus(status int) bool {
	if status >= 100 && status < 200 {
		return true
	}
	return status == 204 || status == 304
}

// canWriteBody is true when the response shape allows a body. HEAD
// responses must omit the body (the body bytes would be silently
// dropped by Go's stdlib, but a w.Write call still produces a noisy
// error log).
func canWriteBody(r *http.Request, status int) bool {
	return r.Method != http.MethodHead && !isNoBodyStatus(status)
}

func splitMocksHeader(h string) []string {
	if h == "" {
		return nil
	}
	parts := strings.Split(h, ",")
	cleaned := parts[:0]
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			cleaned = append(cleaned, p)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

func NewController(ctx context.Context, pu *processor.Unit, sink trace.Sink) *WebController {

	web := &WebController{
		ctx:      ctx,
		pu:       pu,
		sink:     sink,
		shutdown: make(chan bool),
	}

	return web
}

func (web *WebController) Start() {

	// stats.Record(wac.ctx, metrics.ServerRestarts.M(1))
	if strings.Contains(web.pu.Conf.Personalities, "web") {

		go func() {
			web.pu.Logger.Info("web controller started")

			r := mux.NewRouter()

			r.Use(web.CtxMiddleware)
			r.Use(web.LoggingMiddleware)

			// z-page, but what if we want to overwrite it?
			r.PathPrefix("/healthz").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// While draining (SIGUSR1), report unhealthy so the load
				// balancer pulls this node from rotation; SIGUSR2 restores 200.
				if admission.IsDraining() {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(http.StatusServiceUnavailable)
					if _, err := w.Write([]byte("draining\n")); err != nil {
						web.pu.Logger.Error("write error", zap.String("err", err.Error()))
					}
					return
				}
				w.WriteHeader(200)
				_, err := w.Write([]byte("ok\n"))
				if err != nil {
					web.pu.Logger.Error("write error", zap.String("err", err.Error()))
				}
			})

			// HTTP-01 hostname verification: serve the challenge
			// token at the well-known path so the chassis can fetch
			// itself externally and confirm DNS-points-at-chassis +
			// end-to-end-serving. Registered BEFORE the catch-all
			// so it bypasses the rule dispatch entirely — an
			// unverified hostname can't route, but we still need to
			// answer this specific URL.
			r.Path("/.well-known/txco-verify/{token}").HandlerFunc(web.handleVerifyToken).Methods(http.MethodGet)

			// Worker callback: a dedicated, token-authed POST registered
			// BEFORE the catch-all so it bypasses the opstack bus
			// entirely (single-use bearer; not behind BasicAuth; the
			// attacker-influenced worker body never enters the tenant
			// pipeline). The client poll/status is NOT here — it is the
			// same app URL + `?_txc.continuation=<rcid>`, handled through
			// the normal traced pipeline via txco://continuation-result
			// (see detectTenantBody short-circuit).
			r.Path("/_txc/continuations/op/{opc}/complete").
				HandlerFunc(web.handleContinuationComplete).Methods(http.MethodPost)

			r.PathPrefix("/").HandlerFunc(web.BasicAuth(func(w http.ResponseWriter, r *http.Request) {

				web.wg.Add(1)
				defer web.wg.Done()

				// Per-request pipeline-response-wait ceiling. Derived
				// from OpTimeoutMax (the chassis's ceiling on per-op
				// timeouts), NOT from WebWriteTimeout — those are two
				// different concerns and conflating them breaks any
				// long-running op (LLM-backed MCP calls,
				// continuation-style work) whose `WITH timeout` is
				// generous but greater than WebWriteTimeout. The
				// http.Server.WriteTimeout setting at the listener
				// level still uses WebWriteTimeout for its
				// slowloris-style connection defense.
				maxWait, perr := time.ParseDuration(web.pu.Conf.OpTimeoutMax)
				if perr != nil || maxWait <= 0 {
					// Fall back to the legacy behavior if OpTimeoutMax
					// is misconfigured: at least the request gets bounded.
					maxWait = time.Duration(web.pu.Conf.WebWriteTimeout) * time.Second
				}
				ctx, cancel := context.WithTimeout(r.Context(), maxWait)
				defer cancel()

				now := time.Now()
				rid := r.Header.Get("x-request-id")
				if rid == "" {
					rid = hxid.NewTimeSort().String()
				}
				w.Header().Set("X-Request-ID", rid)
				ctx = context.WithValue(ctx, config.CtxKeyRid, rid)
				web.pu.Logger.Debug("web rid", zap.String("rid", rid))

				// create the payload
				payload, _ := sjson.Set("", "_txc.src", "http")
				payload, _ = sjson.Set(payload, "_txc.rid", rid)
				payload, _ = sjson.Set(payload, "_txc.web.req.headers", r.Header)
				payload, _ = sjson.Set(payload, "_txc.web.req.host", r.Host)
				payload, _ = sjson.Set(payload, "_txc.web.req.proto", r.Proto)
				payload, _ = sjson.Set(payload, "_txc.web.req.method", r.Method)

				// Header-driven mock interception. Gated by WebMockHeader so
				// production chassis can't be coerced into mocking by a hostile
				// caller; flip it on per-environment in dev.
				if web.pu.Conf.WebMockHeader {
					if patterns := splitMocksHeader(r.Header.Get("X-Txco-Mocks")); len(patterns) > 0 {
						payload, _ = sjson.Set(payload, "_txc.mocks", patterns)
					}
				}

				rawCookies := r.Cookies()
				if len(rawCookies) > 0 {
					cookies := make(map[string][]interface{})

					for _, cookie := range rawCookies {
						cur, ok := cookies[cookie.Name]
						if ok {
							// existing
							cookies[cookie.Name] = append(cur, cookie.Value)
						} else {
							// new
							var c []interface{}
							cookies[cookie.Name] = append(c, cookie.Value)
						}
					}
					payload, _ = sjson.Set(payload, "_txc.web.req.cookies", cookies)
				}
				payload, _ = sjson.Set(payload, "_txc.web.req.url.full", r.URL.String())
				if r.URL.Scheme != "" {
					payload, _ = sjson.Set(payload, "_txc.web.req.url.scheme", r.URL.Scheme)
				}
				if r.URL.Path != "" {
					payload, _ = sjson.Set(payload, "_txc.web.req.url.path", r.URL.Path)
				}
				if r.URL.User != nil {
					payload, _ = sjson.Set(payload, "_txc.web.req.url.user", r.URL.User.Username)
				}
				if r.URL.Hostname() != "" {
					payload, _ = sjson.Set(payload, "_txc.web.req.url.hostname", r.URL.Hostname())
				}
				if r.URL.Port() != "" {
					payload, _ = sjson.Set(payload, "_txc.web.req.url.port", r.URL.Port())
				}
				if r.URL.RawQuery != "" {
					qp, err := url.ParseQuery(r.URL.RawQuery)
					if err != nil {
						web.pu.Logger.Info("fatal query parser error")
						w.WriteHeader(http.StatusInternalServerError)
						_, err := w.Write([]byte("query error"))
						if err != nil {
							// TODO: error handling
							web.pu.Logger.Error("write error", zap.String("err", err.Error()))
						}
						cancel() // shut down the request
						return
					}
					payload, _ = sjson.Set(payload, "_txc.web.req.url.query", qp)
					payload, _ = sjson.Set(payload, "_txc.web.req.url.query.raw", r.URL.RawQuery)
				}

				body, err := io.ReadAll(r.Body)
				if err != nil {
					web.pu.Logger.Info("error reading body", zap.Reflect("err", err))
					w.WriteHeader(http.StatusInternalServerError)
					_, err := w.Write([]byte("error reading body"))
					if err != nil {
						// TODO: error handling
						web.pu.Logger.Error("write error", zap.String("err", err.Error()))
					}
					cancel() // shut down the request
					return
				}
				if len(body) > 0 {
					body := base64.StdEncoding.EncodeToString([]byte(body))
					payload, _ = sjson.Set(payload, "_txc.web.req.body", body)
				}

				payload, _ = sjson.Set(payload, "_ts", now.Format(time.RFC3339))

				// Breakpoint plumbing: when --debug-breakpoints is on,
				// stamp the gating flag and pull ?_txc.break=N from the
				// query string into the envelope. Production chassis
				// don't set DebugBreakpoints, so the query param is a
				// no-op there (defense in depth: even if a rule SETs
				// _txc.break, the processor ignores it without the flag).
				if web.pu.Conf.DebugBreakpoints {
					payload, _ = sjson.Set(payload, "_txc.flag_breakpoint", true)
					if bs := r.URL.Query().Get("_txc.break"); bs != "" {
						// Two accepted forms:
						//   ?_txc.break=N            — any stack, break at scope ≥ N
						//   ?_txc.break=stack/N      — only inside <stack>
						// Stack names can contain `/` (e.g. _sys/boot),
						// so split on the LAST `/`.
						breakStack := ""
						scopeStr := bs
						if i := strings.LastIndex(bs, "/"); i >= 0 {
							breakStack = bs[:i]
							scopeStr = bs[i+1:]
						}
						if n, parseErr := strconv.Atoi(scopeStr); parseErr == nil {
							payload, _ = sjson.Set(payload, "_txc.break", n)
							if breakStack != "" {
								payload, _ = sjson.Set(payload, "_txc.break_stack", breakStack)
							}
						}
					}
				}

				// Continuation poll: `?_txc.continuation=<rcid>` marks a
				// poll/deferred-response for a suspended run. Read the
				// named param explicitly into `_txc.continuation` (same
				// pattern as ?_txc.break above; the generic query map uses
				// a dotted key that's awkward to address). detectTenantBody
				// short-circuits on this to the internal txc-continuation
				// stack — so the poll flows through the normal traced
				// pipeline. Not debug-gated: this is a product feature.
				if rc := r.URL.Query().Get("_txc.continuation"); rc != "" {
					payload, _ = sjson.Set(payload, "_txc.continuation", rc)
				}

				// Private-fields plumbing: when --debug-private is on,
				// stamp _txc.flag_private=true so getOutput keeps the
				// underscore-prefixed fields in the response body. The
				// default in production (no flag set) strips them so
				// the response is a clean public interface — survives
				// stage merging without leaking chassis internals.
				if web.pu.Conf.DebugPrivate {
					payload, _ = sjson.Set(payload, "_txc.flag_private", true)
				}

				// TODO: create a goroutine/channel to get os.Stat(host + path)

				// send event for processing
				var resCh = make(chan event.Payload) // response channel
				var envelope = event.PackageJSON(ctx, payload, resCh, "http")
				web.pu.Bus <- envelope

				// wait for response
				select {
				case res := <-resCh:

					// Streamed response: the processor flushed a body chunk
					// from a non-terminal scope. The first message is a
					// StreamHead (status + headers); take over and write
					// chunks incrementally as they arrive.
					if res.Type == event.StreamHead {
						web.writeStream(ctx, cancel, w, r, res, resCh)
						return
					}

					// Full response-body dump — debug only; at Info it
					// drowns prod logs (esp. every 15s health probe).
					web.pu.Logger.Debug("web res", zap.String("response", res.Raw))

					if !gjson.Valid(res.Raw) {
						w.WriteHeader(http.StatusInternalServerError)
						w.Header().Set("Content-Type", "text/plain")
						if canWriteBody(r, http.StatusInternalServerError) {
							if _, err := w.Write([]byte("processing failed")); err != nil {
								web.pu.Logger.Error("write error", zap.String("err", err.Error()))
							}
						}
						web.pu.Logger.Info("web fail", zap.Reflect("bad parse", res.Raw))
						return
					}

					// Breakpoint response: when the processor halted at a
					// requested scope, it stamps _txc.broke_at on the
					// envelope. Return the raw merged envelope as JSON so
					// devs can inspect data flowing between stages, bypassing
					// the normal _txc.web.res.* render path entirely.
					if brokeAt := gjson.Get(res.Raw, "_txc.broke_at"); brokeAt.Exists() {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("X-Txc-Break-At", brokeAt.String())
						w.WriteHeader(http.StatusOK)
						if canWriteBody(r, http.StatusOK) {
							if _, werr := w.Write([]byte(res.Raw)); werr != nil {
								web.pu.Logger.Error("write error", zap.String("err", werr.Error()))
							}
						}
						return
					}

					output := res.Raw // is this doubling the allocation?

					// Shared admission gate denials arrive transport-neutral
					// (_txc.admission.*); render them as this outlet's HTTP
					// status/body before the normal _txc.web.res.* path.
					output = applyAdmission(output)

					// after processing, we need to check: status, content-type, headers, body (in that order)
					output, status := checkStatus(output)
					output = checkContentType(output)

					// output any headers
					gjson.Get(output, "_txc.web.res.headers").ForEach(func(key, value gjson.Result) bool {
						hp := "_txc.web.res.headers." + key.String()
						gjson.Get(output, hp).ForEach(func(k, v gjson.Result) bool {
							w.Header().Set(key.String(), v.String())
							return true
						})
						return true
					})

					// if body, then return body
					// if no body, then return json
					hidePrivate := !strings.Contains(web.pu.Conf.WebDebug, "SHOW_PRIVATE_VARS")

					outputBytes, err := getOutput(output, hidePrivate)
					if err != nil {
						web.pu.Logger.Warn("error getting output", zap.Reflect("err", err))
						status = http.StatusInternalServerError
						outputBytes = []byte("output error")
					}

					w.WriteHeader(status)
					if canWriteBody(r, status) {
						if _, err = w.Write(outputBytes); err != nil {
							web.pu.Logger.Error("write error", zap.String("err", err.Error()))
						}
					}
				case <-ctx.Done():
					// Include rid + method + path so a timeout line maps
					// directly to the request (and to its trace, if trace
					// mode is on) without cross-referencing the later "web"
					// access-log line by sid. The pipeline was still
					// running when the deadline fired — pull the rid's
					// trace to see which stage blocked.
					web.pu.Logger.Info("web response timeout",
						zap.String("rid", rid),
						zap.String("method", r.Method),
						zap.String("url", r.URL.Path))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					if canWriteBody(r, http.StatusServiceUnavailable) {
						if _, err = w.Write([]byte("timeout")); err != nil {
							web.pu.Logger.Error("write error", zap.String("err", err.Error()))
						}
					}
					cancel()
				case <-web.ctx.Done():
					web.pu.Logger.Info("web service shutdown")
					w.WriteHeader(http.StatusServiceUnavailable)
					if canWriteBody(r, http.StatusServiceUnavailable) {
						if _, err = w.Write([]byte("shutting down")); err != nil {
							web.pu.Logger.Error("write error", zap.String("err", err.Error()))
						}
					}
					cancel() // shut down the request
				}

			},
				web.pu.Conf.WebUser,
				web.pu.Conf.WebPass,
				"auth"))

			// Go's http.Server.WriteTimeout starts when request
			// headers are read (not when writing begins), so it is
			// effectively the total post-headers request budget at
			// the listener level. We derive it from OpTimeoutMax
			// (the chassis-wide ceiling on per-op timeouts) so a
			// rule legitimately running close to OpTimeoutMax isn't
			// cut off by a tighter listener timeout. Slowloris-
			// style defense moves onto ReadHeaderTimeout
			// (WebWriteTimeout seconds), which bounds only the
			// header-read phase — a hostile slow client can't hold
			// a connection open by trickling headers.
			httpWriteTimeout, perr := time.ParseDuration(web.pu.Conf.OpTimeoutMax)
			if perr != nil || httpWriteTimeout <= 0 {
				httpWriteTimeout = time.Duration(web.pu.Conf.WebWriteTimeout) * time.Second
			}
			web.server = &http.Server{
				Addr:              web.pu.Conf.WebAddr,
				Handler:           r,
				ReadHeaderTimeout: time.Duration(web.pu.Conf.WebWriteTimeout) * time.Second,
				WriteTimeout:      httpWriteTimeout,
				ReadTimeout:       time.Duration(web.pu.Conf.WebReadTimeout) * time.Second,
				IdleTimeout:       time.Duration(web.pu.Conf.WebIdleTimeout) * time.Second,
				MaxHeaderBytes:    1 << 18,
			}

			// Optional HTTPS listener: bundled TLS termination. Same handler,
			// same timeouts; certificates come from the cert manager's
			// TLSConfig (GetCertificate by SNI). Served in its own goroutine —
			// the plain HTTP serve below still blocks this outer goroutine, as
			// before. Off unless --web-tls-addr is set AND a TLS config was
			// wired in (SetTLSConfig).
			if addr := strings.TrimSpace(web.pu.Conf.WebTLSAddr); addr != "" && web.tlsConfig != nil {
				web.tlsServer = &http.Server{
					Addr:              addr,
					Handler:           r,
					TLSConfig:         web.tlsConfig,
					ReadHeaderTimeout: time.Duration(web.pu.Conf.WebWriteTimeout) * time.Second,
					WriteTimeout:      httpWriteTimeout,
					ReadTimeout:       time.Duration(web.pu.Conf.WebReadTimeout) * time.Second,
					IdleTimeout:       time.Duration(web.pu.Conf.WebIdleTimeout) * time.Second,
					MaxHeaderBytes:    1 << 18,
				}
				tlsLn, terr := net.Listen("tcp", addr)
				if terr != nil {
					web.pu.Logger.Fatal("web TLS port already in use (or otherwise unbindable)",
						zap.String("addr", addr), zap.String("err", terr.Error()),
						zap.String("hint", fmt.Sprintf("lsof -iTCP%s -sTCP:LISTEN", addr)))
				}
				web.pu.Logger.Info("Listening on web TLS port", zap.String("port", addr))
				web.wg.Add(1)
				go func() {
					defer web.wg.Done()
					// Empty cert/key paths → ServeTLS uses TLSConfig.GetCertificate.
					if err := web.tlsServer.ServeTLS(tlsLn, "", ""); err != nil && err != http.ErrServerClosed {
						web.pu.Logger.Error("web TLS server stopped", zap.String("error", err.Error()))
					}
				}()
			}

			// Pre-bind synchronously so a port conflict surfaces with a
			// clear, actionable error BEFORE we log "Listening on..."
			// and BEFORE the chassis ever appears "ready". Without this
			// step, ListenAndServe's bind error arrives asynchronously
			// after the misleading "Listening" line, and a second
			// chassis racing for the same port appears to start
			// successfully from the operator's terminal.
			listener, err := net.Listen("tcp", web.pu.Conf.WebAddr)
			if err != nil {
				web.pu.Logger.Fatal("web port already in use (or otherwise unbindable)",
					zap.String("addr", web.pu.Conf.WebAddr),
					zap.String("err", err.Error()),
					zap.String("hint", fmt.Sprintf("lsof -iTCP%s -sTCP:LISTEN", web.pu.Conf.WebAddr)))
			}

			web.pu.Logger.Info("Listening on web port", zap.String("port", web.pu.Conf.WebAddr))
			web.wg.Add(1)
			if err := web.server.Serve(listener); err != nil && err != http.ErrServerClosed {
				// Error starting or closing listener
				web.pu.Logger.Fatal("could not start webserver", zap.String("error", err.Error()))
			}
			web.pu.Logger.Info("Web shutdown")
			web.wg.Done()
			web.Stop()
		}()
	}
}

// writeStream serves a streamed HTTP response. The processor sends a
// StreamHead (status + headers snapshot), then zero or more StreamChunk
// messages (raw body bytes), then a StreamEnd. We write the head once,
// then write+flush each chunk as it arrives so bytes reach the client
// incrementally. No Content-Length is set, so net/http uses chunked
// transfer encoding. Each receive unblocks the processor's blocking send
// on the next chunk — natural backpressure. If the writer can't flush,
// bytes still go out buffered (correct, just not incremental).
func (web *WebController) writeStream(
	ctx context.Context,
	cancel context.CancelFunc,
	w http.ResponseWriter,
	r *http.Request,
	head event.Payload,
	resCh chan event.Payload,
) {
	status := applyResponseHead(w, head.Raw)
	w.WriteHeader(status)
	writeBody := canWriteBody(r, status)
	rc := http.NewResponseController(w)

	for {
		select {
		case msg := <-resCh:
			switch msg.Type {
			case event.StreamChunk:
				if !writeBody {
					// HEAD/204/304: drain chunks so the processor unblocks,
					// but write nothing.
					continue
				}
				if _, err := w.Write([]byte(msg.Raw)); err != nil {
					web.pu.Logger.Debug("stream write error", zap.String("err", err.Error()))
					cancel()
					return
				}
				_ = rc.Flush() // unsupported flusher → buffered, still correct
			case event.StreamEnd:
				return
			}
		case <-ctx.Done():
			cancel()
			return
		case <-web.ctx.Done():
			cancel()
			return
		}
	}
}

// handleVerifyToken serves the HTTP-01 hostname verification challenge.
// The hostname-verification flow stores a token in
// tenant_hostname_challenges and the chassis self-fetches this URL
// (via the dialer-pinned client in chassis/tenants/verifier.go). If
// the token matches an active, non-expired challenge, we return the
// token as plain text — anything else is 404.
//
// Throttled and lookup-by-token-only: the token IS the secret. We
// never disclose hostnames or surface 4xx-vs-5xx differences that
// would help an attacker enumerate.
func (web *WebController) handleVerifyToken(w http.ResponseWriter, r *http.Request) {
	token := mux.Vars(r)["token"]
	if token == "" {
		http.NotFound(w, r)
		return
	}
	// Look up against the dbcache mirror so we don't hit on-disk
	// runtime.db on a request that anyone on the public internet can
	// trigger. The Lookup* methods in chassis/tenants use whichever
	// *sql.DB they were constructed with; here we construct a
	// short-lived store against the mirror specifically.
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	var storedToken string
	var expiresAt string
	err := web.pu.Dbc.Db.QueryRowContext(ctx,
		`SELECT token, expires_at FROM tenant_hostname_challenges
		  WHERE token = ?
		    AND verified_at IS NULL AND revoked_at IS NULL`, token).
		Scan(&storedToken, &expiresAt)
	if err != nil {
		// sql.ErrNoRows or any other error: opaque 404.
		http.NotFound(w, r)
		return
	}
	// Expiry check (string compare on RFC3339 is OK for this purpose).
	if expiresAt != "" && expiresAt < time.Now().UTC().Format(time.RFC3339) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, storedToken)
}

func (web *WebController) Stop() {
	if strings.Contains(web.pu.Conf.Personalities, "web") {
		web.pu.Logger.Info("calling web controller stop")

		// shut down workers
		if err := web.server.Shutdown(web.ctx); err != nil {
			// Error from closing listeners, or context timeout:
			web.pu.Logger.Error("web server shutdown", zap.String("error", err.Error()))
		}
		if web.tlsServer != nil {
			if err := web.tlsServer.Shutdown(web.ctx); err != nil {
				web.pu.Logger.Error("web TLS server shutdown", zap.String("error", err.Error()))
			}
		}
	}
}
