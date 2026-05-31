package dns

import (
	"context"
	"database/sql"
	"net"
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
	ctx     context.Context
	pu      *processor.Unit
	servers []*dns.Server
	snap    atomic.Pointer[ZoneSnapshot]
	rrl     *throttle.Throttle
	wg      sync.WaitGroup

	queries  metric.Int64Counter
	rrlDrops metric.Int64Counter
}

// NewController constructs (but does not start) a DNS controller.
// Mirrors the other personalities' constructor shape so server.go can
// treat them uniformly.
func NewController(ctx context.Context, pu *processor.Unit) *DNSController {
	c := &DNSController{ctx: ctx, pu: pu}
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

	// Build the initial snapshot, then chain dbcache OnReload so a
	// `txco apply` / hostname change / fs-watch rebuilds + swaps it with
	// no restart. Same chaining shape as the static-asset index. The
	// rebuild reads dbc.Snapshot() each time — never a captured handle.
	c.rebuild()
	if c.pu.Dbc != nil {
		prev := c.pu.Dbc.OnReload
		c.pu.Dbc.OnReload = func(db *sql.DB) error {
			var err error
			if prev != nil {
				err = prev(db)
			}
			c.rebuild()
			return err
		}
	}

	// Per-source-IP response-rate-limiter (anti-amplification). 0 (the
	// default) disables it.
	c.rrl = throttle.New(c.pu.Conf.DNSRRLPerSec, time.Second)

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
	c.pu.Logger.Info("dns controller stopped")
}

// rebuild reads the current mirror into a fresh snapshot and swaps it
// in. A build failure keeps the previous snapshot live (never go dark);
// the first failure ensures the pointer is at least a non-nil empty
// snapshot so the handler can serve REFUSED instead of SERVFAIL.
func (c *DNSController) rebuild() {
	if c.pu.Dbc == nil {
		if c.snap.Load() == nil {
			c.snap.Store(&ZoneSnapshot{})
		}
		return
	}
	snap, err := BuildSnapshot(c.pu.Dbc.Snapshot(), c.pu.Logger)
	if err != nil {
		c.pu.Logger.Error("dns zone snapshot rebuild failed; keeping previous",
			zap.String("err", err.Error()))
		if c.snap.Load() == nil {
			c.snap.Store(&ZoneSnapshot{})
		}
		return
	}
	c.snap.Store(snap)
}

// makeHandler returns the miekg/dns handler for one transport. isUDP
// drives EDNS0 size negotiation + truncation (TCP never truncates).
func (c *DNSController) makeHandler(isUDP bool) dns.HandlerFunc {
	return func(w dns.ResponseWriter, req *dns.Msg) {
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

		m := buildReply(c.snap.Load(), req, isUDP)
		if len(req.Question) == 1 {
			c.recordQuery(req.Question[0], m.Rcode)
		}
		if err := w.WriteMsg(m); err != nil {
			c.pu.Logger.Debug("dns write reply failed", zap.String("err", err.Error()))
		}
	}
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

	if isUDP {
		size := dns.MinMsgSize // 512
		if opt := req.IsEdns0(); opt != nil {
			m.SetEdns0(opt.UDPSize(), false)
			if int(opt.UDPSize()) > size {
				size = int(opt.UDPSize())
			}
		}
		m.Truncate(size) // sets TC if the answer doesn't fit
	}
	return m
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
