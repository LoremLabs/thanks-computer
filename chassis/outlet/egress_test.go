package outlet_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/egress"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/outlet/outlettest"
)

type socksServer struct {
	ln  net.Listener
	srv *outlettest.SOCKS5Server
}

func newSocksServer(t *testing.T) *socksServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	s := &socksServer{ln: ln, srv: &outlettest.SOCKS5Server{}}
	go s.srv.Serve(ln)
	return s
}

func (s *socksServer) Targets() []string { return s.srv.Targets() }

// echoTarget is what the relay connects to: it echoes one line.
func echoTarget(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				n, _ := c.Read(buf)
				_, _ = c.Write(buf[:n])
			}()
		}
	}()
	return ln
}

func roundTrip(t *testing.T, dial func(context.Context, string, string) (net.Conn, error), address string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		t.Fatalf("dial %s: %v", address, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return string(buf[:n])
}

func TestDialFuncDirect(t *testing.T) {
	target := echoTarget(t)
	dial := outlet.DialFunc(outlet.EgressParams{}, nil, time.Second)
	if got := roundTrip(t, dial, target.Addr().String()); got != "ping\n" {
		t.Fatalf("echo = %q", got)
	}
	guard, _ := egress.Open("private", egress.Config{})
	dial = outlet.DialFunc(outlet.EgressParams{Mode: outlet.EgressDirect}, guard, time.Second)
	if _, err := dial(context.Background(), "tcp", target.Addr().String()); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("direct dial to loopback under private must be refused: %v", err)
	}
}

func TestDialFuncRelay(t *testing.T) {
	relay := newSocksServer(t)
	target := echoTarget(t)
	p := outlet.EgressParams{Mode: outlet.EgressRelay, Relays: []string{relay.ln.Addr().String()}}

	dial := outlet.DialFunc(p, nil, time.Second)
	if got := roundTrip(t, dial, target.Addr().String()); got != "ping\n" {
		t.Fatalf("echo through relay = %q", got)
	}
	if ts := relay.Targets(); len(ts) != 1 || ts[0] != target.Addr().String() {
		t.Fatalf("relay saw %v, want the exact ip:port", ts)
	}

	// A hostname is resolved on this side; the relay is handed an IP.
	_, port, _ := net.SplitHostPort(target.Addr().String())
	if got := roundTrip(t, dial, "localhost:"+port); got != "ping\n" {
		t.Fatalf("echo via hostname = %q", got)
	}
	for _, seen := range relay.Targets() {
		if strings.HasPrefix(seen, "localhost") {
			t.Fatalf("relay must never receive a hostname: %v", relay.Targets())
		}
	}

	// The guard checks the destination before any relay is asked.
	guard, _ := egress.Open("private", egress.Config{})
	before := len(relay.Targets())
	gdial := outlet.DialFunc(p, guard, time.Second)
	if _, err := gdial(context.Background(), "tcp", target.Addr().String()); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("guarded relay dial to loopback must be refused: %v", err)
	}
	if len(relay.Targets()) != before {
		t.Fatal("a refused destination must not reach the relay")
	}
}

func TestDialFuncRelayFailoverAndUnconfigured(t *testing.T) {
	target := echoTarget(t)
	// A relay address nothing listens on, then a working one.
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	_ = dead.Close()
	relay := newSocksServer(t)
	p := outlet.EgressParams{Mode: outlet.EgressRelay, Relays: []string{deadAddr, relay.ln.Addr().String()}}
	dial := outlet.DialFunc(p, nil, time.Second)
	if got := roundTrip(t, dial, target.Addr().String()); got != "ping\n" {
		t.Fatalf("failover echo = %q", got)
	}
	if len(relay.Targets()) != 1 {
		t.Fatalf("second relay should have served the dial: %v", relay.Targets())
	}

	none := outlet.DialFunc(outlet.EgressParams{Mode: outlet.EgressRelay}, nil, time.Second)
	if _, err := none(context.Background(), "tcp", target.Addr().String()); !errors.Is(err, outlet.ErrNoRelay) {
		t.Fatalf("no relays configured: %v", err)
	}
}
