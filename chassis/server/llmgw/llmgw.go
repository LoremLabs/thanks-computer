// Package llmgw is the AI-gateway inlet: existing AI clients (Claude
// Code first) point their base URL at the chassis, their Anthropic
// Messages request runs once through the tenant's `_llm` stack (which
// may reject it, rewrite it, or repoint the upstream), and the upstream
// response streams back byte-transparent. The inlet owns the transport;
// stacks own policy — the SSE stream is never routed through the
// processor. After the exchange, completion metadata is recorded
// asynchronously (usage event + a phase="completed" envelope into the
// same stack); no client waits on it.
//
// Design doc: thanks-computer-service/docs/todo-ai-gateway-inlet.md.
package llmgw

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

const (
	srcName        = "llm"
	stackName      = "_llm"
	phaseRequest   = "request"
	phaseCompleted = "completed"
	protocolName   = "anthropic.messages"
	defaultPath    = "/v1/messages"

	// Tenant secret names. ANTHROPIC_KEY present ⇒ swap mode: the client
	// authenticates with the gateway key and the real key never leaves
	// the server. Absent ⇒ passthrough: the client's own credential is
	// forwarded verbatim.
	secretUpstreamKey = "ANTHROPIC_KEY"
	secretGatewayKey  = "LLM_GATEWAY_KEY"

	authModeSwap        = "swap"
	authModePassthrough = "passthrough"

	// writeStall bounds how long a single client write may block (a sink
	// that stopped reading), while total stream length stays unbounded.
	writeStall = 60 * time.Second
)

type Gateway struct {
	ctx      context.Context
	pu       *processor.Unit
	resolver *ingress.DBResolver
	upstream string // configured base URL, already validated
	client   *http.Client
	maxWait  time.Duration // stack round-trip ceiling (OpTimeoutMax)
	log      *zap.Logger

	// Seams for tests; New wires the real implementations.
	resolveHost func(host string) (ingress.RouteTarget, bool, error)
	stackExists func(ctx context.Context, tenant string) (bool, error)
	secret      func(ctx context.Context, tenant, name string) ([]byte, error)
}

// New builds the gateway. The upstream client mirrors the processor's
// egress-guarded transport (dial-time IP check under the chassis egress
// policy) but with NO client-level timeout — an LLM stream legitimately
// runs for many minutes; cancellation is the request context — and with
// compression disabled so the client's own Accept-Encoding negotiates
// end-to-end and response bytes pass through untouched.
func New(ctx context.Context, pu *processor.Unit, resolver *ingress.DBResolver, guard egress.Guard) (*Gateway, error) {
	base := strings.TrimSpace(pu.Conf.LLMUpstreamURL)
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("llmgw: invalid --llm-upstream-url: " + base)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control:   egress.DialControl(guard),
	}).DialContext
	transport.DisableCompression = true
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 100

	maxWait, perr := time.ParseDuration(pu.Conf.OpTimeoutMax)
	if perr != nil || maxWait <= 0 {
		maxWait = time.Duration(pu.Conf.WebWriteTimeout) * time.Second
	}

	g := &Gateway{
		ctx:      ctx,
		pu:       pu,
		resolver: resolver,
		upstream: base,
		client:   &http.Client{Transport: transport},
		maxWait:  maxWait,
		log:      pu.Logger,
	}
	g.resolveHost = g.defaultResolveHost
	g.stackExists = g.defaultStackExists
	g.secret = g.defaultSecret
	return g, nil
}

// HandleMessages serves POST /v1/messages. Registered ahead of the web
// catch-all: no BasicAuth, no opstack render path — the gateway shapes
// its own (Anthropic-protocol) responses.
func (g *Gateway) HandleMessages(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, true)
}

// HandleCountTokens serves POST /v1/messages/count_tokens as a
// stackless transparent forward. Claude Code calls it constantly for
// context management; it's a cheap metadata echo of the request, so
// policy (the _llm stack) doesn't apply — but tenant routing, the _llm
// self-gate, and auth do, exactly as for /v1/messages. Without this
// route the path would fall through to the tenant's web stack and
// answer with something that isn't a token count.
func (g *Gateway) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, false)
}

