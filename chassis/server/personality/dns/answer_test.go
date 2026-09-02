package dns

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// responder is a fake processor: it drains the bus and answers each
// envelope with fn(raw), recording every phase it saw.
type responder struct {
	calls  atomic.Int64
	mu     sync.Mutex
	phases []string
	stop   context.CancelFunc
}

func startResponder(t *testing.T, bus chan *event.Envelope, fn func(raw string) string) *responder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &responder{stop: cancel}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case env := <-bus:
				raw := env.Payload.Raw
				r.calls.Add(1)
				r.mu.Lock()
				r.phases = append(r.phases, gjson.Get(raw, "_txc.dns.phase").String())
				r.mu.Unlock()
				out := fn(raw)
				select {
				case env.ResCh <- event.Payload{Raw: out, Type: event.JSON}:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	t.Cleanup(cancel)
	return r
}

func (r *responder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.phases...)
}

// passThrough echoes the head's proposal back as the answer — the
// backwards-compatible `_dns` stack (`EMIT @dns.res = @dns.proposed`).
func passThrough(raw string) string {
	out, _ := sjson.SetRaw("{}", "_txc.dns.res", gjson.Get(raw, "_txc.dns.proposed").Raw)
	return out
}

// txtAnswer answers every query with one TXT at the queried name.
func txtAnswer(val string) func(string) string {
	return func(raw string) string {
		name := gjson.Get(raw, "_txc.dns.q.name").String()
		out, _ := sjson.Set("{}", "_txc.dns.res.rcode", "NOERROR")
		out, _ = sjson.Set(out, "_txc.dns.res.answer", []string{name + " 30 IN TXT \"" + val + "\""})
		return out
	}
}

type laneOpts struct {
	fallback   string // "" → proposal
	deadlineMs int
	perSec     int
	observe    bool
}

// newLaneController: pat.example.com (patTenant, active `_dns` + `shop`
// stacks) flipped to answer_mode=stack; ops.example.com stays a snapshot
// zone. The observe tap is started only when opts.observe.
func newLaneController(t *testing.T, bus chan *event.Envelope, o laneOpts) (*DNSController, *ZoneSnapshot) {
	t.Helper()
	db := newTestDB(t)
	seedSettings(t, db, "ns1.txco.io", "203.0.113.10", "mx.txco.io")
	seedTenant(t, db, patTenant, patSlug)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	seedActiveStack(t, db, patTenant, observeStack, fixedTS)
	seedActiveStack(t, db, patTenant, "shop", fixedTS)
	seedZone(t, db, fixedTS)
	flipZoneToStack(t, db, "pat.example.com", o.fallback)
	snap := buildOrDie(t, db, patCfg())
	if !snap.answering {
		t.Fatal("snapshot has no answering zone")
	}

	sample := 0
	if o.observe {
		sample = 1
	}
	pu := &processor.Unit{
		Logger: zap.NewNop(),
		Bus:    bus,
		Conf: config.Config{
			DNSObserveSample: sample, DNSObserveMaxInflight: 1,
			DNSStackDeadlineMs: o.deadlineMs, DNSStackDispatchPerSec: o.perSec,
		},
	}
	c := &DNSController{ctx: context.Background(), pu: pu}
	c.snap.Store(snap)
	c.lane = newAnswerLane(context.Background(), pu, "node-t")
	if o.observe {
		c.tap = newObserveTap(pu, 0)
		c.tap.start(context.Background())
		t.Cleanup(c.tap.stop)
	}
	return c, snap
}

func flipZoneToStack(t *testing.T, db *sql.DB, origin, fallback string) {
	t.Helper()
	if fallback == "" {
		fallback = "proposal"
	}
	if _, err := db.Exec(`UPDATE dns_zones SET answer_mode='stack', stack_fallback=? WHERE origin=?`, fallback, origin); err != nil {
		t.Fatalf("flip zone: %v", err)
	}
}

func rrLines(rrs []dns.RR) []string { return rrStrings(rrs) }

