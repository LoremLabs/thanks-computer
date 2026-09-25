package wgtunnel

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// Tunnel is one WireGuard interface living in this process. Dial through it
// with DialContext; it satisfies the outlet package's ContextDialer.
type Tunnel struct {
	cfg  *Config
	dev  *device.Device
	tnet *netstack.Net
	log  *zap.Logger

	mu         sync.Mutex
	unresolved map[int]struct{} // peers whose Endpoint hostname hasn't resolved yet
	closed     bool
}

// OpenFile reads a wg-quick file and brings the tunnel up. The file holds a
// private key: it is read once and never logged.
func OpenFile(path string, log *zap.Logger) (*Tunnel, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wgtunnel: read %s: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return Open(cfg, log)
}

// Open brings a tunnel up. A peer whose Endpoint hostname doesn't resolve
// right now is configured without one and resolved again on first use, so
// a DNS hiccup at boot never needs a restart.
func Open(cfg *Config, log *zap.Logger) (*Tunnel, error) {
	if log == nil {
		log = zap.NewNop()
	}
	mtu := cfg.MTU
	if mtu <= 0 {
		mtu = DefaultMTU
	}
	tunDev, tnet, err := netstack.CreateNetTUN(cfg.Addresses, nil, mtu)
	if err != nil {
		return nil, fmt.Errorf("wgtunnel: create interface: %w", err)
	}
	sugar := log.Sugar()
	dev := device.NewDevice(tunDev, conn.NewDefaultBind(), &device.Logger{
		Verbosef: func(string, ...any) {},
		Errorf:   func(format string, args ...any) { sugar.Errorf("wgtunnel: "+format, args...) },
	})
	t := &Tunnel{cfg: cfg, dev: dev, tnet: tnet, log: log, unresolved: map[int]struct{}{}}

	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(cfg.PrivateKey[:]))
	if cfg.ListenPort > 0 {
		fmt.Fprintf(&b, "listen_port=%d\n", cfg.ListenPort)
	}
	b.WriteString("replace_peers=true\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i, p := range cfg.Peers {
		fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(p.PublicKey[:]))
		if p.PresharedKey != nil {
			fmt.Fprintf(&b, "preshared_key=%s\n", hex.EncodeToString(p.PresharedKey[:]))
		}
		b.WriteString("replace_allowed_ips=true\n")
		for _, ip := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", ip.String())
		}
		if p.Keepalive > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", int(p.Keepalive/time.Second))
		}
		if p.Endpoint != "" {
			ap, rerr := resolveEndpoint(ctx, p.Endpoint)
			if rerr != nil {
				log.Warn("wgtunnel: peer endpoint did not resolve; will retry on first use", zap.Int("peer", i+1), zap.Error(rerr))
				t.unresolved[i] = struct{}{}
			} else {
				fmt.Fprintf(&b, "endpoint=%s\n", ap.String())
			}
		}
	}
	if err := dev.IpcSet(b.String()); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgtunnel: configure: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("wgtunnel: up: %w", err)
	}
	log.Info("wgtunnel: up", zap.Int("peers", len(cfg.Peers)), zap.Int("addresses", len(cfg.Addresses)), zap.Int("mtu", mtu))
	return t, nil
}

// ErrNeedsAddrPort is returned for a dial address that isn't ip:port. The
// tunnel does no name resolution.
var ErrNeedsAddrPort = errors.New("wgtunnel: dial address must be ip:port")

// DialContext dials a TCP address through the tunnel.
func (t *Tunnel) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("wgtunnel: network %q is not supported", network)
	}
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, ErrNeedsAddrPort
	}
	t.ensureEndpoints(ctx)
	c, err := t.tnet.DialContextTCPAddrPort(ctx, ap)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ListenTCPAddrPort listens on a tunnel address — the relay side of a
// test, or a service that should be reachable only through the tunnel.
func (t *Tunnel) ListenTCPAddrPort(ap netip.AddrPort) (net.Listener, error) {
	return t.tnet.ListenTCPAddrPort(ap)
}

// Close tears the tunnel down.
func (t *Tunnel) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	t.dev.Close()
	return nil
}

// ensureEndpoints retries the endpoints that didn't resolve at Open.
func (t *Tunnel) ensureEndpoints(ctx context.Context) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.unresolved {
		p := t.cfg.Peers[i]
		ap, err := resolveEndpoint(ctx, p.Endpoint)
		if err != nil {
			continue
		}
		set := fmt.Sprintf("public_key=%s\nendpoint=%s\n", hex.EncodeToString(p.PublicKey[:]), ap.String())
		if err := t.dev.IpcSet(set); err != nil {
			t.log.Warn("wgtunnel: set endpoint", zap.Int("peer", i+1), zap.Error(err))
			continue
		}
		delete(t.unresolved, i)
		t.log.Info("wgtunnel: peer endpoint resolved", zap.Int("peer", i+1))
	}
}

// resolveEndpoint turns host:port into ip:port, resolving a hostname with
// the system resolver (WireGuard endpoints are outside the tunnel).
func resolveEndpoint(ctx context.Context, endpoint string) (netip.AddrPort, error) {
	host, port, err := splitHostPort(endpoint)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if ap, perr := netip.ParseAddrPort(net.JoinHostPort(host, port)); perr == nil {
		return ap, nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if len(ips) == 0 {
		return netip.AddrPort{}, fmt.Errorf("%s: no addresses", host)
	}
	// Prefer IPv4: the hosts that run this have v4 egress for certain.
	pick := ips[0]
	for _, ip := range ips {
		if ip.Unmap().Is4() {
			pick = ip
			break
		}
	}
	p, err := netip.ParseAddrPort(net.JoinHostPort(pick.Unmap().String(), port))
	if err != nil {
		return netip.AddrPort{}, err
	}
	return p, nil
}

func splitHostPort(s string) (string, string, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", err
	}
	if host == "" || port == "" {
		return "", "", fmt.Errorf("%q: host and port are required", s)
	}
	return host, port, nil
}
