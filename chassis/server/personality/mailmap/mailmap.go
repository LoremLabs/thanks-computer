// Package mailmap implements the chassis's "mail-domain accept map"
// head: a tiny TCP server that speaks the Postfix tcp_table(5) protocol
// so the edge Postfix can ask the chassis, per recipient domain, "is
// this an accepted mail domain?" before relaying it to the LMTP head.
//
// This is what lets the single public edge Postfix accept mail for ANY
// verified tenant domain (a stacks subdomain or a custom domain) WITHOUT
// a static domain list and without becoming an open relay: the chassis
// (which owns tenant_hostnames) is the single source of truth, and the
// answer auto-updates as tenants add/verify domains. Postfix wires it as
//
//	relay_domains = tcp:<this head>:<port>
//
// Protocol (Postfix tcp_table(5)): the client sends `get <key>\n` (the
// key %-encoded); we reply:
//   - `200 OK\n`     → key found  → domain is accepted
//   - `500 <text>\n` → not found  → reject_unauth_destination rejects it
//   - `400 <text>\n` → temp error → Postfix treats the table as
//     unavailable and DEFERs (4xx) — never an open relay, never lost mail
//
// If the head is down, Postfix can't connect → table unavailable → defer.
// We NEVER reply 200 on error/unknown (the open-relay guard): every
// accept is backed by a real verified tenant_hostnames row.
//
// OFF by default. Both gates must be flipped:
//   - `mailmap` must appear in `--personalities`
//   - `--mailmap-listen-addrs` must be non-empty
//
// The responder is unauthenticated; bind it only where the co-located
// Postfix can reach it (the compose network) — never a public interface.
package mailmap

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// MailMapController owns the tcp_table listeners and serves the
// relay_domains acceptance lookup.
type MailMapController struct {
	ctx      context.Context
	pu       *processor.Unit
	resolver ingress.MailResolver
	// accepter is `resolver` narrowed to its domain-accept facet,
	// resolved once at Start. nil → fall back to a ResolveRecipient probe.
	accepter    ingress.MailDomainAccepter
	readTimeout time.Duration

	mu        sync.Mutex
	listeners []net.Listener
	conns     map[net.Conn]struct{}
	closing   bool
	wg        sync.WaitGroup
}

// NewController constructs (but does not start) a mailmap controller.
// Mirrors the other personalities' constructor shape so server.go can
// treat them uniformly. `resolver` is the same MailResolver the LMTP
// head uses, so an accept decision can never drift from a route decision.
func NewController(ctx context.Context, pu *processor.Unit, resolver ingress.MailResolver) *MailMapController {
	return &MailMapController{
		ctx:      ctx,
		pu:       pu,
		resolver: resolver,
		conns:    make(map[net.Conn]struct{}),
	}
}

// boundAddrs returns the actual addresses each listener is bound to.
// Exposed for tests that bind ":0" and need the assigned port.
func (c *MailMapController) boundAddrs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	addrs := make([]string, 0, len(c.listeners))
	for _, ln := range c.listeners {
		if ln != nil {
			addrs = append(addrs, ln.Addr().String())
		}
	}
	return addrs
}

// Start binds the configured listeners and serves the tcp_table protocol
// on each. The double-gate (personality string AND non-empty listen
// addrs) keeps the head off by default.
func (c *MailMapController) Start() {
	if !strings.Contains(c.pu.Conf.Personalities, "mailmap") {
		return
	}
	addrs := nonEmpty(c.pu.Conf.MailMapListenAddrs)
	if len(addrs) == 0 {
		c.pu.Logger.Info("mailmap personality enabled but no listen addrs; head not started")
		return
	}

	rt, err := time.ParseDuration(c.pu.Conf.MailMapReadTimeout)
	if err != nil || rt <= 0 {
		c.pu.Logger.Warn("invalid mailmap-read-timeout, using 5s",
			zap.String("value", c.pu.Conf.MailMapReadTimeout))
		rt = 5 * time.Second
	}
	c.readTimeout = rt

	// Narrow the resolver to its domain-accept facet. The bundled
	// DBResolver/yamlResolver implement it; an overlay resolver that
	// doesn't falls back to a per-rcpt probe (logged once).
	if a, ok := c.resolver.(ingress.MailDomainAccepter); ok {
		c.accepter = a
	} else {
		c.pu.Logger.Warn("mailmap: resolver does not implement MailDomainAccepter; falling back to ResolveRecipient probe (a configured listener catch-all would over-accept)")
	}

	for _, addr := range addrs {
		network, bind := parseListenAddr(addr)

		// Pre-bind BEFORE logging "started" so a socket conflict surfaces
		// with an actionable error (matches the lmtp/tcp pre-bind discipline).
		listener, err := net.Listen(network, bind)
		if err != nil {
			hint := "lsof -iTCP" + bind + " -sTCP:LISTEN"
			if network == "unix" {
				hint = "lsof -U | grep " + bind
			}
			c.pu.Logger.Fatal("mailmap socket already in use (or otherwise unbindable)",
				zap.String("addr", addr),
				zap.String("network", network),
				zap.String("bind", bind),
				zap.String("err", err.Error()),
				zap.String("hint", hint))
		}

		c.mu.Lock()
		c.listeners = append(c.listeners, listener)
		c.mu.Unlock()

		c.pu.Logger.Info("mailmap controller started",
			zap.String("addr", addr),
			zap.String("network", network),
			zap.String("bind", bind))

		c.wg.Add(1)
		go c.acceptLoop(listener)
	}
}

