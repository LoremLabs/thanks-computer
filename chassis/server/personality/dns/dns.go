package dns

import (
	"context"
	"database/sql"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// DNSController owns the authoritative-DNS listeners and the prebuilt
// zone snapshot they answer from.
//
// One controller hosts a UDP and a TCP listener per configured address.
// The snapshot is rebuilt on every dbcache reload (config-apply,
// fs-watch) and swapped atomically, so the query hot path does zero DB
// work and never blocks a reload.
//
// DNS is OFF by default. Both gates must be flipped:
//   - `dns` must appear in `--personalities`
//   - `--dns-listen-addrs` must be non-empty
type DNSController struct {
	ctx      context.Context
	pu       *processor.Unit
	synthCfg SynthConfig
	servers  []*dns.Server
	snap     atomic.Pointer[ZoneSnapshot]
	rrl      *throttle.Throttle
	wg       sync.WaitGroup

	// challenges holds transient `_acme-challenge` TXT records served
	// during ACME DNS-01 issuance. Written by the in-process solver
	// (chassis/tls) and/or the RFC2136 UPDATE receiver; read on the query
	// path for `_acme-challenge.*` names only. Never goes through the
	// ZoneSnapshot / dbcache reload cycle. See challenge.go.
	challenges ChallengeStore

	// tsigKeyName/tsigSecret gate the RFC2136 UPDATE receiver (update.go).
	// Both empty ⇒ the UPDATE path is off and every UPDATE is refused.
	// tsigKeyName is the canonical (trailing-dot) key name; tsigSecret is
	// the base64 shared secret.
	tsigKeyName string
	tsigSecret  string

	// tap is the observe lane (observe.go): answered queries in a zone
	// whose tenant has an active `_dns` stack are dispatched into that
	// stack AFTER the wire reply, fire-and-forget. Nil when
	// --dns-observe-sample=0.
	tap *observeTap

	// lane is the stack-answered lane (answer.go): zones with
	// answer_mode=stack dispatch cache-missed queries to `_dns`
	// synchronously and put `@dns.res` on the wire. Always constructed
	// with a pu; inert until a snapshot has an answering zone.
	lane *answerLane

	queries  metric.Int64Counter
	rrlDrops metric.Int64Counter
}

// NewController constructs (but does not start) a DNS controller.
// Mirrors the other personalities' constructor shape so server.go can
// treat them uniformly.
func NewController(ctx context.Context, pu *processor.Unit) *DNSController {
	c := &DNSController{ctx: ctx, pu: pu}
	// Single-node in-memory challenge store by default. A fleet selects a
	// shared backend by DSN (overlay-registered); that wiring lands with
	// the cert-storage config.
	c.challenges = newMemChallengeStore()
	if pu != nil {
		c.synthCfg = SynthConfigFrom(pu.Conf)
		if kn := strings.TrimSpace(pu.Conf.DNSUpdateTSIGKeyName); kn != "" && strings.TrimSpace(pu.Conf.DNSUpdateTSIGSecret) != "" {
			c.tsigKeyName = dns.Fqdn(kn)
			c.tsigSecret = strings.TrimSpace(pu.Conf.DNSUpdateTSIGSecret)
		}
		c.tap = newObserveTap(pu, 0)
		node := ""
		if c.tap != nil {
			node = c.tap.node
		} else {
			node, _ = os.Hostname()
		}
		c.lane = newAnswerLane(ctx, pu, node)
	}
	if pu != nil && pu.Mc != nil && pu.Mc.Meter != nil {
		c.queries, _ = pu.Mc.Meter.Int64Counter("chassis.dns.queries",
			metric.WithDescription("DNS queries answered, by qtype + rcode"),
			metric.WithUnit("1"))
		c.rrlDrops, _ = pu.Mc.Meter.Int64Counter("chassis.dns.rrl_drops",
			metric.WithDescription("DNS queries dropped by response-rate-limiting"),
			metric.WithUnit("1"))
	}
	return c
}

// Start binds UDP+TCP listeners on each configured address and serves
// authoritative DNS from the zone snapshot. The double-gate
// (personality string AND non-empty listen addrs) means an upgrade
// can't silently acquire a privileged listener.
func (c *DNSController) Start() {
	if !strings.Contains(c.pu.Conf.Personalities, "dns") {
		return
	}
	addrs := nonEmpty(c.pu.Conf.DNSListenAddrs)
	if len(addrs) == 0 {
		c.pu.Logger.Info("dns personality enabled but no listen addrs; head not started")
		return
	}

	c.installReload()

	// Per-source-IP response-rate-limiter (anti-amplification). 0 (the
	// default) disables it.
	c.rrl = throttle.New(c.pu.Conf.DNSRRLPerSec, time.Second)

	// Observe-tap workers (post-reply `_dns` dispatch). Started before the
	// listeners so the first answered query has somewhere to go.
	if c.tap != nil {
		c.tap.start(c.ctx)
	}

	for _, addr := range addrs {
		bind := bindAddr(addr)

		// Pre-bind BEFORE logging "started" so a port conflict surfaces
		// with a clear error rather than something resembling "ready",
		// matching tcp/lmtp pre-bind discipline. :53 needs privileges
		// (CAP_NET_BIND_SERVICE / front-LB); dev uses a high port.
		pc, err := net.ListenPacket("udp", bind)
		if err != nil {
			c.pu.Logger.Fatal("dns udp socket unbindable",
				zap.String("bind", bind), zap.String("err", err.Error()),
				zap.String("hint", "lsof -iUDP"+bind))
		}
		ln, err := net.Listen("tcp", bind)
		if err != nil {
			_ = pc.Close()
			c.pu.Logger.Fatal("dns tcp socket unbindable",
				zap.String("bind", bind), zap.String("err", err.Error()),
				zap.String("hint", "lsof -iTCP"+bind+" -sTCP:LISTEN"))
		}

		usrv := &dns.Server{PacketConn: pc, Net: "udp", Handler: c.makeHandler(true)}
		tsrv := &dns.Server{Listener: ln, Net: "tcp", Handler: c.makeHandler(false)}
		// TSIG secret for the RFC2136 UPDATE receiver (update.go). Set on
		// both transports so the server verifies inbound MACs and can sign
		// replies; absent key ⇒ the receiver refuses every UPDATE.
		if c.updatesEnabled() {
			secrets := map[string]string{c.tsigKeyName: c.tsigSecret}
			usrv.TsigSecret = secrets
			tsrv.TsigSecret = secrets
			// Default accept func NOTIMPs OpcodeUpdate; swap it so the
			// receiver's UPDATEs reach the handler (queries unaffected).
			usrv.MsgAcceptFunc = acceptDynamicUpdate
			tsrv.MsgAcceptFunc = acceptDynamicUpdate
		}
		c.servers = append(c.servers, usrv, tsrv)
		c.pu.Logger.Info("dns controller started", zap.String("bind", bind))

		for _, srv := range []*dns.Server{usrv, tsrv} {
			c.wg.Add(1)
			go func(s *dns.Server) {
				defer c.wg.Done()
				if err := s.ActivateAndServe(); err != nil && !strings.Contains(err.Error(), "closed") {
					c.pu.Logger.Error("dns serve error",
						zap.String("net", s.Net), zap.String("err", err.Error()))
				}
			}(srv)
		}
	}
}

// Stop drains in-flight queries and closes the listeners with a 5s
// ceiling so a wedged TCP session can't stall chassis shutdown.
func (c *DNSController) Stop() {
	if !strings.Contains(c.pu.Conf.Personalities, "dns") {
		return
	}
	if len(c.servers) == 0 {
		return
	}
	c.pu.Logger.Info("calling dns controller stop")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range c.servers {
		if err := s.ShutdownContext(ctx); err != nil {
			c.pu.Logger.Warn("dns shutdown error", zap.String("err", err.Error()))
		}
	}
	c.wg.Wait()
	// Listeners are down, so no handler can offer again; abandon whatever
	// is still queued (analytics, not deliveries) and let in-flight
	// dispatches unwind.
	if c.tap != nil {
		c.tap.stop()
	}
	c.pu.Logger.Info("dns controller stopped")
}

// rebuild reads the current mirror into a fresh snapshot and swaps it
// in. A build failure keeps the previous snapshot live (never go dark);
// the first failure ensures the pointer is at least a non-nil empty
// snapshot so the handler can serve REFUSED instead of SERVFAIL.
// installReload builds the initial zone snapshot and chains
// dbc.OnReload so a `txco apply` / hostname change / fs-watch / admin
// mutation rebuilds + swaps it with no restart (same chaining shape as
// the static-asset index).
//
// CRITICAL: the OnReload hook runs INSIDE Reload, handed the freshly-
// built mirror as `db` BEFORE it is published. It MUST rebuild from that
// `db`, never from dbc.Snapshot() — Snapshot() still returns the
// PREVIOUS mirror at that point, so a Snapshot()-based rebuild would
// silently pin stale zones every reload. The initial build runs outside
// Reload, so Snapshot() is correct there.
func (c *DNSController) installReload() {
	if c.pu.Dbc == nil {
		c.rebuild(nil)
		return
	}
	c.rebuild(c.pu.Dbc.Snapshot())
	prev := c.pu.Dbc.OnReload
	c.pu.Dbc.OnReload = func(db *sql.DB) error {
		var err error
		if prev != nil {
			err = prev(db)
		}
		c.rebuild(db)
		return err
	}
}

func (c *DNSController) rebuild(db *sql.DB) {
	if db == nil {
		if c.snap.Load() == nil {
			c.snap.Store(&ZoneSnapshot{})
		}
		return
	}
	snap, err := BuildSnapshot(db, c.synthCfg, c.pu.Logger)
	if err != nil {
		c.pu.Logger.Error("dns zone snapshot rebuild failed; keeping previous",
			zap.String("err", err.Error()))
		if c.snap.Load() == nil {
			c.snap.Store(&ZoneSnapshot{})
		}
		return
	}
	c.snap.Store(snap)
	// A reload is how a re-applied `_dns` stack or a flipped zone reaches
	// the head; cached stack answers from before it are stale by definition.
	c.lane.reset()
}

// ChallengeStore exposes the controller's transient ACME-challenge store
// so the in-process DNS-01 solver (chassis/tls) writes to the same
// instance this head serves from. Nil only before NewController runs.
func (c *DNSController) ChallengeStore() ChallengeStore { return c.challenges }

// Origins returns the canonical origins currently served, from the live
// snapshot (lock-free atomic read; safe to call from an OnReload hook). The
// bundled cert manager uses this to recompute the wildcard cert set when
// delegated zones change.
func (c *DNSController) Origins() []string {
	snap := c.snap.Load()
	if snap == nil {
		return nil
	}
	return snap.Origins()
}

// makeHandler returns the miekg/dns handler for one transport. isUDP
// drives EDNS0 size negotiation + truncation (TCP never truncates).
func (c *DNSController) makeHandler(isUDP bool) dns.HandlerFunc {
	return func(w dns.ResponseWriter, req *dns.Msg) {
		// RFC2136 dynamic UPDATE (update.go): TSIG-authenticated, scoped to
		// `_acme-challenge` TXT. Handled before RRL — it's authenticated and
		// low-volume, not the anonymous-query flood RRL defends against.
		if req.Opcode == dns.OpcodeUpdate {
			c.handleUpdate(w, req)
			return
		}

		// Response-rate-limit by source IP. On exhaustion we DROP rather
		// than reply — replying to a spoofed source is exactly the
		// reflection/amplification behaviour we must not exhibit.
		if c.rrl != nil {
			if ok, _ := c.rrl.Allow(clientIP(w.RemoteAddr())); !ok {
				if c.rrlDrops != nil {
					c.rrlDrops.Add(c.ctx, 1)
				}
				return
			}
		}

		// Transient ACME DNS-01 challenge takes precedence for the
		// `_acme-challenge.*` name only; everything else (incl. that name
		// with no active challenge) falls through to the snapshot.
		snap := c.snap.Load()
		m := c.answerChallenge(req, isUDP)
		// Stack-answered zone (answer.go): the tenant's `_dns` stack decides,
		// synchronously, with the snapshot answer as the proposal/fallback.
		// nil when this query isn't the lane's (not a stack zone, ANY, …).
		stackSaw := false
		if m == nil {
			m, stackSaw = c.lane.answer(snap, w, req, isUDP)
		}
		if m == nil {
			m = buildReply(snap, req, isUDP)
		}
		if len(req.Question) == 1 {
			c.recordQuery(req.Question[0], m.Rcode)
		}
		if err := w.WriteMsg(m); err != nil {
			c.pu.Logger.Debug("dns write reply failed", zap.String("err", err.Error()))
		}
		// Observe tap — strictly AFTER the wire write, so the reply path
		// never waits on the opstack. A failed write still observes: the
		// answer was decided, and the failure itself is signal. Skipped when
		// the stack itself just answered this query (it saw it once already);
		// cache hits and fallbacks are still tapped.
		if !stackSaw {
			c.observe(snap, w, req, m, isUDP)
		}
	}
}

// observe hands one answered query to the observe tap when (and only
// when) the tap is on, the snapshot has any observing zone, the query is
// a single QUERY question, and the name falls in a zone whose tenant has
// an active `_dns` stack. Everything else returns without allocating —
// the default deployment (no `_dns` stack anywhere) pays one bool per
// query.
func (c *DNSController) observe(snap *ZoneSnapshot, w dns.ResponseWriter, req, m *dns.Msg, isUDP bool) {
	if c.tap == nil || snap == nil || !snap.observing {
		return
	}
	if req.Opcode != dns.OpcodeQuery || len(req.Question) != 1 {
		return
	}
	q := req.Question[0]
	z := snap.zoneFor(strings.ToLower(dns.Fqdn(q.Name)))
	if z == nil || !z.observe {
		return
	}
	ob := observation{
		q:         q,
		reply:     m,
		clientIP:  clientIP(w.RemoteAddr()),
		transport: "tcp",
		zone:      z,
	}
	if isUDP {
		ob.transport = "udp"
	}
	if opt := req.IsEdns0(); opt != nil {
		ob.ednsSize = opt.UDPSize()
	}
	c.tap.offer(ob)
}

// buildReply turns a query into an authoritative response from the
// snapshot. Pure (no I/O, no rate-limiting) so it can be unit-tested
// directly. isUDP enables EDNS0 size negotiation + truncation; TCP
// never truncates.
func buildReply(snap *ZoneSnapshot, req *dns.Msg, isUDP bool) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = false // authoritative-only, never recursive

	switch {
	case req.Opcode != dns.OpcodeQuery:
		m.Rcode = dns.RcodeRefused
	case len(req.Question) != 1:
		// Authoritative servers answer exactly one question.
		m.Rcode = dns.RcodeRefused
	case snap == nil:
		m.Rcode = dns.RcodeServerFailure
	default:
		q := req.Question[0]
		ans, nsRR, rcode := snap.Lookup(q)
		m.Rcode = rcode
		m.Answer = ans
		m.Ns = nsRR
		m.Authoritative = rcode != dns.RcodeRefused
	}

	applyUDPSizing(m, req, isUDP)
	return m
}

