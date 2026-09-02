package dns

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The stack-answered lane is the DNS head's second inlet lane: a zone with
// answer_mode=stack (0024) hands each answer-cache-missed query to the
// tenant's `_dns` stack SYNCHRONOUSLY, with the snapshot's would-be answer
// pre-stamped as `@dns.proposed`, and puts the stack's `@dns.res` on the
// wire. Everything that keeps this bounded lives here:
//
//   - answer cache keyed (zone, qname, qtype), by the answer's minimum TTL
//     (negative answers by the SOA minimum) — steady-state QPS is absorbed
//     at memory speed; the stack sees each distinct question once per TTL;
//   - a per-zone dispatch limiter — a random-subdomain flood defeats the
//     cache by construction, so over-limit queries answer with the zone's
//     fallback instead of queueing;
//   - in-flight coalescing — concurrent identical questions share one run;
//   - a hard deadline — past it the head answers with the fallback; the run
//     is NOT cancelled, and a late answer still warms the cache;
//   - default-deny translation — no valid `@dns.res` ⇒ the fallback, same
//     posture as LMTP's 550. The fallback is per zone: "proposal" (what the
//     snapshot would have said — a broken stack degrades to today) or
//     "servfail" (for zones whose truth lives only in the stack).
//
// Stays head-side and never stack-visible: RRL, REFUSED for unserved names
// and ANY, the ACME challenge overlay and RFC2136 receiver, EDNS0
// sizing/truncation. The proposal is whatever the zone's `mode` composes,
// so a pass-through stack (`EMIT @dns.res = @dns.proposed`) answers a stack
// zone byte-identically to its snapshot mode.

const (
	// answerCacheMax bounds the answer cache (entries). Past it, put()
	// evicts a slice of arbitrary entries — the cache is a QPS absorber,
	// not a store, so any eviction policy that keeps it bounded is fine.
	answerCacheMax = 8192

	// stackMaxRRs caps answer+authority records in a stack response —
	// stack-built amplification guard. Real answers are a handful of RRs.
	stackMaxRRs = 64

	// stackMaxTTL clamps a stack-supplied TTL (RFC 2181 §8 recommends
	// treating anything over a week as suspect).
	stackMaxTTL = 7 * 24 * 3600

	// stackRunTimeout caps the run itself (bus hand-off + response), which
	// outlives the wire deadline so a late answer can warm the cache.
	stackRunTimeout = 60 * time.Second
)

// stackAnswer is a translated, guarded stack response — immutable once
// built, so cache readers share it without copying.
type stackAnswer struct {
	rcode  int
	answer []dns.RR
	ns     []dns.RR
	ttl    time.Duration // cache lifetime; 0 = don't cache
}

// reply renders the answer for one request (fresh header/id, EDNS sizing).
func (a *stackAnswer) reply(req *dns.Msg, isUDP bool) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = false
	m.Authoritative = true
	m.Rcode = a.rcode
	m.Answer = a.answer
	m.Ns = a.ns
	applyUDPSizing(m, req, isUDP)
	return m
}

// answerLane owns the cache, limiter, coalescer and dispatch for stack
// zones. Nil on a controller when the lane is off.
type answerLane struct {
	pu       *processor.Unit
	ctx      context.Context
	node     string
	deadline time.Duration
	limiter  *throttle.Throttle
	cache    *answerCache
	flight   *inflight
	outcomes metric.Int64Counter // chassis.dns.answer, by outcome
}

func newAnswerLane(ctx context.Context, pu *processor.Unit, node string) *answerLane {
	if pu == nil {
		return nil
	}
	deadline := time.Duration(pu.Conf.DNSStackDeadlineMs) * time.Millisecond
	if deadline <= 0 {
		deadline = 1500 * time.Millisecond
	}
	l := &answerLane{
		pu:       pu,
		ctx:      ctx,
		node:     node,
		deadline: deadline,
		limiter:  throttle.New(pu.Conf.DNSStackDispatchPerSec, time.Second),
		cache:    newAnswerCache(answerCacheMax),
		flight:   newInflight(),
	}
	if pu.Mc != nil && pu.Mc.Meter != nil {
		l.outcomes, _ = pu.Mc.Meter.Int64Counter("chassis.dns.answer",
			metric.WithDescription("DNS stack-answered zone replies, by outcome"),
			metric.WithUnit("1"))
	}
	return l
}