// TestStackLanePassThroughIsWireIdentical: with the pass-through stack, a
// stack zone answers exactly what its snapshot would — and the stack saw
// the query, so the observe tap stays quiet for it.
func TestStackLanePassThroughIsWireIdentical(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, snap := newLaneController(t, bus, laneOpts{observe: true})
	var got string
	r := startResponder(t, bus, func(raw string) string { got = raw; return passThrough(raw) })

	w := ask(c, bus, "shop.pat.example.com.", dns.TypeA, true, true)
	req := new(dns.Msg)
	req.SetQuestion("shop.pat.example.com.", dns.TypeA)
	want := buildReply(snap, req, false)
	if w.written.Rcode != want.Rcode || !w.written.Authoritative ||
		strings.Join(rrLines(w.written.Answer), "\n") != strings.Join(rrLines(want.Answer), "\n") {
		t.Fatalf("pass-through reply differs from snapshot:\n got %v\nwant %v", w.written.Answer, want.Answer)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
	for path, v := range map[string]string{
		"_txc.dns.phase":            "answer",
		"_txc.dns.tenant":           patSlug,
		"_txc.dns.zone.fallback":    "proposal",
		"_txc.dns.proposed.rcode":   "NOERROR",
		"_txc.dns.q.type":           "A",
		"_txc.dns.client.ip":        "203.0.113.9",
		"_txc.dns.client.transport": "udp",
	} {
		if g := gjson.Get(got, path).String(); g != v {
			t.Errorf("%s = %q, want %q", path, g, v)
		}
	}
	if ans := gjson.Get(got, "_txc.dns.proposed.answer").Array(); len(ans) != 1 || !strings.Contains(ans[0].String(), "203.0.113.10") {
		t.Errorf("proposed.answer = %v", ans)
	}
	if gjson.Get(got, "_txc.dns.reply").Exists() {
		t.Errorf("answer-phase envelope must not carry a reply section")
	}
	// The stack saw this query; no observe tap for it.
	time.Sleep(150 * time.Millisecond)
	if p := r.seen(); strings.Join(p, ",") != "answer" {
		t.Fatalf("phases = %v, want only answer (stack-answered queries are not re-tapped)", p)
	}
}

// TestStackLaneCustomAnswerAndCache: a stack answer replaces the snapshot's
// NXDOMAIN; the second identical query is served from the answer cache
// (no dispatch) and, having not been seen by the stack, IS observed.
func TestStackLaneCustomAnswerAndCache(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, _ := newLaneController(t, bus, laneOpts{observe: true})
	r := startResponder(t, bus, txtAnswer("v1.4.2"))

	for i := 0; i < 2; i++ {
		w := ask(c, bus, "build.pat.example.com.", dns.TypeTXT, false, false)
		if w.written.Rcode != dns.RcodeSuccess || !w.written.Authoritative || len(w.written.Answer) != 1 {
			t.Fatalf("query %d: rcode=%d aa=%v ans=%d", i, w.written.Rcode, w.written.Authoritative, len(w.written.Answer))
		}
		txt, ok := w.written.Answer[0].(*dns.TXT)
		if !ok || strings.Join(txt.Txt, "") != "v1.4.2" || txt.Hdr.Ttl != 30 {
			t.Fatalf("query %d: answer = %v", i, w.written.Answer[0])
		}
	}
	time.Sleep(200 * time.Millisecond)
	if n := r.calls.Load(); n != 2 {
		t.Fatalf("bus saw %d envelopes, want 2 (one answer, one observe of the cache hit)", n)
	}
	if p := r.seen(); strings.Join(p, ",") != "answer,observe" {
		t.Fatalf("phases = %v, want answer,observe", p)
	}
	if c.lane.cache.len() != 1 {
		t.Fatalf("cache entries = %d, want 1", c.lane.cache.len())
	}
}

// TestStackLaneDeadlineFallsBackThenWarms: past the deadline the wire gets
// the proposal (here the snapshot's NXDOMAIN); the run is not cancelled and
// its late answer warms the cache for the next asker.
func TestStackLaneDeadlineFallsBackThenWarms(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, _ := newLaneController(t, bus, laneOpts{deadlineMs: 50})
	slow := txtAnswer("late")
	r := startResponder(t, bus, func(raw string) string { time.Sleep(200 * time.Millisecond); return slow(raw) })

	start := time.Now()
	w := ask(c, bus, "slow.pat.example.com.", dns.TypeTXT, true, false)
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Fatalf("deadline not honoured: reply took %v", el)
	}
	if w.written.Rcode != dns.RcodeNameError || !w.written.Authoritative {
		t.Fatalf("fallback should be the proposal (NXDOMAIN, AA): rcode=%d aa=%v", w.written.Rcode, w.written.Authoritative)
	}

	time.Sleep(400 * time.Millisecond) // let the late answer land
	w = ask(c, bus, "slow.pat.example.com.", dns.TypeTXT, true, false)
	if w.written.Rcode != dns.RcodeSuccess || len(w.written.Answer) != 1 {
		t.Fatalf("late answer did not warm the cache: rcode=%d ans=%d", w.written.Rcode, len(w.written.Answer))
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("dispatches = %d, want 1 (second query from cache)", n)
	}
}

