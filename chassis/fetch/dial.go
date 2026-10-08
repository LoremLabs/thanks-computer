package fetch

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"syscall"

	"github.com/loremlabs/thanks-computer/chassis/egress"
)

// Resolver looks a host up. net.DefaultResolver satisfies it; tests
// substitute their own answers.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DialFunc connects to an address the guard has already approved: always
// an "ip:port", never a hostname.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// deniedError marks a dial the egress guard refused, so it maps to
// destination_denied however the transport wraps it.
type deniedError struct{}

func (deniedError) Error() string { return "fetch: destination denied by egress policy" }

// dnsError marks a lookup that failed or found nothing.
type dnsError struct{}

func (dnsError) Error() string { return "fetch: dns lookup failed" }

// dialer resolves, filters every candidate through the guard, and dials
// an approved address. The transport never resolves anything itself, so
// the address checked is the address connected (DNS rebinding cannot
// slip a second lookup in), and the connection's own socket is checked
// again by the guard's Control hook in the default DialFunc.
type dialer struct {
	guard    egress.Guard
	resolver Resolver
	dial     DialFunc
}

func (d *dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if !allowedPort(port) {
		return nil, deniedError{}
	}
	var addrs []netip.Addr
	if ip, perr := netip.ParseAddr(host); perr == nil {
		addrs = []netip.Addr{ip}
	} else {
		addrs, err = d.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(addrs) == 0 {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, dnsError{}
		}
	}
	var lastErr error
	approved := 0
	for _, a := range addrs {
		a = a.Unmap()
		target := net.JoinHostPort(a.String(), port)
		netw := "tcp6"
		if a.Is4() {
			netw = "tcp4"
		}
		if d.guard.CheckAddr(netw, target) != nil {
			continue
		}
		approved++
		conn, err := d.dial(ctx, netw, target)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if approved == 0 {
		return nil, deniedError{}
	}
	return nil, lastErr
}

// guardedDial is the production DialFunc: a plain TCP dial whose Control
// hook asks the guard once more about the socket's actual address.
func guardedDial(g egress.Guard) DialFunc {
	nd := &net.Dialer{
		Control: func(network, address string, _ syscall.RawConn) error {
			if g.CheckAddr(network, address) != nil {
				return deniedError{}
			}
			return nil
		},
	}
	return nd.DialContext
}

func isDenied(err error) bool {
	var d deniedError
	return errors.As(err, &d)
}

func isDNS(err error) bool {
	var d dnsError
	if errors.As(err, &d) {
		return true
	}
	var ne *net.DNSError
	return errors.As(err, &ne)
}
