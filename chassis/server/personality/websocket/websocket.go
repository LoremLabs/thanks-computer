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
	"errors"
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

	upgrades  metric.Int64Counter
	messages  metric.Int64Counter
	closes    metric.Int64Counter
	directory metric.Int64Counter
	conns     metric.Int64UpDownCounter

	// relay + dir, set once at boot (SetRelay) before Start; nil = every
	// session is reachable on this node only. leaseTTL is the directory
	// lease a session holds while it lives; refreshed past half-life.
	relay    Relay
	dir      Directory
	leaseTTL time.Duration

	now func() time.Time
}

// SetRelay wires cross-node delivery: a Relay to reach other nodes' sessions
// and a Directory that records which node owns each session. Call before
// Start. With neither, a session is reachable only on the node holding its
// socket (the open-core default).
func (c *Controller) SetRelay(r Relay, d Directory) {
	c.relay, c.dir = r, d
	c.leaseTTL = 2 * c.lim.maxIdleTimeout
	if c.leaseTTL < time.Hour {
		c.leaseTTL = time.Hour
	}
}

// crossNode reports whether the controller can look beyond this node.
func (c *Controller) crossNode() bool { return c != nil && c.relay != nil && c.dir != nil }

// LeaseTTL is the directory lease length (tests).
func (c *Controller) LeaseTTL() time.Duration { return c.leaseTTL }

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
		c.directory, _ = pu.Mc.Meter.Int64Counter("chassis.websocket.directory",
			metric.WithDescription("Session directory operations by op and outcome"),
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
	node := ""
	if c.relay != nil {
		node = c.relay.Node()
	}
	c.pu.Logger.Info("websocket controller started",
		zap.Int("max_conns", c.lim.maxConns),
		zap.Int("max_conns_per_tenant", c.lim.maxConnsPerTenant),
		zap.Int64("max_message_bytes", c.lim.maxMessageBytes),
		zap.Duration("idle_timeout", c.lim.idleTimeout),
		zap.Duration("ping_interval", c.lim.pingInterval),
		zap.Bool("cross_node", c.crossNode()),
		zap.String("node", node),
		zap.Duration("lease_ttl", c.leaseTTL))
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
	deadline := time.Now().Add(c.lim.drainTimeout)
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
	// Stop answering for this node only after its sessions are gone, so a
	// request that races the drain gets a real answer; whatever is left in
	// the bus after this is a no-responder, which the sender reads as not
	// found.
	if c.relay != nil {
		sctx, cancel := context.WithDeadline(context.Background(), deadline.Add(2*time.Second))
		defer cancel()
		if err := c.relay.Shutdown(sctx); err != nil {
			c.pu.Logger.Warn("websocket relay shutdown", zap.String("err", err.Error()))
		}
	}
}

// --- cross-node ---------------------------------------------------------------

const directoryOpTimeout = 2 * time.Second

func dirOp(v string) attribute.KeyValue { return attribute.String("txco.websocket.op", v) }

// dirRegister records this node as the session's owner. Best-effort: on
// failure the session still serves — it is simply reachable only here until
// the sweeper's refresh succeeds.
func (c *Controller) dirRegister(s *session) {
	if !c.crossNode() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), directoryOpTimeout)
	defer cancel()
	if err := c.dir.Register(ctx, s.tenant, s.id, c.relay.Node(), s.stack); err != nil {
		c.record(c.directory, dirOp("register"), outcome("error"))
		c.pu.Logger.Warn("websocket directory register failed; session reachable on this node only until refreshed",
			zap.String("sid", s.id), zap.String("err", err.Error()))
		return
	}
	s.leasedAt.Store(c.now().UnixNano())
	c.record(c.directory, dirOp("register"), outcome("ok"))
}