// TestStackLaneFallbackServfail: a zone whose truth lives in the stack
// answers SERVFAIL, not the snapshot, when the stack gives no @dns.res.
func TestStackLaneFallbackServfail(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, _ := newLaneController(t, bus, laneOpts{fallback: "servfail"})
	startResponder(t, bus, func(string) string { return `{}` })

	w := ask(c, bus, "shop.pat.example.com.", dns.TypeA, true, false)
	if w.written.Rcode != dns.RcodeServerFailure || w.written.Authoritative || len(w.written.Answer) != 0 {
		t.Fatalf("want SERVFAIL: rcode=%d aa=%v ans=%d", w.written.Rcode, w.written.Authoritative, len(w.written.Answer))
	}
	if c.lane.cache.len() != 0 {
		t.Fatalf("a fallback must not be cached")
	}
}

// TestStackLaneInvalidAnswerFallsBack: an out-of-bailiwick answer is
// rejected whole and the proposal goes on the wire.
func TestStackLaneInvalidAnswerFallsBack(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, _ := newLaneController(t, bus, laneOpts{})
	startResponder(t, bus, func(string) string {
		out, _ := sjson.Set("{}", "_txc.dns.res.answer", []string{"evil.example.org. 30 IN A 10.0.0.1"})
		return out
	})
	w := ask(c, bus, "shop.pat.example.com.", dns.TypeA, true, false)
	if w.written.Rcode != dns.RcodeSuccess || len(w.written.Answer) != 1 || !strings.Contains(w.written.Answer[0].String(), "203.0.113.10") {
		t.Fatalf("want the snapshot A as fallback, got rcode=%d %v", w.written.Rcode, w.written.Answer)
	}
}

// TestStackLaneLimiter: over the per-zone dispatch ceiling, queries fall
// back without reaching the bus.
func TestStackLaneLimiter(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, _ := newLaneController(t, bus, laneOpts{perSec: 1})
	r := startResponder(t, bus, txtAnswer("x"))

	w1 := ask(c, bus, "a.pat.example.com.", dns.TypeTXT, true, false)
	w2 := ask(c, bus, "b.pat.example.com.", dns.TypeTXT, true, false)
	if w1.written.Rcode != dns.RcodeSuccess || len(w1.written.Answer) != 1 {
		t.Fatalf("first query should be stack-answered")
	}
	if w2.written.Rcode != dns.RcodeNameError {
		t.Fatalf("second query should fall back to the proposal (NXDOMAIN), got rcode=%d", w2.written.Rcode)
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
}

// TestStackLaneCoalesces: concurrent identical questions share one run.
func TestStackLaneCoalesces(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, _ := newLaneController(t, bus, laneOpts{})
	slow := txtAnswer("once")
	r := startResponder(t, bus, func(raw string) string { time.Sleep(100 * time.Millisecond); return slow(raw) })

	var wg sync.WaitGroup
	results := make([]*fakeWriter, 5)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = ask(c, bus, "same.pat.example.com.", dns.TypeTXT, true, false)
		}(i)
	}
	wg.Wait()
	for i, w := range results {
		if w.written.Rcode != dns.RcodeSuccess || len(w.written.Answer) != 1 {
			t.Fatalf("caller %d did not get the shared answer: rcode=%d ans=%d", i, w.written.Rcode, len(w.written.Answer))
		}
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("dispatches = %d, want 1 (coalesced)", n)
	}
}

