package wgtunnel_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"

	"github.com/loremlabs/thanks-computer/chassis/egress/wgtunnel"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/outlet/outlettest"
)

func keypair(t *testing.T) (priv, pub string) {
	t.Helper()
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		t.Fatal(err)
	}
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
	p, err := curve25519.X25519(k[:], curve25519.Basepoint)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(k[:]), base64.StdEncoding.EncodeToString(p)
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	_ = c.Close()
	return port
}

func TestParse(t *testing.T) {
	priv, pub := keypair(t)
	cfg, err := wgtunnel.Parse([]byte(fmt.Sprintf(`# issued by fly wireguard create
[Interface]
PrivateKey = %s
Address = fdaa:1:2:a:b::2/120, 10.99.0.2/32
DNS = fdaa:1:2::3
MTU = 1280

[Peer]
PublicKey = %s
AllowedIPs = fdaa:1:2::/48, 10.99.0.0/24 ; the org network
Endpoint = ams1.gateway.6pn.dev:51820
PersistentKeepalive = 15
`, priv, pub)))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Addresses) != 2 || cfg.Addresses[0].String() != "fdaa:1:2:a:b::2" || cfg.MTU != 1280 {
		t.Fatalf("interface: %+v", cfg)
	}
	p := cfg.Peers[0]
	if len(p.AllowedIPs) != 2 || p.Endpoint != "ams1.gateway.6pn.dev:51820" || p.Keepalive != 15*time.Second {
		t.Fatalf("peer: %+v", p)
	}
	for name, body := range map[string]string{
		"no private key": "[Interface]\nAddress = 10.0.0.1/32\n[Peer]\nPublicKey = " + pub + "\nAllowedIPs = 10.0.0.0/24\n",
		"unknown key":    "[Interface]\nPrivateKey = " + priv + "\nAddress = 10.0.0.1/32\nBogus = 1\n[Peer]\nPublicKey = " + pub + "\nAllowedIPs = 10.0.0.0/24\n",
		"no peer":        "[Interface]\nPrivateKey = " + priv + "\nAddress = 10.0.0.1/32\n",
		"bad key":        "[Interface]\nPrivateKey = notbase64!\nAddress = 10.0.0.1/32\n[Peer]\nPublicKey = " + pub + "\nAllowedIPs = 10.0.0.0/24\n",
		"bad endpoint":   "[Interface]\nPrivateKey = " + priv + "\nAddress = 10.0.0.1/32\n[Peer]\nPublicKey = " + pub + "\nAllowedIPs = 10.0.0.0/24\nEndpoint = nowhere\n",
	} {
		if _, err := wgtunnel.Parse([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

// Two tunnels in one process over loopback UDP: B is the relay side and
// runs a SOCKS5 server on its tunnel address; A is the chassis side and
// dials through it, the way outlet egress: relay does.
func TestTunnelRelayEndToEnd(t *testing.T) {
	aPriv, aPub := keypair(t)
	bPriv, bPub := keypair(t)
	bPort := freeUDPPort(t)

	bCfg, err := wgtunnel.Parse([]byte(fmt.Sprintf(
		"[Interface]\nPrivateKey = %s\nAddress = 10.99.0.1/32\nListenPort = %d\n[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.2/32\n",
		bPriv, bPort, aPub)))
	if err != nil {
		t.Fatal(err)
	}
	relaySide, err := wgtunnel.Open(bCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer relaySide.Close()

	aCfg, err := wgtunnel.Parse([]byte(fmt.Sprintf(
		"[Interface]\nPrivateKey = %s\nAddress = 10.99.0.2/32\n[Peer]\nPublicKey = %s\nAllowedIPs = 10.99.0.1/32\nEndpoint = localhost:%d\nPersistentKeepalive = 1\n",
		aPriv, bPub, bPort)))
	if err != nil {
		t.Fatal(err)
	}
	chassisSide, err := wgtunnel.Open(aCfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer chassisSide.Close()

	// The relay's SOCKS listener exists only inside the tunnel.
	ln, err := relaySide.ListenTCPAddrPort(netip.MustParseAddrPort("10.99.0.1:1080"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	socks := &outlettest.SOCKS5Server{}
	go socks.Serve(ln)

	// The destination the relay connects out to, on the real network.
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
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

	dial := outlet.DialFunc(outlet.EgressParams{
		Mode:    outlet.EgressRelay,
		Relays:  []string{"10.99.0.1:1080"},
		Forward: chassisSide,
	}, nil, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", target.Addr().String())
	if err != nil {
		t.Fatalf("dial through the tunnel and relay: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "ping\n" {
		t.Fatalf("echo through tunnel+relay: %q %v", buf[:n], err)
	}
	if ts := socks.Targets(); len(ts) != 1 || ts[0] != target.Addr().String() {
		t.Fatalf("relay saw %v", ts)
	}

	// The tunnel refuses a hostname: names are never resolved through it.
	if _, err := chassisSide.DialContext(ctx, "tcp", "relay.internal:1080"); err == nil || !strings.Contains(err.Error(), "ip:port") {
		t.Fatalf("hostname through the tunnel: %v", err)
	}
}