func (c *Controller) dirUnregister(s *session) {
	if !c.crossNode() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), directoryOpTimeout)
	defer cancel()
	if err := c.dir.Unregister(ctx, s.tenant, s.id); err != nil {
		c.record(c.directory, dirOp("unregister"), outcome("error"))
		c.pu.Logger.Warn("websocket directory unregister failed; the lease expires on its own",
			zap.String("sid", s.id), zap.String("err", err.Error()))
		return
	}
	c.record(c.directory, dirOp("unregister"), outcome("ok"))
}

// refreshLeases renews the directory lease of every session past half-life
// (and re-registers one whose registration failed), a bounded batch per
// sweep so a node full of long sessions never stalls the sweeper.
func (c *Controller) refreshLeases(now time.Time) {
	if !c.crossNode() {
		return
	}
	const perTick = 256
	cutoff := now.Add(-c.leaseTTL / 2).UnixNano()
	c.mu.Lock()
	due := make([]*session, 0, 16)
	for _, s := range c.sessions {
		if s.leasedAt.Load() <= cutoff {
			due = append(due, s)
			if len(due) == perTick {
				break
			}
		}
	}
	c.mu.Unlock()
	for _, s := range due {
		if s.closed.Load() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), directoryOpTimeout)
		err := c.dir.Refresh(ctx, s.tenant, s.id, c.relay.Node(), s.stack)
		cancel()
		if err != nil {
			c.record(c.directory, dirOp("refresh"), outcome("error"))
			continue
		}
		s.leasedAt.Store(now.UnixNano())
		c.record(c.directory, dirOp("refresh"), outcome("ok"))
	}
}

// resolveRemote finds the node that owns a session this node does not.
// A miss, a directory error, or an entry naming this node (stale: the
// session is gone from here) each end the attempt with the right error.
func (c *Controller) resolveRemote(ctx context.Context, tenant, sid string) (string, error) {
	lctx, cancel := context.WithTimeout(ctx, directoryOpTimeout)
	defer cancel()
	node, found, err := c.dir.Lookup(lctx, tenant, sid)
	if err != nil {
		c.record(c.directory, dirOp("lookup"), outcome("error"))
		c.pu.Logger.Warn("websocket directory lookup failed", zap.String("sid", sid), zap.String("err", err.Error()))
		return "", ErrRelayUnavailable
	}
	if !found {
		c.record(c.directory, dirOp("lookup"), outcome("miss"))
		return "", ErrSessionNotFound
	}
	if node == c.relay.Node() {
		// Our own stale entry: the session closed here and the delete was
		// lost. Clean it up and answer as the local lookup already did.
		c.record(c.directory, dirOp("lookup"), outcome("stale_self"))
		c.dirForget(tenant, sid)
		return "", ErrSessionNotFound
	}
	c.record(c.directory, dirOp("lookup"), outcome("ok"))
	return node, nil
}

// dirForget drops a directory entry that proved stale (best effort).
func (c *Controller) dirForget(tenant, sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), directoryOpTimeout)
	defer cancel()
	_ = c.dir.Unregister(ctx, tenant, sid)
}

// remoteTimeout bounds one cross-node request: the owner's own write
// timeout plus the bus round trip.
func (c *Controller) remoteTimeout() time.Duration { return c.lim.writeTimeout + 2*time.Second }

// noteRemote maps a relay answer to the outbound message outcome and tidies
// the directory when the owner says the session is gone.
func (c *Controller) noteRemote(tenant, sid string, err error) {
	switch {
	case err == nil:
		c.record(c.messages, direction("out"), outcome("remote_ok"))
	case errors.Is(err, ErrSessionNotFound):
		c.record(c.messages, direction("out"), outcome("remote_miss"))
		c.dirForget(tenant, sid)
	case errors.Is(err, ErrWriteTimeout):
		c.record(c.messages, direction("out"), outcome("remote_timeout"))
	default:
		c.record(c.messages, direction("out"), outcome("remote_failed"))
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
			now := c.now()
			c.sweepPending(now)
			c.refreshLeases(now)
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
	c.dirRegister(s)
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
	c.dirUnregister(s)
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
