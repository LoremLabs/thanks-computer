// Package wgtunnel brings up a WireGuard tunnel inside the chassis process —
// userspace, no kernel interface, no root — so an outbound dial can leave
// through a peer on a private network the host itself is not on. The first
// user is outlet egress: a node reaches its relays by their tunnel addresses
// (docs/todo-egress.md). It is the same trick flyctl uses to reach a Fly
// organisation's private network from a laptop.
//
// The configuration is a standard wg-quick file, so a peer config issued by
// `fly wireguard create`, a self-managed one, or any WireGuard server's
// client config works unchanged:
//
//	[Interface]
//	PrivateKey = <base64>
//	Address = fdaa:1:2:a:b::2/120, 10.99.0.2/32
//	MTU = 1420
//
//	[Peer]
//	PublicKey = <base64>
//	AllowedIPs = fdaa:1:2::/48
//	Endpoint = gateway.example:51820
//	PersistentKeepalive = 15
//
// DNS lines are accepted and ignored: names are never resolved through the
// tunnel, and a dial needs an ip:port.
package wgtunnel

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Peer is one [Peer] section.
type Peer struct {
	PublicKey    [32]byte
	PresharedKey *[32]byte
	// Endpoint as written, host:port; a hostname is resolved when the
	// tunnel comes up and again on first use if that failed.
	Endpoint   string
	AllowedIPs []netip.Prefix
	Keepalive  time.Duration
}

// Config is a parsed wg-quick file.
type Config struct {
	PrivateKey [32]byte
	Addresses  []netip.Addr
	ListenPort int
	MTU        int
	Peers      []Peer
}

// DefaultMTU is wg-quick's default.
const DefaultMTU = 1420

// Parse decodes a wg-quick configuration. Unknown keys are refused so a
// typo cannot silently drop a peer setting.
func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	section := ""
	var peer *Peer
	havePrivate := false
	for n, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			switch section {
			case "interface":
			case "peer":
				cfg.Peers = append(cfg.Peers, Peer{})
				peer = &cfg.Peers[len(cfg.Peers)-1]
			default:
				return nil, fmt.Errorf("wgtunnel: line %d: unknown section [%s]", n+1, section)
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("wgtunnel: line %d: expected key = value", n+1)
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		var err error
		switch section {
		case "interface":
			switch k {
			case "privatekey":
				err = decodeKey(v, &cfg.PrivateKey)
				havePrivate = err == nil
			case "address":
				for _, a := range splitList(v) {
					p, perr := netip.ParsePrefix(a)
					if perr != nil {
						addr, aerr := netip.ParseAddr(a)
						if aerr != nil {
							err = fmt.Errorf("address %q: %w", a, perr)
							break
						}
						cfg.Addresses = append(cfg.Addresses, addr)
						continue
					}
					cfg.Addresses = append(cfg.Addresses, p.Addr())
				}
			case "listenport":
				cfg.ListenPort, err = strconv.Atoi(v)
			case "mtu":
				cfg.MTU, err = strconv.Atoi(v)
			case "dns", "table", "fwmark", "saveconfig", "preup", "postup", "predown", "postdown":
				// wg-quick host plumbing; nothing to do in userspace.
			default:
				err = fmt.Errorf("unknown [Interface] key %q", k)
			}
		case "peer":
			switch k {
			case "publickey":
				err = decodeKey(v, &peer.PublicKey)
			case "presharedkey":
				var psk [32]byte
				if err = decodeKey(v, &psk); err == nil {
					peer.PresharedKey = &psk
				}
			case "allowedips":
				for _, a := range splitList(v) {
					p, perr := netip.ParsePrefix(a)
					if perr != nil {
						err = fmt.Errorf("allowedips %q: %w", a, perr)
						break
					}
					peer.AllowedIPs = append(peer.AllowedIPs, p)
				}
			case "endpoint":
				if _, _, serr := splitHostPort(v); serr != nil {
					err = fmt.Errorf("endpoint %q: %w", v, serr)
				}
				peer.Endpoint = v
			case "persistentkeepalive":
				var secs int
				if secs, err = strconv.Atoi(v); err == nil {
					peer.Keepalive = time.Duration(secs) * time.Second
				}
			default:
				err = fmt.Errorf("unknown [Peer] key %q", k)
			}
		default:
			err = fmt.Errorf("key %q before any section", k)
		}
		if err != nil {
			return nil, fmt.Errorf("wgtunnel: line %d: %w", n+1, err)
		}
	}
	if !havePrivate {
		return nil, fmt.Errorf("wgtunnel: [Interface] PrivateKey is required")
	}
	if len(cfg.Addresses) == 0 {
		return nil, fmt.Errorf("wgtunnel: [Interface] Address is required")
	}
	if len(cfg.Peers) == 0 {
		return nil, fmt.Errorf("wgtunnel: at least one [Peer] is required")
	}
	for i, p := range cfg.Peers {
		if p.PublicKey == [32]byte{} {
			return nil, fmt.Errorf("wgtunnel: peer %d has no PublicKey", i+1)
		}
		if len(p.AllowedIPs) == 0 {
			return nil, fmt.Errorf("wgtunnel: peer %d has no AllowedIPs", i+1)
		}
	}
	if cfg.MTU == 0 {
		cfg.MTU = DefaultMTU
	}
	return cfg, nil
}

func decodeKey(v string, out *[32]byte) error {
	b, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return fmt.Errorf("key is not base64")
	}
	if len(b) != 32 {
		return fmt.Errorf("key must be 32 bytes")
	}
	copy(out[:], b)
	return nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
