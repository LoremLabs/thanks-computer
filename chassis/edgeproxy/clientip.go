package edgeproxy

import (
	"net"
	"net/http"
	"strings"
)

// The HTTP half of the trusted-edge contract.
//
// A TCP head learns the client's address from the PROXY header (Wrap, Read).
// An HTTP head sits behind proxies that speak HTTP, and they say it in
// X-Forwarded-For: each proxy appends the address of the peer it accepted
// the request from, so the header reads, left to right, from the client
// towards us:
//
//	X-Forwarded-For: 203.0.113.7, fdaa::edge, fdaa::lb      peer = fdaa::lb2
//
// Anyone can send that header, so it is evidence only when the socket peer
// is one of the operator's proxies — the same boundary the PROXY header
// has. And only its right-hand end is: a proxy appends, it does not vouch
// for what it was handed. So the client is the first address, reading from
// the RIGHT, that is not itself a trusted proxy. Whatever a client typed
// into the header sits to the left of that and is never reached.

// Clients resolves the client address of HTTP requests for one trust list.
// The zero value and nil trust nobody: the client is the socket peer.
type Clients struct {
	trusted []*net.IPNet
}

// NewClients builds a resolver from operator entries (--web-trusted-proxies;
// see ParseTrusted for the forms). Unparseable entries come back in bad and
// are not trusted.
func NewClients(entries []string) (c *Clients, bad []string) {
	nets, bad := ParseTrusted(entries)
	return &Clients{trusted: nets}, bad
}

// Trusts reports whether any proxy is trusted: whether IP can ever answer
// with something other than the socket peer.
func (c *Clients) Trusts() bool { return c != nil && len(c.trusted) > 0 }

// Networks lists the trusted proxies as CIDRs — what was parsed, not what
// was asked for — for a start-up log line.
func (c *Clients) Networks() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.trusted))
	for _, n := range c.trusted {
		out = append(out, n.String())
	}
	return out
}

// IP returns the address of the client that sent r, without a port.
//
//   - The peer is not a trusted proxy (or none are configured): the peer.
//     X-Forwarded-For is not read.
//   - The peer is a trusted proxy: the first X-Forwarded-For address from
//     the right that is not a trusted proxy.
//   - Every address in it is a trusted proxy — the request began inside,
//     say one app calling another: the leftmost, the one that began it.
//   - The header is missing, or the address a proxy recorded cannot be read
//     ("unknown", an obfuscated node name): the peer. Nothing to the left
//     of an unreadable hop is trustworthy, and the proxy's own address is
//     the conservative key for a throttle — shared, never forgeable.
func (c *Clients) IP(r *http.Request) string {
	peer := hostOnly(r.RemoteAddr)
	if !c.Trusts() {
		return peer
	}
	if ip := net.ParseIP(peer); ip == nil || !c.contains(ip) {
		return peer
	}
	inside := ""
	values := r.Header.Values("X-Forwarded-For")
	for i := len(values) - 1; i >= 0; i-- {
		hops := strings.Split(values[i], ",")
		for j := len(hops) - 1; j >= 0; j-- {
			ip := net.ParseIP(hostOnly(strings.TrimSpace(hops[j])))
			if ip == nil {
				return peer
			}
			if !c.contains(ip) {
				return ip.String()
			}
			inside = ip.String()
		}
	}
	if inside != "" {
		return inside
	}
	return peer
}

func (c *Clients) contains(ip net.IP) bool {
	for _, n := range c.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// hostOnly strips a port, IPv6 brackets and a zone from an address:
// "1.2.3.4:80", "[::1]:80", "[::1]", "fe80::1%en0" and a bare address all
// come back as the address. A bare IPv6 address is full of colons and has
// no port, which is why this is not just net.SplitHostPort.
func hostOnly(a string) string {
	if h, _, err := net.SplitHostPort(a); err == nil {
		a = h
	}
	a = strings.TrimSuffix(strings.TrimPrefix(a, "["), "]")
	if i := strings.IndexByte(a, '%'); i >= 0 {
		a = a[:i]
	}
	return a
}
