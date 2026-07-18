package llmgw

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"go.uber.org/zap"
)

// hopByHopHeaders are connection-scoped per RFC 9110 §7.6.1 and must not
// be forwarded in either direction. Host and Content-Length are handled
// by net/http itself from the outgoing request/response state.
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// copyHeaders copies src into dst minus hop-by-hop headers and any
// listed in a Connection header (RFC 9110 §7.6.1 second clause).
func copyHeaders(dst, src http.Header) {
	drop := map[string]bool{}
	for _, h := range hopByHopHeaders {
		drop[h] = true
	}
	for _, v := range src.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name)); name != "" {
				drop[name] = true
			}
		}
	}
	for k, vv := range src {
		if drop[textproto.CanonicalMIMEHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// forward builds and sends the upstream request from the stack's verdict.
// The base URL is the stack's override when present (scheme-checked by the
// caller), else the configured upstream; pathAndQuery is the client's own
// path + query verbatim (Claude Code appends ?beta=true — the query is
// protocol surface, not routing). Client headers ride along minus
// hop-by-hop and minus credentials in swap mode (the inbound credential
// was the gateway key, never meant for the upstream). ctx is the client
// request's context, so a client disconnect cancels the upstream call at
// any point.
func (g *Gateway) forward(ctx context.Context, v verdict, base, pathAndQuery string, clientHdr http.Header, authMode, upstreamKey string, forceIdentity bool) (*http.Response, error) {
	u := strings.TrimRight(base, "/") + pathAndQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader([]byte(v.request)))
	if err != nil {
		return nil, err
	}
	copyHeaders(req.Header, clientHdr)
	req.Header.Del("Content-Length") // recomputed from the possibly-rewritten body
	if forceIdentity {
		// Streaming policy requests: ask the upstream for an unencoded
		// stream so the passive usage capture can parse the SSE frames.
		// Field finding: clients like Claude Code (Bun) send
		// Accept-Encoding: gzip/br/zstd, the upstream edge honors it on
		// event-streams, and an encoded stream makes capture decline
		// (unsupported_encoding). Identity is HTTP-semantically the same
		// content; the client sees an unencoded stream with no
		// Content-Encoding header — valid under its own negotiation.
		// Non-stream responses keep the client's encoding (the JSON
		// capture path inflates gzip itself).
		req.Header.Set("Accept-Encoding", "identity")
	}
	if authMode == authModeSwap {
		// The inbound x-api-key was the tenant's gateway key; replace it
		// with the real upstream credential and drop any client
		// Authorization so a stray client token never reaches upstream.
		req.Header.Set("x-api-key", upstreamKey)
		req.Header.Del("Authorization")
	}
	for name, val := range v.headers {
		req.Header.Set(name, val)
	}
	return g.client.Do(req)
}

// proxyBody streams the upstream body to the client: per read, extend the
// write deadline, write, flush. Flush-per-read makes SSE incremental; on
// a plain JSON body it is harmless. The rolling deadline bounds a client
// that stops reading (stall) while leaving total stream length unbounded —
// the listener-level WriteTimeout alone would kill any response longer
// than OpTimeoutMax. Both controller calls degrade gracefully when the
// writer chain doesn't support them (buffered, still correct).
//
// tee, when non-nil, passively observes every byte read from the
// upstream (usage capture). It is fed BEFORE the client write so bytes
// already received are captured even when the client write then fails
// (the disconnect case), and it never errors or blocks — observability
// must not be able to break the response.
func proxyBody(w http.ResponseWriter, body io.Reader, stall time.Duration, tee io.Writer, log *zap.Logger) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32*1024)
	var written int64
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if tee != nil {
				_, _ = tee.Write(buf[:n])
			}
			_ = rc.SetWriteDeadline(time.Now().Add(stall))
			wn, werr := w.Write(buf[:n])
			written += int64(wn)
			if werr != nil {
				return written, werr
			}
			_ = rc.Flush()
		}
		if rerr == io.EOF {
			return written, nil
		}
		if rerr != nil {
			return written, rerr
		}
	}
}
