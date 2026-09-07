package dns

import (
	"context"
	"database/sql"
	"fmt"
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
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/kv/redisstore"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// DNSController owns the authoritative-DNS listeners and the prebuilt
// zone snapshot they answer from.
//
// One controller hosts a listener per configured address and transport:
// a bare `host:port` entry binds UDP and TCP, a `udp:`/`tcp:`-prefixed one
// binds that transport only (parseListenSpec). The snapshot is rebuilt on
// every dbcache reload (config-apply, fs-watch) and swapped atomically, so
// the query hot path does zero DB work and never blocks a reload.
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
	// ZoneSnapshot / dbcache reload cycle. In-process by default; the
	// shared KV backend when --dns-challenge-store=kv (challenge_kv.go).
	// Final once NewController returns — see selectChallengeStore.
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
	// In-process challenge store unless --dns-challenge-store says
	// otherwise. Chosen HERE, not by a later setter: see selectChallengeStore.
	c.challenges = newMemChallengeStore()
	if pu != nil {
		c.selectChallengeStore()
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

// selectChallengeStore installs the backend --dns-challenge-store names.
//
// It runs inside NewController, not as a later wiring step, because
// server.go hands ChallengeStore() — an interface VALUE — to the bundled
// ACME solver right after the constructor returns. A store installed any
// later would leave the solver writing into the in-memory map while the
// head reads the shared one: no error, no log, just a certificate that
// never issues. Selecting in the constructor removes that hazard by
// construction.
//
// The boot guard in server.go runs the same ChallengeBackend selector and
// fails the boot on a misconfiguration, so the fallbacks below (logged at
// Error, never silent) are reachable only from direct constructors —
// tests and embedders.
func (c *DNSController) selectChallengeStore() {
	log := c.pu.Logger
	if log == nil {
		log = zap.NewNop()
	}
	backend, err := ChallengeBackend(c.pu.Conf.DNSChallengeStore, c.pu.Conf.KVStore == redisstore.StoreName)
	if err != nil {
		log.Error("dns challenge store: invalid selection; keeping the in-process store", zap.Error(err))
		return
	}
	if backend != ChallengeBackendKV {
		return
	}
	if c.pu.Kv == nil {
		// kvstore.New(nil, …) would degrade every read to "no challenge"
		// silently; refuse instead.
		log.Error("dns challenge store: kv selected but this node opened no KV store; keeping the in-process store")
		return
	}
	// A fresh UNCLAMPED handle, as the websocket directory does: --kv-max-ttl
	// guards authors' keys and must not shorten the challenge safety expiry.
	c.challenges = newKVChallengeStore(c.ctx, kvstore.New(c.pu.Kv, 0, 0), log)
	log.Info("dns challenge store: shared kv",
		zap.String("namespace", ChallengeNamespace),
		zap.String("effect", "every head serves a challenge written on any head; certificate issuance is coordinated by cert storage, not by this"))
}

// SetChallengeStore replaces the challenge store. Boot-goroutine only,
// BEFORE Start() and before anything captures ChallengeStore() (server.go
// hands it to the bundled ACME solver) — a later swap splits writers from
// readers. Completes the seam the other personalities' setters follow
// (SetRelay, SetFileCAS, …); the in-tree backends need no call to it.
func (c *DNSController) SetChallengeStore(s ChallengeStore) {
	if s != nil {
		c.challenges = s
	}
}

// Start binds the configured listeners and serves authoritative DNS from
// the zone snapshot. The double-gate (personality string AND non-empty
// listen addrs) means an upgrade can't silently acquire a privileged
// listener.
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

	seen := map[string]string{} // "<net>|<addr>" → the entry that first bound it
	for _, entry := range addrs {
		spec, err := parseListenSpec(entry)
		if err != nil {
			c.pu.Logger.Fatal("dns listen address invalid",
				zap.String("entry", entry), zap.String("err", err.Error()),
				zap.String("hint", "want host:port, udp:host:port or tcp:host:port"))
		}
		// A repeated (transport, address) is the fat-finger this config
		// invites (`udp:…,udp:…`); say so, rather than a raw EADDRINUSE.
		for _, n := range spec.nets() {
			if prev, dup := seen[n+"|"+spec.addr]; dup {
				c.pu.Logger.Fatal("dns listen address repeated",
					zap.String("entry", entry), zap.String("net", n),
					zap.String("bind", spec.addr), zap.String("first", prev))
			}
			seen[n+"|"+spec.addr] = entry
		}

		// Pre-bind BEFORE logging "started" so a port conflict surfaces
		// with a clear error rather than something resembling "ready",
		// matching tcp/lmtp pre-bind discipline. :53 needs privileges
		// (CAP_NET_BIND_SERVICE / front-LB); dev uses a high port.
		var pc net.PacketConn
		var ln net.Listener
		if spec.udp {
			pc, err = net.ListenPacket("udp", spec.addr)
			if err != nil {
				c.pu.Logger.Fatal("dns udp socket unbindable",
					zap.String("bind", spec.addr), zap.String("err", err.Error()),
					zap.String("hint", "lsof -iUDP"+spec.addr))
			}
		}
		if spec.tcp {
			ln, err = net.Listen("tcp", spec.addr)
			if err != nil {
				if pc != nil {
					_ = pc.Close()
				}
				c.pu.Logger.Fatal("dns tcp socket unbindable",
					zap.String("bind", spec.addr), zap.String("err", err.Error()),
					zap.String("hint", "lsof -iTCP"+spec.addr+" -sTCP:LISTEN"))
			}
		}

		var started []*dns.Server
		if pc != nil {
			started = append(started, c.newServer(pc, nil))
		}
		if ln != nil {
			started = append(started, c.newServer(nil, ln))
		}
		if !spec.udp || !spec.tcp {
			// DNS needs both transports (RFC 7766): a truncated UDP answer
			// must be retryable over TCP — CA validators follow TC — and a
			// TSIG-signed UPDATE readily exceeds 512 bytes. Warn, not Fatal:
			// a front LB may legitimately terminate one transport and hand
			// the other to a different bind.
			c.pu.Logger.Warn("dns listener bound on one transport only; DNS requires both UDP and TCP — make sure the other transport reaches this head too",
				zap.String("bind", spec.addr), zap.Strings("nets", spec.nets()))
		}
		c.servers = append(c.servers, started...)
		c.pu.Logger.Info("dns controller started", zap.String("bind", spec.addr), zap.Strings("nets", spec.nets()))

		for _, srv := range started {
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

// newServer builds the miekg server for one bound transport (exactly one
// of pc/ln set). Per-server construction lives here so the TSIG secret and
// the UPDATE-accepting MsgAcceptFunc are applied to whichever transports
// exist and the two cannot drift.
func (c *DNSController) newServer(pc net.PacketConn, ln net.Listener) *dns.Server {
	srv := &dns.Server{}
	if pc != nil {
		srv.PacketConn, srv.Net, srv.Handler = pc, "udp", c.makeHandler(true)
	} else {
		srv.Listener, srv.Net, srv.Handler = ln, "tcp", c.makeHandler(false)
	}
	// TSIG secret for the RFC2136 UPDATE receiver (update.go), so the
	// server verifies inbound MACs and can sign replies; absent key ⇒ the
	// receiver refuses every UPDATE. The default accept func NOTIMPs
	// OpcodeUpdate; swap it so UPDATEs reach the handler (queries
	// unaffected).
	if c.updatesEnabled() {
		srv.TsigSecret = map[string]string{c.tsigKeyName: c.tsigSecret}
		srv.MsgAcceptFunc = acceptDynamicUpdate
	}
	return srv
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

// listenSpec is one parsed --dns-listen-addrs entry.
type listenSpec struct {
	addr string // host:port for net.Listen / net.ListenPacket
	udp  bool
	tcp  bool
}

// nets lists the transports the spec binds, in bind order.
func (l listenSpec) nets() []string {
	var out []string
	if l.udp {
		out = append(out, "udp")
	}
	if l.tcp {
		out = append(out, "tcp")
	}
	return out
}

// parseListenSpec parses a listen entry. A bare `host:port` (or `:port`)
// binds both transports — the default and the historical behaviour. An
// exact `udp:` or `tcp:` head (split on the FIRST colon) binds that
// transport only, for a front that delivers the two on different
// addresses: Fly, for one, requires UDP on the `fly-global-services`
// address and TCP on 0.0.0.0. Anything else before the first colon is a
// hostname (`example.com:53`), which is also why an unknown prefix cannot
// be rejected here: `upd:53` is host `upd`, and fails at bind. The
// remainder must be a valid host:port — `udp:` alone is an error, not a
// random port on every interface.
func parseListenSpec(entry string) (listenSpec, error) {
	raw := strings.TrimSpace(entry)
	spec := listenSpec{addr: raw, udp: true, tcp: true}
	if i := strings.Index(raw, ":"); i >= 0 {
		switch raw[:i] {
		case "udp":
			spec.addr, spec.tcp = raw[i+1:], false
		case "tcp":
			spec.addr, spec.udp = raw[i+1:], false
		}
	}
	if _, _, err := net.SplitHostPort(spec.addr); err != nil {
		return listenSpec{}, fmt.Errorf("dns listen entry %q: %w", entry, err)
	}
	return spec, nil
}