// serve is the shared request path. runPolicy=true (messages) runs the
// _llm stack round-trip and records completion; false (count_tokens)
// forwards the request untouched and unrecorded.
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, runPolicy bool) {
	start := time.Now()
	rid := r.Header.Get("x-request-id")
	if rid == "" {
		rid = hxid.NewTimeSort().String()
	}
	w.Header().Set("X-Request-ID", rid)

	// 1. Body, capped like the web inlet (the read is pre-admission, so
	// the cap bounds unauthenticated memory) — but the 413/400 are
	// Anthropic-shaped here.
	if max := g.pu.Conf.WebMaxBodyBytes; max > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, int64(max))
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, errTypeRequestTooLarge, "request body too large", "")
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, errTypeInvalidRequest, "error reading request body", "")
		return
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		writeAnthropicError(w, http.StatusBadRequest, errTypeInvalidRequest, "request body must be a JSON object", "")
		return
	}
	streamRequested := gjson.GetBytes(body, "stream").Bool()
	model := gjson.GetBytes(body, "model").String()

	// 2. Tenant from Host — the same hostname→tenant routing every web
	// request uses. A transient resolver failure is an honest 503, never
	// a fabricated 404 (todo-route-resolution-404-under-load).
	target, ok, rerr := g.resolveHost(r.Host)
	if rerr != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, errTypeAPI, "routing temporarily unavailable", "1")
		return
	}
	if !ok {
		writeAnthropicError(w, http.StatusNotFound, errTypeNotFound, "no gateway configured for this host", "")
		return
	}

	// 3. Self-gating: a tenant opts in by authoring an _llm stack;
	// without one this endpoint doesn't exist for their hostname.
	hasStack, serr := g.stackExists(r.Context(), target.Tenant)
	if serr != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, errTypeAPI, "gateway temporarily unavailable", "1")
		return
	}
	if !hasStack {
		writeAnthropicError(w, http.StatusNotFound, errTypeNotFound, "no gateway configured for this host", "")
		return
	}

	// 4. Auth: swap-if-configured.
	authMode, upstreamKey, authOK := g.authenticate(w, r, target.Tenant)
	if !authOK {
		return // authenticate wrote the error
	}

	// 5. One stack round-trip over the bus — admission, tracing, and
	// usage apply exactly as they do to every other inlet. count_tokens
	// (runPolicy=false) skips the stack: it's a metadata echo, and its
	// call volume would double every _llm trace for no policy value.
	v := verdict{request: string(body)}
	base := g.upstream
	if runPolicy {
		ctx, cancel := context.WithTimeout(r.Context(), g.maxWait)
		defer cancel()
		ctx = context.WithValue(ctx, config.CtxKeyRid, rid)
		payload := requestPayload(rid, target.Tenant, target.Verified, r.Host, body, streamRequested, authMode, r.Header)
		final, runErr := g.runStack(ctx, payload)
		if runErr != nil {
			g.log.Info("llmgw stack run failed", zap.String("rid", rid),
				zap.String("tenant", target.Tenant), zap.String("err", runErr.Error()))
			writeAnthropicError(w, http.StatusInternalServerError, errTypeAPI, "gateway processing failed", "")
			return
		}

		// 6. Verdicts, most-final first.
		v = parseVerdict(final)
		if v.unavailable {
			writeAnthropicError(w, http.StatusServiceUnavailable, errTypeAPI, "routing temporarily unavailable", "1")
			return
		}
		if v.admission {
			status := v.admStatus
			if status < 100 || status > 599 {
				status = http.StatusForbidden
			}
			errType := errTypePermission
			switch {
			case status == http.StatusTooManyRequests:
				errType = errTypeRateLimit
			case status >= 500:
				errType = errTypeAPI
			}
			msg := "request denied"
			if v.admReason != "" {
				msg = "request denied: " + v.admReason
			}
			writeAnthropicError(w, status, errType, msg, v.admRetry)
			return
		}
		if v.reject {
			writeAnthropicError(w, v.rejectStatus, v.rejectType, v.rejectMsg, "")
			return
		}
		if !gjson.Valid(v.request) || !gjson.Parse(v.request).IsObject() {
			g.log.Info("llmgw stack left no request object", zap.String("rid", rid),
				zap.String("tenant", target.Tenant))
			writeAnthropicError(w, http.StatusInternalServerError, errTypeAPI, "gateway processing failed", "")
			return
		}
		if v.upstreamURL != "" {
			ou, oerr := url.Parse(v.upstreamURL)
			if oerr != nil || (ou.Scheme != "http" && ou.Scheme != "https") || ou.Host == "" {
				g.log.Info("llmgw stack set an invalid upstream url", zap.String("rid", rid),
					zap.String("tenant", target.Tenant), zap.String("url", v.upstreamURL))
				writeAnthropicError(w, http.StatusInternalServerError, errTypeAPI, "gateway processing failed", "")
				return
			}
			base = v.upstreamURL
		}
		// The forwarded model may differ from the client's ask; report
		// what actually went upstream.
		if m := gjson.Get(v.request, "model").String(); m != "" {
			model = m
		}
	}

	// 7. Forward to the same path the client called, query string
	// included (Claude Code sends /v1/messages?beta=true — dropping the
	// query would silently change protocol behavior). The egress guard
	// rejects dials into private address space at connect time (a
	// stack-supplied override URL gets no more reach than any
	// EXEC "https://...").
	pathAndQuery := r.URL.Path
	if r.URL.RawQuery != "" {
		pathAndQuery += "?" + r.URL.RawQuery
	}
	resp, ferr := g.forward(r.Context(), v, base, pathAndQuery, r.Header, authMode, upstreamKey)
	if ferr != nil {
		if r.Context().Err() != nil {
			// Client went away while we were connecting: nothing to answer.
			if runPolicy {
				go g.fireCompletion(completion{
					rid: rid, tenant: target.Tenant, verified: target.Verified, host: r.Host,
					durationMS: time.Since(start).Milliseconds(), bytesIn: int64(len(body)),
					model: model, stream: streamRequested, upstream: base,
					clientDisconnected: true, errStr: "client disconnected",
				})
			}
			return
		}
		g.log.Info("llmgw upstream dial failed", zap.String("rid", rid),
			zap.String("tenant", target.Tenant), zap.String("upstream", base),
			zap.String("err", ferr.Error()))
		writeAnthropicError(w, http.StatusBadGateway, errTypeAPI, "upstream unavailable", "")
		if runPolicy {
			go g.fireCompletion(completion{
				rid: rid, tenant: target.Tenant, verified: target.Verified, host: r.Host,
				durationMS: time.Since(start).Milliseconds(), bytesIn: int64(len(body)),
				model: model, stream: streamRequested, upstream: base,
				errStr: ferr.Error(),
			})
		}
		return
	}
	defer resp.Body.Close()

	// 8. Byte-transparent response: upstream status + headers verbatim
	// (minus hop-by-hop), body streamed with per-chunk flush. Error
	// bodies pass through untouched — the client sees exactly what the
	// upstream said.
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	written, perr := proxyBody(w, resp.Body, writeStall, g.log)

	// 9. Completion — async; the response is already done. count_tokens
	// stays unrecorded: it ran no stack and meters nothing.
	if !runPolicy {
		return
	}
	c := completion{
		rid: rid, tenant: target.Tenant, verified: target.Verified, host: r.Host,
		status:     resp.StatusCode,
		durationMS: time.Since(start).Milliseconds(),
		bytesIn:    int64(len(body)),
		bytesOut:   written,
		model:      model,
		stream:     streamRequested,
		upstream:   base,
	}
	if perr != nil {
		c.errStr = perr.Error()
		c.clientDisconnected = r.Context().Err() != nil
	}
	go g.fireCompletion(c)
}

