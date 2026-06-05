package mailmap

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// fakeAccepter implements both MailResolver and MailDomainAccepter,
// accepting a fixed set of domains.
type fakeAccepter struct{ accept map[string]bool }

func (f fakeAccepter) ResolveRecipient(rcpt, listener string) (ingress.RouteTarget, bool) {
	if at := strings.LastIndex(rcpt, "@"); at >= 0 {
		return f.domain(rcpt[at+1:])
	}
	return ingress.RouteTarget{}, false
}
func (f fakeAccepter) AcceptMailDomain(domain string) (ingress.RouteTarget, bool) {
	return f.domain(domain)
}
func (f fakeAccepter) domain(d string) (ingress.RouteTarget, bool) {
	if f.accept[strings.ToLower(d)] {
		return ingress.RouteTarget{Tenant: "t", Stack: "t/_mail"}, true
	}
	return ingress.RouteTarget{}, false
}

// probeOnlyResolver implements MailResolver but NOT MailDomainAccepter,
// exercising the controller's ResolveRecipient-probe fallback.
type probeOnlyResolver struct{ accept map[string]bool }

func (p probeOnlyResolver) ResolveRecipient(rcpt, listener string) (ingress.RouteTarget, bool) {
	if at := strings.LastIndex(rcpt, "@"); at >= 0 && p.accept[strings.ToLower(rcpt[at+1:])] {
		return ingress.RouteTarget{Tenant: "t", Stack: "t/_mail"}, true
	}
	return ingress.RouteTarget{}, false
}

func newTestController(t *testing.T, resolver ingress.MailResolver) (string, func()) {
	t.Helper()
	pu := &processor.Unit{
		Conf: config.Config{
			Personalities:      "mailmap",
			MailMapListenAddrs: []string{"127.0.0.1:0"}, // ephemeral
			MailMapReadTimeout: "5s",
		},
		Logger: zap.NewNop(), // Nop is Fatal-safe (no os.Exit); ephemeral bind never conflicts
	}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu, resolver)
	ctrl.Start()
	addrs := ctrl.boundAddrs()
	if len(addrs) == 0 {
		cancel()
		t.Fatal("mailmap controller did not bind")
	}
	return addrs[0], func() { ctrl.Stop(); cancel() }
}

func TestMailMapTCPTable(t *testing.T) {
	addr, stop := newTestController(t, fakeAccepter{accept: map[string]bool{
		"good.example":               true,
		"sub.stacks.thanks.computer": true,
	}})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	cases := []struct{ in, want string }{
		{"get good.example", "200 OK"},                      // accepted
		{"get GOOD.EXAMPLE", "200 OK"},                      // case-insensitive
		{"get sub.stacks.thanks.computer", "200 OK"},        // hosted subdomain
		{"get bad.example", "500 not a hosted mail domain"}, // open-relay guard
		{"get ", "500 empty key"},                           // empty key → reject
		{"bogus request", "400 only get is supported"},      // malformed → temp/defer
		{"get good.example", "200 OK"},                      // connection still usable after errors
	}
	for _, tc := range cases {
		if _, err := conn.Write([]byte(tc.in + "\n")); err != nil {
			t.Fatalf("write %q: %v", tc.in, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read for %q: %v", tc.in, err)
		}
		if got := strings.TrimRight(got, "\r\n"); got != tc.want {
			t.Errorf("query %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMailMapProbeFallback verifies the controller still answers when the
// resolver only implements MailResolver (no MailDomainAccepter) by probing
// ResolveRecipient with a synthetic local-part.
func TestMailMapProbeFallback(t *testing.T) {
	addr, stop := newTestController(t, probeOnlyResolver{accept: map[string]bool{"good.example": true}})
	defer stop()

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	r := bufio.NewReader(conn)

	for _, tc := range []struct{ in, want string }{
		{"get good.example", "200 OK"},
		{"get bad.example", "500 not a hosted mail domain"},
	} {
		if _, err := conn.Write([]byte(tc.in + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		got, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got := strings.TrimRight(got, "\r\n"); got != tc.want {
			t.Errorf("query %q = %q, want %q", tc.in, got, tc.want)
		}
	}
}