func (c *MailMapController) acceptLoop(ln net.Listener) {
	defer c.wg.Done()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed on Stop
		}
		c.mu.Lock()
		if c.closing {
			c.mu.Unlock()
			_ = conn.Close()
			return
		}
		c.conns[conn] = struct{}{}
		c.mu.Unlock()

		c.wg.Add(1)
		go c.serveConn(conn)
	}
}

// serveConn reads tcp_table requests off a connection until EOF/idle.
// Postfix reuses the connection and pipelines `get` lines, so we loop.
func (c *MailMapController) serveConn(conn net.Conn) {
	defer c.wg.Done()
	defer func() {
		c.mu.Lock()
		delete(c.conns, conn)
		c.mu.Unlock()
		_ = conn.Close()
		if rec := recover(); rec != nil {
			c.pu.Logger.Error("mailmap serveConn panic", zap.Any("recover", rec))
		}
	}()

	reader := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(c.readTimeout))
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		resp := c.handle(strings.TrimRight(line, "\r\n"))
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
	}
}

// handle parses one tcp_table request line and returns the reply
// (including the trailing newline).
func (c *MailMapController) handle(line string) string {
	const verb = "get "
	if !strings.HasPrefix(line, verb) {
		// tcp_table also defines put/etc; a relay_domains lookup only gets.
		return "400 only get is supported\n"
	}
	rawKey := strings.TrimSpace(line[len(verb):])
	key, err := url.PathUnescape(rawKey)
	if err != nil {
		key = rawKey // best-effort; a domain rarely needs %-decoding
	}
	domain := strings.ToLower(strings.TrimSpace(key))
	if domain == "" {
		return "500 empty key\n"
	}

	ok, lerr := c.accept(domain)
	switch {
	case lerr != nil:
		c.pu.Logger.Error("mailmap lookup error",
			zap.String("domain", domain), zap.Error(lerr))
		return "400 lookup error\n" // temp failure → Postfix DEFERs (4xx)
	case ok:
		if c.pu.Logger.Core().Enabled(zap.DebugLevel) {
			c.pu.Logger.Debug("mailmap accept", zap.String("domain", domain))
		}
		return "200 OK\n"
	default:
		if c.pu.Logger.Core().Enabled(zap.DebugLevel) {
			c.pu.Logger.Debug("mailmap reject", zap.String("domain", domain))
		}
		return "500 not a hosted mail domain\n"
	}
}

// accept resolves the domain-acceptance decision, recovering any panic
// into an error (→ 400 → defer) rather than a false (→ 500 → reject): a
// transient fault must never masquerade as "not hosted".
func (c *MailMapController) accept(domain string) (ok bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			ok = false
			err = fmt.Errorf("panic: %v", rec)
		}
	}()
	if c.accepter != nil {
		_, hit := c.accepter.AcceptMailDomain(domain)
		return hit, nil
	}
	if c.resolver != nil {
		_, hit := c.resolver.ResolveRecipient("probe@"+domain, "default")
		return hit, nil
	}
	return false, nil
}

// Stop closes the listeners and any in-flight connections, then waits for
// the accept loops and handlers to drain.
func (c *MailMapController) Stop() {
	if !strings.Contains(c.pu.Conf.Personalities, "mailmap") {
		return
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	lns := c.listeners
	conns := make([]net.Conn, 0, len(c.conns))
	for conn := range c.conns {
		conns = append(conns, conn)
	}
	c.mu.Unlock()

	if len(lns) == 0 {
		return
	}
	c.pu.Logger.Info("calling mailmap controller stop")
	for _, ln := range lns {
		_ = ln.Close()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	c.wg.Wait()
	c.pu.Logger.Info("mailmap controller stopped")
}

// nonEmpty drops blank entries (viper CSV-parsing edge cases).
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseListenAddr splits a config value into (network, address) for
// net.Listen. Accepts `unix:/path`, `tcp:host:port`, or a bare
// `:port` / `host:port` (defaulting to tcp). Mirrors the lmtp head.
func parseListenAddr(addr string) (network, bind string) {
	if strings.HasPrefix(addr, "unix:") {
		return "unix", strings.TrimPrefix(addr, "unix:")
	}
	if strings.HasPrefix(addr, "tcp:") {
		return "tcp", strings.TrimPrefix(addr, "tcp:")
	}
	return "tcp", addr
}