// TestStackZoneWithoutDnsStackServesSnapshot: answer_mode=stack with no
// `_dns` stack to dispatch to serves the snapshot and never touches the bus.
func TestStackZoneWithoutDnsStackServesSnapshot(t *testing.T) {
	db := newTestDB(t)
	seedTenant(t, db, testTenantID, "test")
	seedZone(t, db, fixedTS)
	flipZoneToStack(t, db, testOrigin, "")
	snap := buildOrDie(t, db, SynthConfig{})
	if snap.answering || snap.byOrigin(testOrigin).stackAnswered {
		t.Fatal("zone must not be stack-answered without a _dns stack")
	}
	bus := make(chan *event.Envelope, 1)
	pu := &processor.Unit{Logger: zap.NewNop(), Bus: bus, Conf: config.Config{}}
	c := &DNSController{ctx: context.Background(), pu: pu}
	c.snap.Store(snap)
	c.lane = newAnswerLane(context.Background(), pu, "n")
	w := ask(c, bus, "ops.example.com.", dns.TypeSOA, true, false)
	if w.written.Rcode != dns.RcodeSuccess || len(bus) != 0 {
		t.Fatalf("snapshot path expected: rcode=%d bus=%d", w.written.Rcode, len(bus))
	}
}

// TestTranslateStackAnswerGuards pins the response contract's guards.
func TestTranslateStackAnswerGuards(t *testing.T) {
	z := &zone{origin: "pat.example.com", originFQDN: "pat.example.com.",
		soa: &dns.SOA{Hdr: dns.RR_Header{Name: "pat.example.com.", Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 300}, Minttl: 90}}
	q := dns.Question{Name: "x.pat.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	res := func(answer []string, extra ...string) string {
		out := "{}"
		if answer != nil {
			out, _ = sjson.Set(out, "_txc.dns.res.answer", answer)
		} else {
			out, _ = sjson.Set(out, "_txc.dns.res.rcode", "NOERROR")
		}
		for i := 0; i+1 < len(extra); i += 2 {
			out, _ = sjson.Set(out, "_txc.dns.res."+extra[i], extra[i+1])
		}
		return out
	}
	many := make([]string, stackMaxRRs+1)
	for i := range many {
		many[i] = "x.pat.example.com. 30 IN A 10.0.0.1"
	}

	cases := []struct {
		name    string
		raw     string
		wantErr string // substring; "" = accepted
		check   func(*stackAnswer) string
	}{
		{"absent res → nil,nil", `{"_txc":{"web":{"res":{"status":202}}}}`, "", func(a *stackAnswer) string {
			if a != nil {
				return "expected nil answer for absent res"
			}
			return ""
		}},
		{"plain A", res([]string{"x.pat.example.com. 30 IN A 10.0.0.1"}), "", func(a *stackAnswer) string {
			if a.rcode != dns.RcodeSuccess || len(a.answer) != 1 || a.ttl != 30*time.Second {
				return "bad translation"
			}
			return ""
		}},
		{"owner without trailing dot is FQDN", res([]string{"x.pat.example.com 30 IN A 10.0.0.1"}), "", func(a *stackAnswer) string {
			if a.answer[0].Header().Name != "x.pat.example.com." {
				return "owner = " + a.answer[0].Header().Name
			}
			return ""
		}},
		{"TTL clamped", res([]string{"x.pat.example.com. 99999999 IN A 10.0.0.1"}), "", func(a *stackAnswer) string {
			if a.answer[0].Header().Ttl != stackMaxTTL {
				return "ttl not clamped"
			}
			return ""
		}},
		{"negative answer lives SOA minimum", res(nil, "rcode", "NXDOMAIN"), "", func(a *stackAnswer) string {
			if a.rcode != dns.RcodeNameError || a.ttl != 90*time.Second {
				return "negative ttl/rcode wrong"
			}
			return ""
		}},
		{"authority SOA sets ttl", res(nil, "rcode", "NXDOMAIN", "authority.0", "pat.example.com. 60 IN SOA ns1. h. 1 2 3 4 5"), "", func(a *stackAnswer) string {
			if len(a.ns) != 1 || a.ttl != 60*time.Second {
				return "authority not honoured"
			}
			return ""
		}},
		{"out of bailiwick", res([]string{"evil.example.org. 30 IN A 10.0.0.1"}), "outside zone", nil},
		{"parent of zone is outside", res([]string{"example.com. 30 IN A 10.0.0.1"}), "outside zone", nil},
		{"OPT via RFC3597", res([]string{"x.pat.example.com. 0 IN TYPE41 \\# 0"}), "not allowed", nil},
		{"unparsable", res([]string{"this is not a record"}), "cannot parse", nil},
		{"too many", res(many), "too many", nil},
		{"rcode not allowed", res(nil, "rcode", "FORMERR"), "not allowed", nil},
		{"rcode unknown", res(nil, "rcode", "BOGUS"), "unknown rcode", nil},
		{"answer not array", `{"_txc":{"dns":{"res":{"answer":"x.pat.example.com. 30 IN A 10.0.0.1"}}}}`, "must be an array", nil},
		{"res not object", `{"_txc":{"dns":{"res":"nope"}}}`, "must be an object", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := translateStackAnswer(tc.raw, z, q)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tc.check != nil {
				if msg := tc.check(a); msg != "" {
					t.Fatal(msg)
				}
			}
		})
	}
}