// answer serves one query for a stack-answered zone, or returns nil when
// the query is not the lane's to answer (not a stack zone, ANY, opcode/
// question hygiene) so the caller falls through to the snapshot path.
// stackSaw reports whether the stack itself handled THIS query (a sync
// dispatch), so the caller can skip the observe tap — the stack sees each
// query exactly once; cache hits and fallbacks are still tapped.
func (l *answerLane) answer(snap *ZoneSnapshot, w dns.ResponseWriter, req *dns.Msg, isUDP bool) (m *dns.Msg, stackSaw bool) {
	if l == nil || snap == nil || !snap.answering {
		return nil, false
	}
	if req.Opcode != dns.OpcodeQuery || len(req.Question) != 1 {
		return nil, false
	}
	q := req.Question[0]
	qname := strings.ToLower(dns.Fqdn(q.Name))
	z := snap.zoneFor(qname)
	if z == nil || !z.stackAnswered || q.Qtype == dns.TypeANY {
		return nil, false
	}

	key := z.origin + "|" + qname + "|" + dns.Type(q.Qtype).String()
	if a := l.cache.get(key); a != nil {
		l.record("cache")
		return a.reply(req, isUDP), false
	}

	// The proposal: what the snapshot would answer. Computed once, used
	// both as the envelope's `@dns.proposed` and as the fallback.
	pAns, pNs, pRcode := snap.Lookup(q)
	proposal := &stackAnswer{rcode: pRcode, answer: pAns, ns: pNs}

	ob := observation{
		q:         q,
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

	// Coalesce concurrent identical questions onto one run. Followers
	// wait for the leader's result under the same deadline; a leader that
	// times out hands them the same "no answer" and everyone falls back.
	// The dispatch limiter is spent by the LEADER only: a cold-cache burst
	// of one question is one dispatch, not N tokens (checking it per query
	// made followers fall back while the leader's answer was in flight).
	res, leader := l.flight.do(key, l.deadline, func() *stackAnswer {
		if ok, _ := l.limiter.Allow(z.origin); !ok {
			l.record("fallback_limit")
			return nil
		}
		a, outcome := l.dispatch(z, ob, proposal, key)
		l.record(outcome)
		return a
	})
	if res == nil {
		return l.fallback(z, proposal, req, isUDP), leader
	}
	return res.reply(req, isUDP), leader
}

// fallback renders the zone's no-answer policy.
func (l *answerLane) fallback(z *zone, proposal *stackAnswer, req *dns.Msg, isUDP bool) *dns.Msg {
	if z.fallback == "servfail" {
		m := new(dns.Msg)
		m.SetReply(req)
		m.RecursionAvailable = false
		m.Rcode = dns.RcodeServerFailure
		applyUDPSizing(m, req, isUDP)
		return m
	}
	m := proposal.reply(req, isUDP)
	m.Authoritative = proposal.rcode != dns.RcodeRefused
	return m
}

// dispatch runs one query through the bus and waits up to the deadline
// for the stack's answer. Returns the translated answer (already cached)
// or nil + the outcome that explains the fallback.
func (l *answerLane) dispatch(z *zone, ob observation, proposal *stackAnswer, key string) (*stackAnswer, string) {
	rid := hxid.NewTimeSort().String()
	payload := buildAnswerEnvelope(ob, proposal, rid, l.node, time.Now())

	// The run's own lifetime outlives the wire deadline on purpose.
	rctx, cancel := context.WithTimeout(l.ctx, stackRunTimeout)
	rctx = context.WithValue(rctx, config.CtxKeyRid, rid)
	resCh := make(chan event.Payload, 1)
	envelope := event.PackageJSON(rctx, payload, resCh, "dns")

	deadline := time.NewTimer(l.deadline)
	defer deadline.Stop()

	select {
	case l.pu.Bus <- envelope:
	case <-deadline.C:
		cancel()
		return nil, "fallback_bus_timeout"
	}

	select {
	case res := <-resCh:
		cancel()
		return l.settle(res.Raw, z, ob.q, key, rid)
	case <-deadline.C:
		// Past the wire deadline: answer with the fallback now, but let the
		// run finish and warm the cache for the next asker.
		go func() {
			defer cancel()
			select {
			case res := <-resCh:
				if a, outcome := l.settle(res.Raw, z, ob.q, key, rid); a != nil {
					l.record("late_" + outcome)
				}
			case <-rctx.Done():
			}
		}()
		return nil, "fallback_deadline"
	}
}

// settle translates a response and caches a valid answer.
func (l *answerLane) settle(raw string, z *zone, q dns.Question, key, rid string) (*stackAnswer, string) {
	if status, reason, ok := admission.Denied(raw); ok {
		if l.pu.Logger.Core().Enabled(zap.DebugLevel) {
			l.pu.Logger.Debug("dns stack answer denied by admission",
				zap.String("rid", rid), zap.String("tenant", z.tenantSlug),
				zap.Int("status", status), zap.String("reason", reason))
		}
		return nil, "fallback_denied"
	}
	a, err := translateStackAnswer(raw, z, q)
	if err != nil {
		l.pu.Logger.Warn("dns stack answer rejected; using zone fallback",
			zap.String("rid", rid), zap.String("zone", z.origin),
			zap.String("qname", q.Name), zap.String("err", err.Error()))
		return nil, "fallback_invalid"
	}
	if a == nil {
		// No @dns.res at all — the stack chose not to answer (or suspended
		// without pre-emitting one). Default-deny ⇒ fallback.
		return nil, "fallback_absent"
	}
	if a.ttl > 0 {
		l.cache.put(key, a, time.Now().Add(a.ttl))
	}
	return a, "stack"
}

func (l *answerLane) record(outcome string) {
	if l == nil || l.outcomes == nil {
		return
	}
	l.outcomes.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String("txco.dns.outcome", outcome),
	))
}

