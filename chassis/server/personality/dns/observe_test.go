package dns

import (
	"context"
	"database/sql"
	"net"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// fakeWriter is a miekg/dns ResponseWriter that records the reply and, at
// the moment of the write, how many envelopes the bus already held — the
// structural proof that the tap dispatches AFTER the wire write.
type fakeWriter struct {
	remote        net.Addr
	bus           chan *event.Envelope
	written       *dns.Msg
	busLenAtWrite int
}

func (f *fakeWriter) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5354}
}
func (f *fakeWriter) RemoteAddr() net.Addr { return f.remote }
func (f *fakeWriter) WriteMsg(m *dns.Msg) error {
	f.written = m
	f.busLenAtWrite = len(f.bus)
	return nil
}
func (f *fakeWriter) Write(b []byte) (int, error) { return len(b), nil }
func (f *fakeWriter) Close() error                { return nil }
func (f *fakeWriter) TsigStatus() error           { return nil }
func (f *fakeWriter) TsigTimersOnly(bool)         {}
func (f *fakeWriter) Hijack()                     {}

// newTapController builds a controller serving two zones: pat.example.com
// (tenant patTenant, which HAS an active `_dns` stack plus a `shop` stack
// so `shop.pat.example.com A` answers NOERROR) and ops.example.com
// (testTenantID, no `_dns` stack). The tap is constructed but NOT started
// — tests that need workers call c.tap.start.
// seedTenant inserts the tenants row that maps a tenant_id to its routable
// slug (what `_txc.dns.tenant` must carry).
func seedTenant(t *testing.T, db *sql.DB, id, slug string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO tenants (tenant_id, slug, created_at) VALUES (?, ?, ?)`, id, slug, fixedTS); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
}

const patSlug = "pat"

func newTapController(t *testing.T, bus chan *event.Envelope, sample, queueDepth int) *DNSController {
	t.Helper()
	db := newTestDB(t)
	seedSettings(t, db, "ns1.txco.io", "203.0.113.10", "mx.txco.io")
	seedTenant(t, db, patTenant, patSlug)
	seedPatternZone(t, db, patTenant, "pat.example.com", fixedTS)
	seedActiveStack(t, db, patTenant, observeStack, fixedTS)
	seedActiveStack(t, db, patTenant, "shop", fixedTS)
	seedZone(t, db, fixedTS) // ops.example.com for testTenantID, no _dns
	snap := buildOrDie(t, db, patCfg())

	pu := &processor.Unit{
		Logger: zap.NewNop(),
		Bus:    bus,
		Conf:   config.Config{DNSObserveSample: sample, DNSObserveMaxInflight: 1},
	}
	c := &DNSController{ctx: context.Background(), pu: pu}
	c.snap.Store(snap)
	c.tap = newObserveTap(pu, queueDepth)
	if c.tap == nil {
		t.Fatalf("tap nil for sample=%d", sample)
	}
	return c
}

func udpClient() net.Addr { return &net.UDPAddr{IP: net.ParseIP("203.0.113.9"), Port: 5300} }

func ask(c *DNSController, bus chan *event.Envelope, name string, qtype uint16, isUDP bool, edns bool) *fakeWriter {
	req := new(dns.Msg)
	req.SetQuestion(name, qtype)
	if edns {
		req.SetEdns0(1232, false)
	}
	w := &fakeWriter{remote: udpClient(), bus: bus}
	c.makeHandler(isUDP)(w, req)
	return w
}

func recvEnvelope(t *testing.T, bus chan *event.Envelope, within time.Duration) *event.Envelope {
	t.Helper()
	select {
	case env := <-bus:
		return env
	case <-time.After(within):
		return nil
	}
}

// TestObserveTapDispatchesAfterReply: an answered query in an observing
// zone reaches the bus as a `dns` envelope carrying the question, client,
// zone and reply facts — and only after the reply was written.
func TestObserveTapDispatchesAfterReply(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c := newTapController(t, bus, 1, 0)
	c.tap.start(context.Background())
	defer c.tap.stop()

	w := ask(c, bus, "shop.pat.example.com.", dns.TypeA, true, true)
	if w.written == nil || w.written.Rcode != dns.RcodeSuccess || len(w.written.Answer) != 1 {
		t.Fatalf("wire reply: %+v", w.written)
	}
	if w.busLenAtWrite != 0 {
		t.Fatalf("bus held %d envelopes at wire-write time; tap must run after the reply", w.busLenAtWrite)
	}

	env := recvEnvelope(t, bus, 2*time.Second)
	if env == nil {
		t.Fatal("no envelope on the bus")
	}
	if env.Src != "dns" {
		t.Errorf("envelope.Src = %q, want dns", env.Src)
	}
	raw := env.Payload.Raw
	want := map[string]string{
		"_txc.src":                  "dns",
		"_txc.dns.tenant":           patSlug, // the routable slug, never the tnt_ id
		"_txc.dns.phase":            "observe",
		"_txc.dns.q.name":           "shop.pat.example.com.",
		"_txc.dns.q.type":           "A",
		"_txc.dns.q.class":          "IN",
		"_txc.dns.client.ip":        "203.0.113.9",
		"_txc.client.ip":            "203.0.113.9",
		"_txc.dns.client.transport": "udp",
		"_txc.dns.zone.origin":      "pat.example.com",
		"_txc.dns.zone.mode":        "pattern",
		"_txc.dns.reply.rcode":      "NOERROR",
	}
	for path, v := range want {
		if got := gjson.Get(raw, path).String(); got != v {
			t.Errorf("%s = %q, want %q", path, got, v)
		}
	}
	if got := gjson.Get(raw, "_txc.dns.client.edns_udpsize").Int(); got != 1232 {
		t.Errorf("edns_udpsize = %d, want 1232", got)
	}
	if !gjson.Get(raw, "_txc.dns.reply.authoritative").Bool() {
		t.Errorf("reply.authoritative must be true")
	}
	if gjson.Get(raw, "_txc.dns.reply.truncated").Bool() {
		t.Errorf("reply.truncated must be false")
	}
	ans := gjson.Get(raw, "_txc.dns.reply.answer").Array()
	if len(ans) != 1 || !strings.Contains(ans[0].String(), "203.0.113.10") {
		t.Errorf("reply.answer = %v, want one A line for 203.0.113.10", ans)
	}
	if got := gjson.Get(raw, "_txc.dns.reply.authority").Raw; got != "[]" {
		t.Errorf("reply.authority = %s, want []", got)
	}
	if gjson.Get(raw, "_txc.rid").String() == "" || gjson.Get(raw, "_ts").String() == "" {
		t.Errorf("rid/_ts missing")
	}
	if gjson.Get(raw, "_txc.route").Exists() {
		t.Errorf("head must not pre-stamp _txc.route; detectTenantBody proposes from _txc.dns.tenant")
	}
	// Drain the response so the worker records "ok" and exits cleanly.
	env.ResCh <- event.Payload{Raw: "{}", Type: event.JSON}
}

// TestObserveTapSkips: a zone whose tenant has no `_dns` stack, a name
// outside every served zone (REFUSED), and a TCP NXDOMAIN in the observing
// zone — only the last one is tapped.
func TestObserveTapSkips(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c := newTapController(t, bus, 1, 0)
	c.tap.start(context.Background())
	defer c.tap.stop()

	ask(c, bus, "ops.example.com.", dns.TypeSOA, true, false) // served, tenant has no _dns
	ask(c, bus, "example.org.", dns.TypeA, true, false)       // unserved → REFUSED
	if env := recvEnvelope(t, bus, 150*time.Millisecond); env != nil {
		t.Fatalf("unexpected envelope: %s", env.Payload.Raw)
	}

	w := ask(c, bus, "nope.pat.example.com.", dns.TypeAAAA, false, false)
	if w.written.Rcode != dns.RcodeNameError {
		t.Fatalf("rcode = %d, want NXDOMAIN", w.written.Rcode)
	}
	env := recvEnvelope(t, bus, 2*time.Second)
	if env == nil {
		t.Fatal("NXDOMAIN in observing zone not tapped")
	}
	raw := env.Payload.Raw
	if got := gjson.Get(raw, "_txc.dns.reply.rcode").String(); got != "NXDOMAIN" {
		t.Errorf("rcode = %q", got)
	}
	if got := gjson.Get(raw, "_txc.dns.client.transport").String(); got != "tcp" {
		t.Errorf("transport = %q, want tcp", got)
	}
	if gjson.Get(raw, "_txc.dns.client.edns_udpsize").Exists() {
		t.Errorf("edns_udpsize must be absent without OPT")
	}
	if auth := gjson.Get(raw, "_txc.dns.reply.authority").Array(); len(auth) != 1 || !strings.Contains(auth[0].String(), "SOA") {
		t.Errorf("authority = %v, want the SOA", auth)
	}
	env.ResCh <- event.Payload{Raw: "{}", Type: event.JSON}
}

// TestObserveTapSampling: --dns-observe-sample=2 taps one query in two.
func TestObserveTapSampling(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c := newTapController(t, bus, 2, 0)
	c.tap.start(context.Background())
	defer c.tap.stop()

	for i := 0; i < 4; i++ {
		ask(c, bus, "shop.pat.example.com.", dns.TypeA, true, false)
	}
	got := 0
	for {
		env := recvEnvelope(t, bus, 300*time.Millisecond)
		if env == nil {
			break
		}
		got++
		env.ResCh <- event.Payload{Raw: "{}", Type: event.JSON}
	}
	if got != 2 {
		t.Fatalf("tapped %d of 4 queries, want 2", got)
	}
}

// TestObserveTapDropsWhenFull: with no worker draining and a 1-deep
// queue, extra observations drop (counted) and the handler never blocks.
func TestObserveTapDropsWhenFull(t *testing.T) {
	bus := make(chan *event.Envelope, 8)
	c := newTapController(t, bus, 1, 1) // workers deliberately not started

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			ask(c, bus, "shop.pat.example.com.", dns.TypeA, true, false)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler blocked on a full observe queue")
	}
	if got := c.tap.Dropped(); got != 2 {
		t.Fatalf("dropped = %d, want 2", got)
	}
	if len(bus) != 0 {
		t.Fatalf("bus got %d envelopes with no worker running", len(bus))
	}
}

// TestObserveTapOffAtZero: --dns-observe-sample=0 constructs no tap, and
// the handler path stays a plain reply.
func TestObserveTapOffAtZero(t *testing.T) {
	pu := &processor.Unit{Logger: zap.NewNop(), Conf: config.Config{DNSObserveSample: 0}}
	if c := NewController(context.Background(), pu); c.tap != nil {
		t.Fatal("sample=0 must leave the tap nil")
	}
	pu.Conf.DNSObserveSample = 1
	if c := NewController(context.Background(), pu); c.tap == nil {
		t.Fatal("sample=1 must construct the tap")
	}
	// A nil tap on a controller that still has observing zones is inert.
	bus := make(chan *event.Envelope, 1)
	c := newTapController(t, bus, 1, 0)
	c.tap = nil
	w := ask(c, bus, "shop.pat.example.com.", dns.TypeA, true, false)
	if w.written == nil || len(bus) != 0 {
		t.Fatalf("nil tap: written=%v bus=%d", w.written != nil, len(bus))
	}
}

// TestSnapshotObserveFlags: zone.observe follows the tenant's active
// `_dns` stack; snap.observing is the any-zone rollup.
func TestSnapshotObserveFlags(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})
	if snap.observing {
		t.Fatal("no _dns stack anywhere, yet snap.observing is true")
	}
	if z := snap.byOrigin(testOrigin); z == nil || z.observe || z.mode != "pattern" {
		t.Fatalf("zone flags without _dns: %+v", z)
	}

	// `_dns` active but no tenants row → no slug to route on → stays off.
	seedActiveStack(t, db, testTenantID, observeStack, fixedTS)
	snap = buildOrDie(t, db, SynthConfig{})
	if snap.observing {
		t.Fatal("observe must stay off when the tenant has no routable slug")
	}

	seedTenant(t, db, testTenantID, "test")
	snap = buildOrDie(t, db, SynthConfig{})
	if !snap.observing {
		t.Fatal("snap.observing false after the tenant activated _dns")
	}
	if z := snap.byOrigin(testOrigin); z == nil || !z.observe || z.tenantSlug != "test" {
		t.Fatalf("zone flags after the tenant activated _dns: %+v", z)
	}
}

// TestBuildObserveEnvelopeShape pins the pure envelope builder: mnemonic
// fallbacks for unknown types/rcodes, presentation-format records, and
// the exact key set under _txc.dns.
func TestBuildObserveEnvelopeShape(t *testing.T) {
	z := &zone{tenantID: "tnt_x", tenantSlug: "x", origin: "x.example", originFQDN: "x.example.", mode: "manual"}
	rr, err := dns.NewRR("x.example. 30 IN TXT \"hello world\"")
	if err != nil {
		t.Fatal(err)
	}
	reply := new(dns.Msg)
	reply.Rcode = 4095 // no mnemonic in miekg/dns
	reply.Truncated = true
	reply.Answer = []dns.RR{rr}
	ob := observation{
		q:         dns.Question{Name: "X.Example.", Qtype: 65280, Qclass: dns.ClassINET},
		reply:     reply,
		clientIP:  "2001:db8::1",
		transport: "udp",
		zone:      z,
	}
	raw := buildObserveEnvelope(ob, "rid1", "node-a", time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC))

	checks := map[string]string{
		"_txc.rid":                  "rid1",
		"_ts":                       "2026-09-02T10:00:00Z",
		"_txc.dns.node":             "node-a",
		"_txc.dns.tenant":           "x",
		"_txc.dns.q.name":           "x.example.",
		"_txc.dns.q.type":           "TYPE65280",
		"_txc.dns.reply.rcode":      "RCODE4095",
		"_txc.dns.zone.mode":        "manual",
		"_txc.dns.reply.answer.0":   rr.String(),
		"_txc.dns.reply.authority":  "[]",
		"_txc.dns.reply.truncated":  "true",
		"_txc.dns.client.transport": "udp",
	}
	for path, v := range checks {
		r := gjson.Get(raw, path)
		got := r.String()
		if r.IsArray() {
			got = r.Raw
		}
		if got != v {
			t.Errorf("%s = %q, want %q", path, got, v)
		}
	}
	if gjson.Get(raw, "_txc.dns.client.edns_udpsize").Exists() {
		t.Errorf("edns_udpsize must be absent when 0")
	}
	// The record line must round-trip through the parser a stack-answered
	// lane will use — presentation format is the contract.
	back, err := dns.NewRR(gjson.Get(raw, "_txc.dns.reply.answer.0").String())
	if err != nil || back.String() != rr.String() {
		t.Errorf("answer line does not round-trip via dns.NewRR: %v / %q", err, back)
	}
	keys := []string{}
	gjson.Get(raw, "_txc.dns").ForEach(func(k, _ gjson.Result) bool { keys = append(keys, k.String()); return true })
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "client,node,phase,q,reply,tenant,zone" {
		t.Errorf("_txc.dns keys = %s", got)
	}
}