// applyUDPSizing negotiates EDNS0 buffer size and truncates over UDP (TCP
// never truncates). Shared by buildReply and the challenge answer path so
// both honour the same size discipline.
func applyUDPSizing(m, req *dns.Msg, isUDP bool) {
	if !isUDP {
		return
	}
	size := dns.MinMsgSize // 512
	if opt := req.IsEdns0(); opt != nil {
		m.SetEdns0(opt.UDPSize(), false)
		if int(opt.UDPSize()) > size {
			size = int(opt.UDPSize())
		}
	}
	m.Truncate(size) // sets TC if the answer doesn't fit
}

// answerChallenge serves a transient ACME DNS-01 challenge, or returns nil
// to let the normal snapshot path handle the query. It answers ONLY a
// single TXT question for an `_acme-challenge.<served-zone>` owner that has
// a live value in the challenge store — so a missing challenge falls
// through to the snapshot's normal NXDOMAIN/NODATA, and a name outside any
// served zone still REFUSES. Authoritative, never recursive.
func (c *DNSController) answerChallenge(req *dns.Msg, isUDP bool) *dns.Msg {
	if c.challenges == nil || req.Opcode != dns.OpcodeQuery || len(req.Question) != 1 {
		return nil
	}
	q := req.Question[0]
	if q.Qtype != dns.TypeTXT {
		return nil
	}
	qname := strings.ToLower(dns.Fqdn(q.Name))
	if !isACMEChallengeName(qname) {
		return nil
	}
	// Only answer under a zone we actually serve (keeps authoritative-only
	// posture; a challenge for an unserved name is not ours to answer).
	if snap := c.snap.Load(); snap == nil || snap.zoneFor(qname) == nil {
		return nil
	}
	vals := c.challenges.ActiveTXT(qname)
	if len(vals) == 0 {
		return nil
	}
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = false
	m.Authoritative = true
	m.Rcode = dns.RcodeSuccess
	for _, v := range vals {
		m.Answer = append(m.Answer, &dns.TXT{
			Hdr: dns.RR_Header{Name: qname, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: challengeRecordTTL},
			Txt: chunkTXT(v),
		})
	}
	applyUDPSizing(m, req, isUDP)
	return m
}

