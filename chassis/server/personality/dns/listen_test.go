package dns

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// TestParseListenSpec pins the --dns-listen-addrs grammar: bare = both
// transports (the historical behaviour), an exact udp:/tcp: head = that
// transport only, anything else before the first colon is a hostname.
func TestParseListenSpec(t *testing.T) {
	cases := []struct {
		in      string
		addr    string
		udp     bool
		tcp     bool
		wantErr bool
	}{
		{":5354", ":5354", true, true, false},
		{"127.0.0.1:53", "127.0.0.1:53", true, true, false},
		{"udp:127.0.0.1:53", "127.0.0.1:53", true, false, false},
		{"tcp:0.0.0.0:53", "0.0.0.0:53", false, true, false},
		{"udp:fly-global-services:53", "fly-global-services:53", true, false, false},
		{"[::1]:53", "[::1]:53", true, true, false},
		{"udp:[::1]:53", "[::1]:53", true, false, false},
		{"example.com:53", "example.com:53", true, true, false},
		{"  tcp:127.0.0.1:53  ", "127.0.0.1:53", false, true, false},
		// Documented degrade: an unknown head is a hostname (fails at bind).
		{"upd:53", "upd:53", true, true, false},
		// Only the FIRST prefix is a transport; the rest is host "tcp".
		{"udp:tcp:53", "tcp:53", true, false, false},
		// Errors — in particular `udp:` alone, which used to bind a random
		// port on every interface.
		{"udp:", "", false, false, true},
		{"tcp:", "", false, false, true},
		{"", "", false, false, true},
		{"53", "", false, false, true},
		{"UDP:127.0.0.1:53", "", false, false, true}, // exact-case head only
	}
	for _, c := range cases {
		got, err := parseListenSpec(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseListenSpec(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if got.addr != c.addr || got.udp != c.udp || got.tcp != c.tcp {
			t.Errorf("parseListenSpec(%q) = %+v, want addr=%q udp=%v tcp=%v", c.in, got, c.addr, c.udp, c.tcp)
		}
	}
}

// startController builds a controller directly (every dependency Start()
// touches is nil-safe), starts it on the given entries, and waits until
// each bound server answers — miekg's Shutdown errors on a server that has
// not started yet, so a probe is what makes the Stop() in cleanup sound.
func startController(t *testing.T, logger *zap.Logger, addrs []string) *DNSController {
	t.Helper()
	c := &DNSController{
		pu: &processor.Unit{
			Logger: logger,
			Conf:   config.Config{Personalities: "dns", DNSListenAddrs: addrs},
		},
	}
	c.Start()
	for _, srv := range c.servers {
		probe(t, srv)
	}
	t.Cleanup(c.Stop)
	return c
}

// probe exchanges one query with srv, retrying until the server goroutine
// has activated. The reply's content is irrelevant (an empty snapshot); a
// reply at all is what proves the transport is bound and served.
func probe(t *testing.T, srv *dns.Server) {
	t.Helper()
	var addr string
	if srv.PacketConn != nil {
		addr = srv.PacketConn.LocalAddr().String()
	} else {
		addr = srv.Listener.Addr().String()
	}
	cl := &dns.Client{Net: srv.Net, Timeout: 500 * time.Millisecond}
	m := new(dns.Msg)
	m.SetQuestion("probe.example.com.", dns.TypeA)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := cl.Exchange(m, addr); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("%s %s never answered: %v", srv.Net, addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func nets(c *DNSController) []string {
	var out []string
	for _, s := range c.servers {
		out = append(out, s.Net)
	}
	return out
}

// TestStartBindsPerProtocol: a udp: entry binds one UDP server, a tcp:
// entry one TCP server, a bare entry both — and each one actually serves.
func TestStartBindsPerProtocol(t *testing.T) {
	cases := []struct {
		addrs []string
		want  []string
	}{
		{[]string{"udp:127.0.0.1:0"}, []string{"udp"}},
		{[]string{"tcp:127.0.0.1:0"}, []string{"tcp"}},
		{[]string{"127.0.0.1:0"}, []string{"udp", "tcp"}},
		{[]string{"udp:127.0.0.1:0", "tcp:127.0.0.1:0"}, []string{"udp", "tcp"}},
	}
	for _, c := range cases {
		ctrl := startController(t, zap.NewNop(), c.addrs)
		got := nets(ctrl)
		if len(got) != len(c.want) {
			t.Fatalf("%v: bound %v, want %v", c.addrs, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%v: bound %v, want %v", c.addrs, got, c.want)
			}
		}
	}
}

// TestNewServerAppliesTSIG: the TSIG secret and the UPDATE-accepting
// MsgAcceptFunc land on whichever transport is built, and on neither when
// the receiver is off. Tested on the helper directly — once a server is
// serving, miekg's own init() writes MsgAcceptFunc, so reading it after
// Start() is a data race and asserts the wrong thing.
func TestNewServerAppliesTSIG(t *testing.T) {
	on := &DNSController{tsigKeyName: "acme-test.", tsigSecret: "c2VjcmV0"}
	off := &DNSController{}

	for _, tc := range []struct {
		name string
		mk   func(c *DNSController) *dns.Server
		net  string
	}{
		{"udp", func(c *DNSController) *dns.Server { return c.newServer(newUDPConn(t), nil) }, "udp"},
		{"tcp", func(c *DNSController) *dns.Server { return c.newServer(nil, newTCPListener(t)) }, "tcp"},
	} {
		srv := tc.mk(on)
		if srv.Net != tc.net || srv.Handler == nil {
			t.Fatalf("%s: server shape wrong: net=%q handler=%v", tc.name, srv.Net, srv.Handler != nil)
		}
		if srv.TsigSecret == nil || srv.TsigSecret["acme-test."] == "" || srv.MsgAcceptFunc == nil {
			t.Fatalf("%s: tsig not applied: secret=%v accept=%v", tc.name, srv.TsigSecret, srv.MsgAcceptFunc != nil)
		}
		plain := tc.mk(off)
		if plain.TsigSecret != nil || plain.MsgAcceptFunc != nil {
			t.Fatalf("%s: receiver must stay off without a key", tc.name)
		}
	}
}

func newUDPConn(t *testing.T) net.PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc
}

func newTCPListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// TestStartWarnsOnMissingTransport: a one-transport bind warns (DNS needs
// both to reach the head), a bare bind does not.
func TestStartWarnsOnMissingTransport(t *testing.T) {
	const msg = "one transport only"

	core, logs := observer.New(zapcore.WarnLevel)
	startController(t, zap.New(core), []string{"udp:127.0.0.1:0"})
	if got := logs.FilterMessageSnippet(msg).Len(); got != 1 {
		t.Fatalf("udp-only bind: want 1 warning, got %d: %v", got, logs.All())
	}

	core, logs = observer.New(zapcore.WarnLevel)
	startController(t, zap.New(core), []string{"127.0.0.1:0"})
	if got := logs.FilterMessageSnippet(msg).Len(); got != 0 {
		t.Fatalf("both-transport bind: want no warning, got %v", logs.All())
	}
}
