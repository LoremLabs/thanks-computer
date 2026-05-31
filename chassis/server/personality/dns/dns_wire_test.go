package dns

import (
	"net"
	"testing"

	"github.com/miekg/dns"
)

// TestBuildReply covers the response semantics independent of the
// network: AA flag, rcodes, and the one-question / opcode guards.
func TestBuildReply(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	soaReq := new(dns.Msg)
	soaReq.SetQuestion("ops.example.com.", dns.TypeSOA)
	if m := buildReply(snap, soaReq, false); !m.Authoritative || m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
		t.Fatalf("SOA reply: aa=%v rcode=%d ans=%d", m.Authoritative, m.Rcode, len(m.Answer))
	}

	refReq := new(dns.Msg)
	refReq.SetQuestion("example.org.", dns.TypeA)
	if m := buildReply(snap, refReq, false); m.Authoritative || m.Rcode != dns.RcodeRefused {
		t.Fatalf("refused reply: aa=%v rcode=%d (must not be authoritative for a foreign zone)", m.Authoritative, m.Rcode)
	}

	nxReq := new(dns.Msg)
	nxReq.SetQuestion("nope.ops.example.com.", dns.TypeA)
	if m := buildReply(snap, nxReq, false); !m.Authoritative || m.Rcode != dns.RcodeNameError || len(m.Ns) != 1 {
		t.Fatalf("nxdomain reply: aa=%v rcode=%d ns=%d", m.Authoritative, m.Rcode, len(m.Ns))
	}

	// Exactly one question — two is refused.
	multi := new(dns.Msg)
	multi.SetQuestion("ops.example.com.", dns.TypeSOA)
	multi.Question = append(multi.Question, dns.Question{Name: "ops.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	if m := buildReply(snap, multi, false); m.Rcode != dns.RcodeRefused || m.Authoritative {
		t.Fatalf("multi-question: rcode=%d aa=%v", m.Rcode, m.Authoritative)
	}

	// Non-QUERY opcode is refused.
	notify := new(dns.Msg)
	notify.SetQuestion("ops.example.com.", dns.TypeSOA)
	notify.Opcode = dns.OpcodeNotify
	if m := buildReply(snap, notify, false); m.Rcode != dns.RcodeRefused {
		t.Fatalf("opcode notify: rcode=%d", m.Rcode)
	}

	// No snapshot yet → SERVFAIL (never panics).
	if m := buildReply(nil, soaReq, false); m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("nil snapshot: rcode=%d", m.Rcode)
	}
}

// startServer boots a real miekg/dns server bound to an ephemeral
// loopback port for `network` ("udp"|"tcp"), answering from snap via
// buildReply. Returns the dial address and a stop func.
func startServer(t *testing.T, snap *ZoneSnapshot, network string) (addr string, stop func()) {
	t.Helper()
	isUDP := network == "udp"
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		_ = w.WriteMsg(buildReply(snap, r, isUDP))
	})
	var srv *dns.Server
	switch network {
	case "udp":
		pc, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("udp listen: %v", err)
		}
		srv = &dns.Server{PacketConn: pc, Net: "udp", Handler: handler}
		addr = pc.LocalAddr().String()
	case "tcp":
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("tcp listen: %v", err)
		}
		srv = &dns.Server{Listener: ln, Net: "tcp", Handler: handler}
		addr = ln.Addr().String()
	default:
		t.Fatalf("unknown network %q", network)
	}
	ready := make(chan struct{})
	srv.NotifyStartedFunc = func() { close(ready) }
	go func() { _ = srv.ActivateAndServe() }()
	<-ready
	return addr, func() { _ = srv.Shutdown() }
}

// TestLiveServerRoundTrip is the dig-equivalent: a real query over a
// real UDP and TCP listener, asserting the authoritative answer comes
// back with the AA flag set and a foreign zone is refused.
func TestLiveServerRoundTrip(t *testing.T) {
	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})

	for _, network := range []string{"udp", "tcp"} {
		t.Run(network, func(t *testing.T) {
			addr, stop := startServer(t, snap, network)
			defer stop()
			c := &dns.Client{Net: network}

			m := new(dns.Msg)
			m.SetQuestion("ops.example.com.", dns.TypeSOA)
			resp, _, err := c.Exchange(m, addr)
			if err != nil {
				t.Fatalf("exchange SOA: %v", err)
			}
			if !resp.Authoritative {
				t.Fatalf("AA flag not set on authoritative answer")
			}
			if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
				t.Fatalf("SOA: rcode=%d ans=%d", resp.Rcode, len(resp.Answer))
			}
			soa, ok := resp.Answer[0].(*dns.SOA)
			if !ok || soa.Serial != fixedSerial {
				t.Fatalf("bad SOA answer: %v", resp.Answer[0])
			}

			// A query outside any served zone is refused, not answered.
			m2 := new(dns.Msg)
			m2.SetQuestion("example.org.", dns.TypeA)
			resp2, _, err := c.Exchange(m2, addr)
			if err != nil {
				t.Fatalf("exchange foreign: %v", err)
			}
			if resp2.Rcode != dns.RcodeRefused || resp2.Authoritative {
				t.Fatalf("foreign zone: rcode=%d aa=%v", resp2.Rcode, resp2.Authoritative)
			}
		})
	}
}