// chunkTXT splits a TXT value into <=255-byte character-strings as the
// wire format requires. ACME key authorizations are 43 bytes so this is a
// single chunk in practice, but stay correct for longer values.
func chunkTXT(s string) []string {
	const max = 255
	if len(s) <= max {
		return []string{s}
	}
	var out []string
	for len(s) > max {
		out = append(out, s[:max])
		s = s[max:]
	}
	return append(out, s)
}

func (c *DNSController) recordQuery(q dns.Question, rcode int) {
	if c.queries == nil {
		return
	}
	c.queries.Add(c.ctx, 1, metric.WithAttributes(
		attribute.String("txco.dns.qtype", dns.TypeToString[q.Qtype]),
		attribute.String("txco.dns.rcode", dns.RcodeToString[rcode]),
	))
}

// clientIP extracts the host portion of a remote address for RRL
// keying.
func clientIP(a net.Addr) string {
	if a == nil {
		return ""
	}
	if h, _, err := net.SplitHostPort(a.String()); err == nil {
		return h
	}
	return a.String()
}

// nonEmpty drops blank entries (viper's []string parsing can yield a
// single "" element for an explicitly-empty flag).
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// bindAddr normalizes a listen entry to a host:port for net.Listen. DNS
// always serves both UDP and TCP on the same address, so an optional
// `udp:`/`tcp:` prefix is just stripped.
func bindAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	addr = strings.TrimPrefix(addr, "udp:")
	addr = strings.TrimPrefix(addr, "tcp:")
	return addr
}
