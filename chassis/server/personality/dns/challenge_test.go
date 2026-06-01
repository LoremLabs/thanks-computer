package dns

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

const acmeOwner = "_acme-challenge.ops.example.com."

// TestMemChallengeStore covers publish/read/cleanup, dedup-refresh,
// multiple coexisting values, and the in-memory safety expiry (via an
// injected clock).
func TestMemChallengeStore(t *testing.T) {
	s := newMemChallengeStore()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	now := base
	s.now = func() time.Time { return now }

	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("empty store should yield nil, got %v", got)
	}

	s.Present(acmeOwner, "tok-A")
	s.Present(acmeOwner, "tok-B")
	s.Present(acmeOwner, "tok-A") // dedup: still two values
	if got := s.ActiveTXT(acmeOwner); len(got) != 2 {
		t.Fatalf("want 2 values, got %v", got)
	}

	// Case-insensitive owner match.
	if got := s.ActiveTXT("_ACME-Challenge.OPS.example.com."); len(got) != 2 {
		t.Fatalf("owner match should be case-insensitive, got %v", got)
	}

	// CleanUp one value leaves the other.
	s.CleanUp(acmeOwner, "tok-A")
	if got := s.ActiveTXT(acmeOwner); len(got) != 1 || got[0] != "tok-B" {
		t.Fatalf("after cleanup want [tok-B], got %v", got)
	}

	// CleanUp is idempotent.
	s.CleanUp(acmeOwner, "tok-A")
	s.CleanUp(acmeOwner, "tok-B")
	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("after full cleanup want nil, got %v", got)
	}

	// Safety expiry: a value not explicitly cleaned vanishes past the TTL.
	s.Present(acmeOwner, "tok-C")
	now = base.Add(challengeStoreTTL + time.Second)
	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("expired value should be gone, got %v", got)
	}
}

// TestAnswerChallenge covers the dispatch decision in isolation: it
// answers ONLY a TXT for `_acme-challenge.<served-zone>` with a live
// value, and returns nil (fall through to the snapshot) in every other
// case.
func TestAnswerChallenge(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	store := newMemChallengeStore()
	c := &DNSController{challenges: store}
	c.snap.Store(snap)

	ask := func(name string, qtype uint16) *dns.Msg {
		req := new(dns.Msg)
		req.SetQuestion(name, qtype)
		return c.answerChallenge(req, false)
	}

	// No active challenge yet → fall through.
	if m := ask(acmeOwner, dns.TypeTXT); m != nil {
		t.Fatalf("no challenge present should fall through, got %v", m)
	}

	store.Present(acmeOwner, "the-key-authz")

	// Served challenge → authoritative NOERROR + the TXT.
	m := ask(acmeOwner, dns.TypeTXT)
	if m == nil || !m.Authoritative || m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
		t.Fatalf("challenge answer: %v", m)
	}
	txt, ok := m.Answer[0].(*dns.TXT)
	if !ok || len(txt.Txt) != 1 || txt.Txt[0] != "the-key-authz" || txt.Hdr.Ttl != challengeRecordTTL {
		t.Fatalf("bad TXT answer: %#v", m.Answer[0])
	}

	// Non-TXT for the challenge name → fall through (snapshot decides).
	if m := ask(acmeOwner, dns.TypeA); m != nil {
		t.Fatalf("non-TXT should fall through, got %v", m)
	}

	// `_acme-challenge` under a zone we DON'T serve → fall through (→ REFUSED).
	store.Present("_acme-challenge.example.org.", "x")
	if m := ask("_acme-challenge.example.org.", dns.TypeTXT); m != nil {
		t.Fatalf("foreign-zone challenge should fall through, got %v", m)
	}

	// A normal TXT name (not a challenge) → fall through to the snapshot.
	if m := ask("ops.example.com.", dns.TypeTXT); m != nil {
		t.Fatalf("non-challenge TXT should fall through, got %v", m)
	}

	// After CleanUp the challenge name falls through again.
	store.CleanUp(acmeOwner, "the-key-authz")
	if m := ask(acmeOwner, dns.TypeTXT); m != nil {
		t.Fatalf("after cleanup should fall through, got %v", m)
	}
}

// TestChallengeWireRoundTrip is the dig-equivalent for the overlay: a real
// UDP query through the controller's actual dispatch (answerChallenge →
// buildReply). The challenge resolves while present, and the same name
// returns NXDOMAIN once cleaned up — proving the record is transient and
// served outside the snapshot.
func TestChallengeWireRoundTrip(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	store := newMemChallengeStore()
	c := &DNSController{challenges: store}
	c.snap.Store(snap)

	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := c.answerChallenge(r, true)
		if m == nil {
			m = buildReply(c.snap.Load(), r, true)
		}
		_ = w.WriteMsg(m)
	})
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("udp listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Net: "udp", Handler: handler}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = srv.ActivateAndServe() }()
	<-ready
	defer func() { _ = srv.Shutdown() }()

	addr := pc.LocalAddr().String()
	cl := &dns.Client{Net: "udp"}
	query := func() *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion(acmeOwner, dns.TypeTXT)
		resp, _, qerr := cl.Exchange(m, addr)
		if qerr != nil {
			t.Fatalf("exchange: %v", qerr)
		}
		return resp
	}

	// Before publishing: name doesn't exist in the zone → NXDOMAIN.
	if resp := query(); resp.Rcode != dns.RcodeNameError {
		t.Fatalf("pre-publish want NXDOMAIN, got rcode=%d", resp.Rcode)
	}

	store.Present(acmeOwner, "live-token")
	resp := query()
	if resp.Rcode != dns.RcodeSuccess || !resp.Authoritative || len(resp.Answer) != 1 {
		t.Fatalf("published challenge: rcode=%d aa=%v ans=%d", resp.Rcode, resp.Authoritative, len(resp.Answer))
	}
	if txt, ok := resp.Answer[0].(*dns.TXT); !ok || txt.Txt[0] != "live-token" {
		t.Fatalf("bad answer: %v", resp.Answer[0])
	}

	store.CleanUp(acmeOwner, "live-token")
	if resp := query(); resp.Rcode != dns.RcodeNameError {
		t.Fatalf("post-cleanup want NXDOMAIN, got rcode=%d", resp.Rcode)
	}
}
