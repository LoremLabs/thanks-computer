// Package websocket implements the chassis's `websocket` personality: the
// long-lived side of an HTTP Upgrade.
//
// The web head owns the way in. An upgrade request runs through the stack's
// ordinary web ops like any other request — path, cookies, session lookup,
// Origin, whatever the stack decides — and a rule that wants the connection
// EXECs `txco://websocket/accept`. That op records the decision here, keyed
// by a session id the web head minted before the run. When the run ends the
// web head asks this controller whether an accept was recorded; if so it
// performs the RFC 6455 handshake and hands the socket over, and the
// request handler returns.
//
// From then on the connection is a chassis-owned session:
//
//   - one reader goroutine turns frames into complete messages and queues
//     them (bounded);
//   - one worker goroutine takes them in order and makes ONE bounded
//     processor run per message into `<stack>/_websocket/0` — the stack
//     never holds the socket, and an idle connection runs nothing;
//   - one pinger goroutine keeps the connection alive through proxies and
//     closes it when it goes idle;
//   - `txco://websocket/{send,reply,close}` write back through the registry
//     this controller keeps.
//
// Deliberate v1 cuts, all labelled in docs/advanced/protocols/websocket.md:
// no outbound queue (a send is one bounded write; a peer that will not read
// is closed at --websocket-write-timeout), sends resolve on this node only
// (the registry seam is where a fleet directory plugs in), binary rides the
// envelope as base64 under the same size cap as text.
//
// The personality is OFF by default: `websocket` must be in --personalities,
// and it only does anything alongside `web`.
package websocket

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/processor"
)

const (
	// Source is the `_txc.src` every session run carries.
	Source = "websocket"
	// SubStack is the sub-stack a session's runs enter: <stack>/_websocket/0.
	SubStack = "_websocket"
	// StateMaxBytes caps the accept-time `state` object a stack attaches to a
	// session (stamped verbatim on every message envelope).
	StateMaxBytes = 4096

	// acceptTTL bounds how long a recorded accept waits to be claimed by the
	// web head. The claim happens as soon as the upgrade run finishes, so
	// anything older is an orphan (a run that broke at a breakpoint, an op
	// that ran outside an upgrade request).
	acceptTTL = 30 * time.Second
	// maxPending bounds the pending-accept map: an accept op can only be
	// reached through a tenant's own stack, but a runaway rule must not be
	// able to grow chassis memory without bound.
	maxPending    = 4096
	sweepInterval = 5 * time.Second

	// Event names a stack may opt into at accept time.
	EventClose = "close"
)

// limits are the parsed --websocket-* knobs.
type limits struct {
	maxConns          int
	maxConnsPerTenant int
	maxMessageBytes   int64
	inboundQueue      int
	runTimeout        time.Duration
	idleTimeout       time.Duration
	maxIdleTimeout    time.Duration
	pingInterval      time.Duration
	writeTimeout      time.Duration
	drainTimeout      time.Duration
}

// Controller is the session registry and lifecycle owner. It satisfies the
// server's Start()/Stop() controller contract and the Registry interface the
// txco://websocket/* ops depend on.
type Controller struct {
	ctx     context.Context
	pu      *processor.Unit
	enabled bool
	lim     limits

	mu        sync.Mutex
	sessions  map[string]*session
	perTenant map[string]int
	pending   map[string]pendingAccept

	wg       sync.WaitGroup
	stopping atomic.Bool
	sweepCtx context.Context
	sweepEnd context.CancelFunc

	upgrades metric.Int64Counter
	messages metric.Int64Counter
	closes   metric.Int64Counter
	conns    metric.Int64UpDownCounter

	now func() time.Time
}

// NewController constructs (but does not start) the websocket controller.
// Inert unless both `websocket` and `web` are in --personalities: the
// controller then never records an accept, so the web head never upgrades.
func NewController(ctx context.Context, pu *processor.Unit) *Controller {
	c := &Controller{
		ctx:       ctx,
		pu:        pu,
		sessions:  make(map[string]*session),
		perTenant: make(map[string]int),
		pending:   make(map[string]pendingAccept),
		now:       time.Now,
	}
	if pu == nil {
		return c
	}
	c.enabled = pu.Conf.HasPersonality("websocket") && pu.Conf.HasPersonality("web")
	c.lim = parseLimits(pu)
	if pu.Mc != nil && pu.Mc.Meter != nil {
		c.upgrades, _ = pu.Mc.Meter.Int64Counter("chassis.websocket.upgrades",
			metric.WithDescription("WebSocket upgrade attempts by outcome"),
			metric.WithUnit("1"))
		c.messages, _ = pu.Mc.Meter.Int64Counter("chassis.websocket.messages",
			metric.WithDescription("WebSocket messages by direction and outcome"),
			metric.WithUnit("1"))
		c.closes, _ = pu.Mc.Meter.Int64Counter("chassis.websocket.closes",
			metric.WithDescription("WebSocket session closes by initiator"),
			metric.WithUnit("1"))
		c.conns, _ = pu.Mc.Meter.Int64UpDownCounter("chassis.websocket.connections",
			metric.WithDescription("Open WebSocket sessions on this node"),
			metric.WithUnit("1"))
	}
	return c
}

// Enabled reports whether this node accepts websocket sessions.
func (c *Controller) Enabled() bool { return c != nil && c.enabled }

// Start begins the pending-accept sweeper. There is no listener to bind:
// sessions arrive through the web head's Upgrade handoff.
func (c *Controller) Start() {
	if !c.Enabled() {
		return
	}
	c.sweepCtx, c.sweepEnd = context.WithCancel(c.ctx)
	c.wg.Add(1)
	go c.sweeper(c.sweepCtx)
	c.pu.Logger.Info("websocket controller started",
		zap.Int("max_conns", c.lim.maxConns),
		zap.Int("max_conns_per_tenant", c.lim.maxConnsPerTenant),
		zap.Int64("max_message_bytes", c.lim.maxMessageBytes),
		zap.Duration("idle_timeout", c.lim.idleTimeout),
		zap.Duration("ping_interval", c.lim.pingInterval))
}