// reset drops every cached answer — called on snapshot rebuild so a
// re-applied `_dns` stack or a flipped zone takes effect on the next query.
func (l *answerLane) reset() {
	if l != nil {
		l.cache.reset()
	}
}

// buildAnswerEnvelope renders the phase="answer" envelope: the observe
// facts (question, client, zone) plus the snapshot proposal in the same
// presentation-format encoding the response contract uses.
func buildAnswerEnvelope(ob observation, proposal *stackAnswer, rid, node string, now time.Time) string {
	b := envelopeBase(ob, rid, node, now, "answer")
	b.Set("_txc.dns.zone.fallback", ob.zone.fallback)
	b.Set("_txc.dns.proposed.rcode", rcodeName(proposal.rcode))
	b.Set("_txc.dns.proposed.answer", rrStrings(proposal.answer))
	b.Set("_txc.dns.proposed.authority", rrStrings(proposal.ns))
	return b.String()
}

// --- response translation ------------------------------------------------

// translateStackAnswer reads `_txc.dns.res` off a stack response and turns
// it into a guarded stackAnswer. Returns (nil, nil) when the response
// carries no `_txc.dns.res` at all (default-deny is the caller's call), and
// an error for a present-but-unacceptable one. Pure; unit-tested directly.
//
// Contract (read as `@dns.res.*` in txcl):
//
//	rcode      "NOERROR" (default when absent) | "NXDOMAIN" | "SERVFAIL" | "REFUSED"
//	answer     ["<owner> <ttl> IN <TYPE> <rdata>", …]  zone-file lines, dns.NewRR
//	authority  same encoding (typically the SOA for negative answers)
//
// Guards: every owner inside the queried zone; no OPT/TSIG/TKEY/meta types;
// at most stackMaxRRs records; TTLs clamped to stackMaxTTL. Any violation
// rejects the WHOLE response — a stack that answers out of bailiwick is
// wrong, not partially right.
func translateStackAnswer(raw string, z *zone, q dns.Question) (*stackAnswer, error) {
	res := gjson.Get(raw, "_txc.dns.res")
	if !res.Exists() {
		return nil, nil
	}
	if !res.IsObject() {
		return nil, errStack("@dns.res must be an object")
	}
	rcode := dns.RcodeSuccess
	if rc := res.Get("rcode"); rc.Exists() {
		code, ok := dns.StringToRcode[strings.ToUpper(strings.TrimSpace(rc.String()))]
		if !ok {
			return nil, errStack("unknown rcode " + rc.String())
		}
		switch code {
		case dns.RcodeSuccess, dns.RcodeNameError, dns.RcodeServerFailure, dns.RcodeRefused:
			rcode = code
		default:
			return nil, errStack("rcode " + rc.String() + " not allowed from a stack")
		}
	}
	answer, err := parseStackRRs(res.Get("answer"), z, "answer")
	if err != nil {
		return nil, err
	}
	ns, err := parseStackRRs(res.Get("authority"), z, "authority")
	if err != nil {
		return nil, err
	}
	if len(answer)+len(ns) > stackMaxRRs {
		return nil, errStack("too many records")
	}
	// Cache lifetime: the minimum TTL across what was answered; a purely
	// negative/empty answer lives as long as the zone's SOA minimum
	// (RFC 2308 negative caching), so NXDOMAIN floods don't re-dispatch.
	ttl := time.Duration(-1)
	for _, rr := range append(append([]dns.RR{}, answer...), ns...) {
		t := time.Duration(rr.Header().Ttl) * time.Second
		if ttl < 0 || t < ttl {
			ttl = t
		}
	}
	if ttl < 0 {
		if z.soa != nil {
			ttl = time.Duration(z.soa.Minttl) * time.Second
		} else {
			ttl = 0
		}
	}
	return &stackAnswer{rcode: rcode, answer: answer, ns: ns, ttl: ttl}, nil
}