// TestAnswerCacheBounded: the cache never grows past its max.
func TestAnswerCacheBounded(t *testing.T) {
	c := newAnswerCache(64)
	exp := time.Now().Add(time.Minute)
	for i := 0; i < 200; i++ {
		c.put(strings.Repeat("k", i%50)+string(rune('a'+i%26))+string(rune('0'+i%10)), &stackAnswer{}, exp)
	}
	if n := c.len(); n > 64 {
		t.Fatalf("cache grew to %d entries, max 64", n)
	}
	c.put("expired", &stackAnswer{}, time.Now().Add(-time.Second))
	if c.get("expired") != nil {
		t.Fatal("expired entry served")
	}
}

// TestStackLaneColdBurstUnderLimiter: a burst of identical questions on a
// cold cache costs ONE dispatch token, so every caller gets the stack's
// answer even with the tightest limiter (the limiter is spent by the
// coalescing leader, not per query).
func TestStackLaneColdBurstUnderLimiter(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c, _ := newLaneController(t, bus, laneOpts{perSec: 1})
	slow := txtAnswer("burst")
	r := startResponder(t, bus, func(raw string) string { time.Sleep(50 * time.Millisecond); return slow(raw) })

	var wg sync.WaitGroup
	results := make([]*fakeWriter, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = ask(c, bus, "burst.pat.example.com.", dns.TypeTXT, true, false)
		}(i)
	}
	wg.Wait()
	for i, w := range results {
		if w.written.Rcode != dns.RcodeSuccess || len(w.written.Answer) != 1 {
			t.Fatalf("caller %d fell back (rcode=%d) — limiter charged a follower", i, w.written.Rcode)
		}
	}
	if n := r.calls.Load(); n != 1 {
		t.Fatalf("dispatches = %d, want 1", n)
	}
	// The budget IS spent now: a different name in the same second falls back.
	w := ask(c, bus, "other.pat.example.com.", dns.TypeTXT, true, false)
	if w.written.Rcode != dns.RcodeNameError {
		t.Fatalf("second distinct question should hit the limiter, got rcode=%d", w.written.Rcode)
	}
}
