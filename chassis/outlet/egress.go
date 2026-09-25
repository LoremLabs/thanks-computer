package outlet

import (
	"context"
	"errors"
	"net"
	"time"

	"golang.org/x/net/proxy"

	"github.com/loremlabs/thanks-computer/chassis/egress"
)

// Egress modes. A declaration's `egress` picks one; an omitted field takes
// the node's default (--outlet-egress).
const (
	// EgressDirect dials the destination from this node's own address.
	EgressDirect = "direct"
	// EgressRelay asks one of the node's relays (--outlet-egress-relays)
	// to make the connection, so it originates from the relay's address —
	// the one a customer allowlists. Only the dial changes: the pool, the
	// statement, TLS and every limit stay on the chassis.
	EgressRelay = "relay"
)

// ValidEgress reports whether s is an egress mode a declaration may name.
func ValidEgress(s string) bool { return s == "" || s == EgressDirect || s == EgressRelay }

// ContextDialer reaches a relay. nil means the host network; a WireGuard
// tunnel living in this process (chassis/egress/wgtunnel) is the other one.
type ContextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// EgressConfig is the node's egress default and its relays.
type EgressConfig struct {
	// Default is what a declaration without `egress` gets: EgressDirect
	// or EgressRelay. Empty means direct.
	Default string
	// Relays are SOCKS5 relays as ip:port on the fleet's private network
	// (or on the tunnel, when Forward is set), tried in order. Empty means
	// relay egress is unavailable on this node.
	Relays []string
	// Forward is how relays are reached; nil means the host network.
	Forward ContextDialer
}

// EgressParams is the egress a driver is handed for one pool.
type EgressParams struct {
	Mode    string
	Relays  []string
	Forward ContextDialer
}

// ErrNoRelay is the dial error for `egress: relay` on a node that has no
// relays configured. Run-time state, so it reaches the op as data
// (CodeConnectFailed), never a deploy error.
var ErrNoRelay = errors.New("outlet: relay egress is not configured on this node")

// DialFunc returns the dial function a driver installs on its client.
//
// Direct dials from this node, with the egress guard checking every
// resolved address before the socket connects. Relay dials ask a relay to
// connect instead: the destination is checked against the same guard
// first — drivers resolve the DSN host themselves and hand over ip:port,
// and a hostname is resolved here — and the relay is then asked for that
// exact address, so it can never arrive somewhere the chassis didn't
// check. The relay itself is reached with a plain dialer: it lives on the
// fleet's private network, which the guard would otherwise refuse. SOCKS
// is involved only while the connection is established; afterwards the
// socket is an ordinary TCP stream, and TLS runs end to end over it.
func DialFunc(p EgressParams, guard egress.Guard, timeout time.Duration) func(ctx context.Context, network, address string) (net.Conn, error) {
	if p.Mode == EgressRelay {
		return relayDial(p.Relays, guard, timeout, p.Forward)
	}
	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if guard != nil {
		d.Control = egress.DialControl(guard)
	}
	return d.DialContext
}

func relayDial(relays []string, guard egress.Guard, timeout time.Duration, via ContextDialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	var forward proxy.Dialer = &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if via != nil {
		forward = forwardDialer{d: via, timeout: timeout}
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if len(relays) == 0 {
			return nil, ErrNoRelay
		}
		addrs, err := destinationAddrs(ctx, address)
		if err != nil {
			return nil, err
		}
		var last error
		for _, addr := range addrs {
			if guard != nil {
				if gerr := guard.CheckAddr(network, addr); gerr != nil {
					last = gerr
					continue
				}
			}
			for _, relay := range relays {
				sd, serr := proxy.SOCKS5("tcp", relay, nil, forward)
				if serr != nil {
					last = serr
					continue
				}
				conn, derr := sd.(proxy.ContextDialer).DialContext(ctx, network, addr)
				if derr == nil {
					return conn, nil
				}
				last = derr
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
		}
		if last == nil {
			last = errors.New("outlet: destination has no address")
		}
		return nil, last
	}
}

// forwardDialer adapts a ContextDialer to what the SOCKS client wants.
type forwardDialer struct {
	d       ContextDialer
	timeout time.Duration
}

func (f forwardDialer) Dial(network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	return f.d.DialContext(ctx, network, address)
}

func (f forwardDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	return f.d.DialContext(ctx, network, address)
}

// destinationAddrs returns the ip:port candidates for an address: the
// address itself when its host is an IP literal, else each resolved IP.
func destinationAddrs(ctx context.Context, address string) ([]string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(host) != nil {
		return []string{address}, nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.JoinHostPort(ip.IP.String(), port))
	}
	return out, nil
}