// parseStackRRs parses one section of presentation-format lines under the
// bailiwick + type guards. Absent/null → empty; anything but an array of
// strings is an error.
func parseStackRRs(sec gjson.Result, z *zone, name string) ([]dns.RR, error) {
	if !sec.Exists() || sec.Type == gjson.Null {
		return nil, nil
	}
	if !sec.IsArray() {
		return nil, errStack("@dns.res." + name + " must be an array of zone-file lines")
	}
	var out []dns.RR
	var perr error
	sec.ForEach(func(_, v gjson.Result) bool {
		if v.Type != gjson.String {
			perr = errStack("@dns.res." + name + " entries must be strings")
			return false
		}
		rr, err := dns.NewRR(v.String())
		if err != nil || rr == nil {
			perr = errStack("@dns.res." + name + ": cannot parse " + v.String())
			return false
		}
		h := rr.Header()
		switch h.Rrtype {
		case dns.TypeOPT, dns.TypeTSIG, dns.TypeTKEY, dns.TypeANY, dns.TypeAXFR, dns.TypeIXFR, dns.TypeNone:
			perr = errStack("@dns.res." + name + ": type " + dns.Type(h.Rrtype).String() + " not allowed")
			return false
		}
		owner := strings.ToLower(h.Name)
		if owner != z.originFQDN && !strings.HasSuffix(owner, "."+z.originFQDN) {
			perr = errStack("@dns.res." + name + ": owner " + h.Name + " is outside zone " + z.originFQDN)
			return false
		}
		if h.Ttl > stackMaxTTL {
			h.Ttl = stackMaxTTL
		}
		out = append(out, rr)
		return true
	})
	if perr != nil {
		return nil, perr
	}
	return out, nil
}

type errStack string

func (e errStack) Error() string { return string(e) }

// --- answer cache --------------------------------------------------------

type cacheEntry struct {
	a       *stackAnswer
	expires time.Time
}

type answerCache struct {
	mu  sync.Mutex
	m   map[string]cacheEntry
	max int
}

func newAnswerCache(max int) *answerCache {
	return &answerCache{m: make(map[string]cacheEntry), max: max}
}

func (c *answerCache) get(key string) *stackAnswer {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok {
		return nil
	}
	if time.Now().After(e.expires) {
		delete(c.m, key)
		return nil
	}
	return e.a
}

func (c *answerCache) put(key string, a *stackAnswer, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.m) >= c.max {
		// Bounded, not clever: drop an arbitrary eighth (map order is
		// random) so a flood of distinct names can't grow this without end.
		n := c.max / 8
		for k := range c.m {
			delete(c.m, k)
			n--
			if n <= 0 {
				break
			}
		}
	}
	c.m[key] = cacheEntry{a: a, expires: expires}
}

func (c *answerCache) reset() {
	c.mu.Lock()
	c.m = make(map[string]cacheEntry)
	c.mu.Unlock()
}

func (c *answerCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// --- in-flight coalescing -------------------------------------------------

type flightCall struct {
	done chan struct{}
	res  *stackAnswer
}

// inflight is a minimal singleflight: the first caller for a key runs fn;
// concurrent callers for the same key wait (up to their own deadline) for
// its result instead of dispatching again.
type inflight struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

func newInflight() *inflight { return &inflight{m: make(map[string]*flightCall)} }

// do returns fn's result and whether this caller was the leader (ran fn).
// A follower whose wait exceeds `wait` returns nil (fallback).
func (f *inflight) do(key string, wait time.Duration, fn func() *stackAnswer) (*stackAnswer, bool) {
	f.mu.Lock()
	if c, ok := f.m[key]; ok {
		f.mu.Unlock()
		select {
		case <-c.done:
			return c.res, false
		case <-time.After(wait):
			return nil, false
		}
	}
	c := &flightCall{done: make(chan struct{})}
	f.m[key] = c
	f.mu.Unlock()

	c.res = fn()
	f.mu.Lock()
	delete(f.m, key)
	f.mu.Unlock()
	close(c.done)
	return c.res, true
}