// Stop closes every session with 1001 (going away) and waits, bounded by
// --websocket-drain-timeout. http.Server.Shutdown never waits for hijacked
// connections, so this is the only drain a websocket session gets.
func (c *Controller) Stop() {
	if !c.Enabled() {
		return
	}
	c.stopping.Store(true)
	if c.sweepEnd != nil {
		c.sweepEnd()
	}
	c.mu.Lock()
	live := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		live = append(live, s)
	}
	c.mu.Unlock()
	c.pu.Logger.Info("calling websocket controller stop", zap.Int("sessions", len(live)))

	var closers sync.WaitGroup
	for _, s := range live {
		closers.Add(1)
		go func(s *session) {
			defer closers.Done()
			s.closeWith(1001, "server shutting down", initiatorChassis)
		}(s)
	}
	done := make(chan struct{})
	go func() {
		closers.Wait()
		c.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		c.pu.Logger.Info("websocket controller stopped")
	case <-time.After(c.lim.drainTimeout):
		c.mu.Lock()
		left := len(c.sessions)
		c.mu.Unlock()
		c.pu.Logger.Warn("websocket drain timeout; abandoning stragglers",
			zap.Duration("drain_timeout", c.lim.drainTimeout), zap.Int("sessions", left))
	}
}

// sweeper expires orphaned accepts.
func (c *Controller) sweeper(ctx context.Context) {
	defer c.wg.Done()
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sweepPending(c.now())
		}
	}
}

// reserve takes a connection slot for the tenant; false when either cap is
// hit. 0 = unlimited (the imap connCounter convention).
func (c *Controller) reserve(tenant string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lim.maxConns > 0 && len(c.sessions) >= c.lim.maxConns {
		return false
	}
	if c.lim.maxConnsPerTenant > 0 && c.perTenant[tenant] >= c.lim.maxConnsPerTenant {
		return false
	}
	c.perTenant[tenant]++
	return true
}

func (c *Controller) releaseSlot(tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.perTenant[tenant] <= 1 {
		delete(c.perTenant, tenant)
		return
	}
	c.perTenant[tenant]--
}

func (c *Controller) register(s *session) {
	c.mu.Lock()
	c.sessions[s.id] = s
	c.mu.Unlock()
	if c.conns != nil {
		c.conns.Add(context.Background(), 1)
	}
}

func (c *Controller) unregister(s *session) {
	c.mu.Lock()
	_, present := c.sessions[s.id]
	delete(c.sessions, s.id)
	c.mu.Unlock()
	if !present {
		return
	}
	c.releaseSlot(s.tenant)
	if c.conns != nil {
		c.conns.Add(context.Background(), -1)
	}
}

// lookup finds a live session of the tenant. A session of ANOTHER tenant is
// indistinguishable from a missing one — no existence oracle across tenants.
func (c *Controller) lookup(tenant, id string) (*session, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sessions[id]
	if !ok || s.tenant != tenant {
		return nil, false
	}
	return s, true
}

// Count returns the number of open sessions on this node (tests, status).
func (c *Controller) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sessions)
}

func (c *Controller) record(ctr metric.Int64Counter, kv ...attribute.KeyValue) {
	if c == nil || ctr == nil {
		return
	}
	ctr.Add(context.Background(), 1, metric.WithAttributes(kv...))
}

func outcome(v string) attribute.KeyValue   { return attribute.String("txco.websocket.outcome", v) }
func direction(v string) attribute.KeyValue { return attribute.String("txco.websocket.direction", v) }
func initiator(v string) attribute.KeyValue { return attribute.String("txco.websocket.initiator", v) }

// parseLimits reads the --websocket-* config; a malformed duration warns
// and falls back to its documented default (the lmtp head's idiom).
func parseLimits(pu *processor.Unit) limits {
	conf := pu.Conf
	dur := func(name, v string, def time.Duration) time.Duration {
		v = strings.TrimSpace(v)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			if pu.Logger != nil {
				pu.Logger.Warn("invalid "+name+", using default",
					zap.String("value", v), zap.Duration("default", def))
			}
			return def
		}
		return d
	}
	l := limits{
		maxConns:          conf.WebsocketMaxConns,
		maxConnsPerTenant: conf.WebsocketMaxConnsPerTenant,
		maxMessageBytes:   int64(conf.WebsocketMaxMessageBytes),
		inboundQueue:      conf.WebsocketInboundQueue,
		runTimeout:        dur("websocket-run-timeout", conf.WebsocketRunTimeout, 60*time.Second),
		idleTimeout:       dur("websocket-idle-timeout", conf.WebsocketIdleTimeout, 5*time.Minute),
		maxIdleTimeout:    dur("websocket-max-idle-timeout", conf.WebsocketMaxIdleTimeout, time.Hour),
		pingInterval:      dur("websocket-ping-interval", conf.WebsocketPingInterval, 25*time.Second),
		writeTimeout:      dur("websocket-write-timeout", conf.WebsocketWriteTimeout, 10*time.Second),
		drainTimeout:      dur("websocket-drain-timeout", conf.WebsocketDrainTimeout, 5*time.Second),
	}
	if l.maxMessageBytes <= 0 {
		l.maxMessageBytes = 262144
	}
	if l.inboundQueue <= 0 {
		l.inboundQueue = 16
	}
	if l.idleTimeout > l.maxIdleTimeout {
		l.maxIdleTimeout = l.idleTimeout
	}
	return l
}
