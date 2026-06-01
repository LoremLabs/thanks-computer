package dns

import (
	"encoding/base64"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/processor"
)

const (
	testTSIGKey    = "acme-updater."
	testChalRR     = `_acme-challenge.ops.example.com. 60 IN TXT "the-token"`
	testChalOwner  = "_acme-challenge.ops.example.com."
	testChalValue  = "the-token"
	testChalNonChl = `www.ops.example.com. 60 IN TXT "nope"`
)

// newUpdateServer starts a real UDP server backed by the controller's
// actual makeHandler (so OpcodeUpdate → handleUpdate is exercised end to
// end) with TSIG configured. Returns dial addr + the controller + stop.
func newUpdateServer(t *testing.T, secret string) (string, *DNSController, func()) {
	t.Helper()
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	c := &DNSController{
		pu:          &processor.Unit{Logger: zap.NewNop()},
		challenges:  newMemChallengeStore(),
		tsigKeyName: testTSIGKey,
		tsigSecret:  secret,
	}
	c.snap.Store(snap)

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	srv := &dns.Server{
		PacketConn:    pc,
		Net:           "udp",
		Handler:       c.makeHandler(true),
		TsigSecret:    map[string]string{testTSIGKey: secret},
		MsgAcceptFunc: acceptDynamicUpdate,
	}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = srv.ActivateAndServe() }()
	<-ready
	return pc.LocalAddr().String(), c, func() { _ = srv.Shutdown() }
}

func TestRFC2136UpdateReceiver(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("supersecrettsigkey-0123456789abc"))
	addr, _, stop := newUpdateServer(t, secret)
	defer stop()

	signed := &dns.Client{Net: "udp", TsigSecret: map[string]string{testTSIGKey: secret}}
	plain := &dns.Client{Net: "udp"}

	sendUpdate := func(cl *dns.Client, build func(*dns.Msg), sign bool) *dns.Msg {
		t.Helper()
		u := new(dns.Msg)
		u.SetUpdate("ops.example.com.")
		build(u)
		if sign {
			u.SetTsig(testTSIGKey, dns.HmacSHA256, tsigFudgeSeconds, time.Now().Unix())
		}
		resp, _, err := cl.Exchange(u, addr)
		if err != nil {
			t.Fatalf("update exchange: %v", err)
		}
		return resp
	}

	queryTXT := func() *dns.Msg {
		t.Helper()
		m := new(dns.Msg)
		m.SetQuestion(testChalOwner, dns.TypeTXT)
		resp, _, err := plain.Exchange(m, addr)
		if err != nil {
			t.Fatalf("txt query: %v", err)
		}
		return resp
	}

	mustRR := func(s string) dns.RR {
		rr, err := dns.NewRR(s)
		if err != nil {
			t.Fatalf("NewRR(%q): %v", s, err)
		}
		return rr
	}

	// Signed insert → SUCCESS, then the challenge resolves.
	if resp := sendUpdate(signed, func(u *dns.Msg) { u.Insert([]dns.RR{mustRR(testChalRR)}) }, true); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("signed insert rcode=%d", resp.Rcode)
	}
	if resp := queryTXT(); resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("challenge not served after insert: rcode=%d ans=%d", resp.Rcode, len(resp.Answer))
	} else if txt := resp.Answer[0].(*dns.TXT); txt.Txt[0] != testChalValue {
		t.Fatalf("served value=%q want %q", txt.Txt[0], testChalValue)
	}

	// Signed individual remove (Class NONE) → SUCCESS, then NXDOMAIN.
	if resp := sendUpdate(signed, func(u *dns.Msg) { u.Remove([]dns.RR{mustRR(testChalRR)}) }, true); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("signed remove rcode=%d", resp.Rcode)
	}
	if resp := queryTXT(); resp.Rcode != dns.RcodeNameError {
		t.Fatalf("challenge still served after remove: rcode=%d", resp.Rcode)
	}

	// Signed RemoveRRset (Class ANY) also clears.
	sendUpdate(signed, func(u *dns.Msg) { u.Insert([]dns.RR{mustRR(testChalRR)}) }, true)
	if resp := sendUpdate(signed, func(u *dns.Msg) { u.RemoveRRset([]dns.RR{mustRR(testChalRR)}) }, true); resp.Rcode != dns.RcodeSuccess {
		t.Fatalf("RemoveRRset rcode=%d", resp.Rcode)
	}
	if resp := queryTXT(); resp.Rcode != dns.RcodeNameError {
		t.Fatalf("challenge still served after RemoveRRset: rcode=%d", resp.Rcode)
	}

	// Unsigned UPDATE → NOTAUTH (and nothing written).
	if resp := sendUpdate(plain, func(u *dns.Msg) { u.Insert([]dns.RR{mustRR(testChalRR)}) }, false); resp.Rcode != dns.RcodeNotAuth {
		t.Fatalf("unsigned update rcode=%d want NOTAUTH", resp.Rcode)
	}
	if resp := queryTXT(); resp.Rcode != dns.RcodeNameError {
		t.Fatalf("unsigned update should not have written: rcode=%d", resp.Rcode)
	}

	// Signed but NON-challenge name → REFUSED (policy), nothing written.
	if resp := sendUpdate(signed, func(u *dns.Msg) { u.Insert([]dns.RR{mustRR(testChalNonChl)}) }, true); resp.Rcode != dns.RcodeRefused {
		t.Fatalf("non-challenge update rcode=%d want REFUSED", resp.Rcode)
	}

	// Signed UPDATE for a zone we don't serve → NOTZONE.
	foreign := func() *dns.Msg {
		u := new(dns.Msg)
		u.SetUpdate("example.org.")
		u.Insert([]dns.RR{mustRR(`_acme-challenge.example.org. 60 IN TXT "x"`)})
		u.SetTsig(testTSIGKey, dns.HmacSHA256, tsigFudgeSeconds, time.Now().Unix())
		resp, _, err := signed.Exchange(u, addr)
		if err != nil {
			t.Fatalf("foreign update exchange: %v", err)
		}
		return resp
	}()
	if foreign.Rcode != dns.RcodeNotZone {
		t.Fatalf("foreign-zone update rcode=%d want NOTZONE", foreign.Rcode)
	}
}

// TestRFC2136UpdateDisabled confirms that with no TSIG key configured the
// receiver isn't wired in: the default accept func NOTIMPs the UPDATE at the
// server level (it never reaches our handler).
func TestRFC2136UpdateDisabled(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	c := &DNSController{
		pu:         &processor.Unit{Logger: zap.NewNop()},
		challenges: newMemChallengeStore(),
		// no tsigKeyName/tsigSecret ⇒ updates disabled
	}
	c.snap.Store(snap)
	if c.updatesEnabled() {
		t.Fatal("updates should be disabled with no key")
	}

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Net: "udp", Handler: c.makeHandler(true)}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = srv.ActivateAndServe() }()
	<-ready
	defer func() { _ = srv.Shutdown() }()

	u := new(dns.Msg)
	u.SetUpdate("ops.example.com.")
	rr, _ := dns.NewRR(testChalRR)
	u.Insert([]dns.RR{rr})
	resp, _, err := (&dns.Client{Net: "udp"}).Exchange(u, pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if resp.Rcode != dns.RcodeNotImplemented {
		t.Fatalf("disabled receiver rcode=%d want NOTIMP", resp.Rcode)
	}
}