// authenticate implements swap-if-configured. Swap mode fails closed:
// with ANTHROPIC_KEY stored but LLM_GATEWAY_KEY missing (a misconfig) or
// mismatched, the answer is 401 — the real key is never spent on an
// unauthenticated request. A secret-store failure (as opposed to
// absence) is a 503: we cannot know which mode applies, so we serve
// neither. Returns ok=false after writing the error response.
func (g *Gateway) authenticate(w http.ResponseWriter, r *http.Request, tenant string) (mode, upstreamKey string, ok bool) {
	ak, err := g.secret(r.Context(), tenant, secretUpstreamKey)
	if errors.Is(err, secrets.ErrSecretNotFound) {
		return authModePassthrough, "", true
	}
	if err != nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, errTypeAPI, "gateway temporarily unavailable", "1")
		return "", "", false
	}
	defer secrets.Zero(ak)

	gk, gerr := g.secret(r.Context(), tenant, secretGatewayKey)
	if gerr != nil {
		if errors.Is(gerr, secrets.ErrSecretNotFound) {
			g.log.Warn("llmgw misconfig: ANTHROPIC_KEY stored without LLM_GATEWAY_KEY; refusing all clients",
				zap.String("tenant", tenant))
		} else {
			writeAnthropicError(w, http.StatusServiceUnavailable, errTypeAPI, "gateway temporarily unavailable", "1")
			return "", "", false
		}
		writeAnthropicError(w, http.StatusUnauthorized, errTypeAuthentication, "invalid x-api-key", "")
		return "", "", false
	}
	defer secrets.Zero(gk)

	inbound := r.Header.Get("x-api-key")
	if inbound == "" || subtle.ConstantTimeCompare(gk, []byte(inbound)) != 1 {
		writeAnthropicError(w, http.StatusUnauthorized, errTypeAuthentication, "invalid x-api-key", "")
		return "", "", false
	}
	return authModeSwap, string(ak), true
}

