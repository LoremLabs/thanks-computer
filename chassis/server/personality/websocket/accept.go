package websocket

import (
	"crypto/rand"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/oklog/ulid/v2"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
)

// pendingAccept is a recorded decision waiting for the web head to claim it.
type pendingAccept struct {
	acc Accept
	at  time.Time
}

// IsUpgrade reports whether the request asks for a WebSocket upgrade:
// GET with `Connection: upgrade` and `Upgrade: websocket` (token lists,
// case-insensitive). The web head stamps `_txc.websocket.upgrade` on
// exactly these requests; everything else is ordinary HTTP.
func IsUpgrade(r *http.Request) bool {
	if r == nil || r.Method != http.MethodGet {
		return false
	}
	return headerHasToken(r.Header, "Connection", "upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

// RequestedSubprotocols parses Sec-WebSocket-Protocol (a comma list that
// may span several header lines) into a clean list.
func RequestedSubprotocols(r *http.Request) []string {
	var out []string
	for _, line := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, tok := range strings.Split(line, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out = append(out, tok)
			}
		}
	}
	return out
}

func headerHasToken(h http.Header, key, want string) bool {
	for _, line := range h.Values(key) {
		for _, tok := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), want) {
				return true
			}
		}
	}
	return false
}

// NewSessionID mints an opaque, unguessable, time-sortable session id:
// `ws_` + a ULID whose 80 entropy bits come from crypto/rand. Not hxid —
// its ULID entropy is math/rand-seeded, fine for a request id and wrong
// for an id that names a socket.
func (c *Controller) NewSessionID() string {
	return "ws_" + ulid.MustNew(ulid.Timestamp(c.now()), rand.Reader).String()
}

// RecordAccept implements Registry: the txco://websocket/accept op stores
// the decision for the session id the web head minted.
func (c *Controller) RecordAccept(sid string, a Accept) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	if a.IdleTimeout <= 0 || a.IdleTimeout > c.lim.maxIdleTimeout {
		if a.IdleTimeout > c.lim.maxIdleTimeout {
			a.IdleTimeout = c.lim.maxIdleTimeout
		} else {
			a.IdleTimeout = c.lim.idleTimeout
		}
	}
	if a.MaxMessageBytes <= 0 || a.MaxMessageBytes > c.lim.maxMessageBytes {
		a.MaxMessageBytes = c.lim.maxMessageBytes
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.pending[sid]; !exists && len(c.pending) >= maxPending {
		return ErrPendingFull
	}
	c.pending[sid] = pendingAccept{acc: a, at: c.now()}
	return nil
}

// Claim returns and removes the recorded accept for sid, if any.
func (c *Controller) Claim(sid string) (Accept, bool) {
	if !c.Enabled() || sid == "" {
		return Accept{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[sid]
	if !ok {
		return Accept{}, false
	}
	delete(c.pending, sid)
	if c.now().Sub(p.at) > acceptTTL {
		return Accept{}, false
	}
	return p.acc, true
}

func (c *Controller) sweepPending(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sid, p := range c.pending {
		if now.Sub(p.at) > acceptTTL {
			delete(c.pending, sid)
		}
	}
}

// Upgrade performs the handshake for a claimed accept and starts the
// session. It writes the HTTP response itself on every refusal, so the
// caller returns unconditionally afterwards. The request context dies
// with the handler; the session roots on the controller's context.
func (c *Controller) Upgrade(w http.ResponseWriter, r *http.Request, sid string, a Accept) {
	refuse := func(status int, body, why string) {
		c.record(c.upgrades, outcome(why))
		if status == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Connection", "close")
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body + "\n"))
		c.pu.Logger.Info("websocket upgrade refused",
			zap.String("tenant", a.Tenant), zap.String("stack", a.Stack),
			zap.String("sid", sid), zap.String("why", why), zap.Int("status", status))
	}
	if c.stopping.Load() || admission.IsDraining() {
		refuse(http.StatusServiceUnavailable, "draining", "draining")
		return
	}
	if !c.reserve(a.Tenant) {
		refuse(http.StatusServiceUnavailable, "too many connections", "capped")
		return
	}
	if a.SubprotocolRequired && !offersAny(RequestedSubprotocols(r), a.Subprotocols) {
		c.releaseSlot(a.Tenant)
		refuse(http.StatusBadRequest, "no acceptable subprotocol", "subprotocol")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       a.Subprotocols,
		OriginPatterns:     a.Origins,
		InsecureSkipVerify: a.AnyOrigin,
	})
	if err != nil {
		// Accept already wrote the 4xx (bad handshake, forbidden origin).
		c.releaseSlot(a.Tenant)
		c.record(c.upgrades, outcome("rejected"))
		c.pu.Logger.Info("websocket upgrade rejected",
			zap.String("tenant", a.Tenant), zap.String("stack", a.Stack),
			zap.String("sid", sid), zap.String("err", err.Error()))
		return
	}
	conn.SetReadLimit(a.MaxMessageBytes)

	s := newSession(c, sid, a, conn, r)
	c.register(s)
	c.record(c.upgrades, outcome("ok"))
	c.pu.Logger.Info("websocket open",
		zap.String("tenant", s.tenant), zap.String("stack", s.stack),
		zap.String("sid", s.id), zap.String("ip", s.clientIP),
		zap.String("path", s.path), zap.String("subprotocol", s.subprotocol))
	c.wg.Add(3)
	go s.reader()
	go s.worker()
	go s.pinger()
}

func offersAny(offered, allowed []string) bool {
	for _, o := range offered {
		for _, a := range allowed {
			if strings.EqualFold(o, a) {
				return true
			}
		}
	}
	return false
}

// clientIP mirrors the web access log: first X-Forwarded-For hop, else the
// peer address, host part only.
func clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = fwd
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			ip = strings.TrimSpace(fwd[:i])
		}
	}
	if strings.ContainsRune(ip, ':') {
		if h, _, err := net.SplitHostPort(ip); err == nil {
			ip = h
		}
	}
	return ip
}
