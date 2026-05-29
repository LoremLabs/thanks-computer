// Package private registers the "private" egress policy: it blocks
// outbound op dials whose resolved IP falls in loopback, private,
// link-local (incl. cloud-metadata 169.254.169.254), CGNAT/Tailscale,
// IPv6 ULA/link-local, and other IETF special-use space — plus any
// operator-supplied deny CIDRs. An allow CIDR is an explicit escape
// hatch and wins over deny.
//
// The check runs at the dial step on the already-resolved IP, so it is
// DNS-rebinding safe. All CIDR sets are parsed once at Open(); CheckAddr
// is pure in-memory.
package private

import (
	"fmt"
	"net"

	"github.com/loremlabs/thanks-computer/chassis/egress"
)

// builtinDeny is the IETF special-use / private address space the
// "private" policy refuses. 169.254.0.0/16 covers the
// 169.254.169.254 link-local cloud-metadata endpoint used by every
// major provider; 100.64.0.0/10 covers CGNAT and Tailscale.
var builtinDeny = []string{
	// IPv4
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"255.255.255.255/32",
	// IPv6
	"::/128",
	"::1/128",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
	"2001:db8::/32",
	"100::/64",
}

func init() {
	egress.Register("private", func(cfg egress.Config) (egress.Guard, error) {
		deny, err := parseCIDRs(append(append([]string{}, builtinDeny...), cfg.DenyCIDRs...))
		if err != nil {
			return nil, fmt.Errorf("egress: deny %w", err)
		}
		allow, err := parseCIDRs(cfg.AllowCIDRs)
		if err != nil {
			return nil, fmt.Errorf("egress: allow %w", err)
		}
		return &guard{deny: deny, allow: allow}, nil
	})
}

func parseCIDRs(cidrs []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		if c == "" {
			continue
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("CIDR %q: %w", c, err)
		}
		out = append(out, n)
	}
	return out, nil
}

type guard struct {
	deny  []*net.IPNet
	allow []*net.IPNet
}

func (*guard) Name() string { return "private" }

func (g *guard) CheckAddr(network, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// At the dial step the address is always an IP literal. A
		// non-IP here is unexpected; fail closed under "private".
		return fmt.Errorf("egress: blocked unparseable address %q", address)
	}

	candidates := embedded(ip)

	// Allow wins: an explicit escape-hatch CIDR permits the dial even if
	// it (or an embedded v4) would otherwise be denied.
	for _, c := range candidates {
		if contains(g.allow, c) {
			return nil
		}
	}
	for _, c := range candidates {
		if contains(g.deny, c) {
			return fmt.Errorf("egress: blocked %s (private/internal address space)", ip)
		}
	}
	return nil
}

// embedded returns ip plus any IPv4 address tunnelled inside it
// (IPv4-mapped ::ffff:a.b.c.d, 6to4 2002::/16, NAT64 64:ff9b::/96), so
// an op cannot reach a private v4 by wrapping it in IPv6.
func embedded(ip net.IP) []net.IP {
	out := []net.IP{ip}
	if v4 := ip.To4(); v4 != nil {
		out = append(out, v4)
		return out
	}
	if b := ip.To16(); b != nil {
		switch {
		case b[0] == 0x20 && b[1] == 0x02: // 2002::/16 (6to4)
			out = append(out, net.IPv4(b[2], b[3], b[4], b[5]))
		case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b: // 64:ff9b::/96 (NAT64)
			out = append(out, net.IPv4(b[12], b[13], b[14], b[15]))
		}
	}
	return out
}

func contains(nets []*net.IPNet, ip net.IP) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