// defaultResolveHost is the Host→tenant lookup: the same DB-backed
// resolver (HostRouteCache-fronted) the web pipeline's detect-tenant op
// uses, keyed Src:"http" because the hostname table is an HTTP concept —
// the envelope's src stays "llm".
func (g *Gateway) defaultResolveHost(host string) (ingress.RouteTarget, bool, error) {
	if g.resolver == nil {
		return ingress.RouteTarget{}, false, nil
	}
	return g.resolver.ResolveErr(ingress.RouteKey{Src: "http", Hostname: host})
}

// defaultStackExists asks the in-memory dbcache mirror whether the tenant
// authored an _llm stack (cron's subscribers pattern: snapshot pointer
// under the lock, query unlocked). One point read per request — noise
// against a multi-second LLM exchange. No dbcache ⇒ no stacks ⇒ miss.
func (g *Gateway) defaultStackExists(ctx context.Context, tenant string) (bool, error) {
	if g.pu.Dbc == nil {
		return false, nil
	}
	g.pu.Dbc.Mu.Lock()
	snap := g.pu.Dbc.Db
	g.pu.Dbc.Mu.Unlock()
	if snap == nil {
		return false, nil
	}
	qctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	var one int
	err := snap.QueryRowContext(qctx,
		`SELECT 1
		   FROM ops o
		   JOIN tenants t ON t.tenant_id = o.tenant_id
		  WHERE t.slug = ? AND o.stack = ? AND t.revoked_at IS NULL
		  LIMIT 1`, tenant, stackName).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// defaultSecret materializes a tenant secret scoped to the _llm stack
// (stack-scoped → tenant-wide fallback is the resolver's own behavior).
// No secret store configured reads as absence — the gateway then runs in
// passthrough mode rather than failing.
func (g *Gateway) defaultSecret(ctx context.Context, tenant, name string) ([]byte, error) {
	if g.pu.Secrets == nil {
		return nil, secrets.ErrSecretNotFound
	}
	pt, _, err := g.pu.Secrets.MaterializeForOpSlug(ctx, tenant, stackName, name)
	return pt, err
}
