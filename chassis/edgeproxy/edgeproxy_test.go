package edgeproxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
)

func nets(t *testing.T, entries ...string) []*net.IPNet {
	t.Helper()
	n, bad := ParseTrusted(entries)
	if len(bad) > 0 {
		t.Fatalf("ParseTrusted(%v) bad = %v", entries, bad)
	}
	return n
}

func TestParseTrustedAndTrusted(t *testing.T) {
	n, bad := ParseTrusted([]string{"10.0.0.0/8", " 192.168.1.5 ", "", "fdaa::/8", "::1", "bogus", "10.0.0.0/99"})
	if len(n) != 4 {
		t.Errorf("nets = %v, want 4", n)
	}
	if len(bad) != 2 || bad[0] != "bogus" || bad[1] != "10.0.0.0/99" {
		t.Errorf("bad = %v", bad)
	}
	for addr, want := range map[string]bool{
		"10.1.2.3:44":     true,
		"192.168.1.5:1":   true,
		"192.168.1.6:1":   false, // a bare IP is a /32, not its subnet
		"127.0.0.1:9":     false,
		"[fdaa::3]:6697":  true,
		"[::1]:1":         true,
		"[2001:db8::1]:1": false,
	} {
		a, err := net.ResolveTCPAddr("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		if got := Trusted(n, a); got != want {
			t.Errorf("Trusted(%s) = %v, want %v", addr, got, want)
		}
	}
	if Trusted(n, nil) {
		t.Error("nil addr trusted")
	}
	if Trusted(nil, &net.TCPAddr{IP: net.ParseIP("10.1.2.3")}) {
		t.Error("empty trust list trusted someone")
	}
}

// edgeHeader is the header the edge adapter writes: v2, the real client
// and the address it dialled, and the three TLVs of the contract.
func edgeHeader(t *testing.T, authority, alpn, version string) *proxyproto.Header {
	t.Helper()
	h := proxyproto.HeaderProxyFromAddrs(2,
		&net.TCPAddr{IP: net.ParseIP("176.151.108.50"), Port: 51000},
		&net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 6697})
	var tlvs []proxyproto.TLV
	if authority != "" {
		tlvs = append(tlvs, proxyproto.TLV{Type: proxyproto.PP2_TYPE_AUTHORITY, Value: []byte(authority)})
	}
	if alpn != "" {
		tlvs = append(tlvs, proxyproto.TLV{Type: proxyproto.PP2_TYPE_ALPN, Value: []byte(alpn)})
	}
	if version != "" {
		ssl, err := tlvparse.PP2SSL{
			Client: tlvparse.PP2_BITFIELD_CLIENT_SSL,
			Verify: 1, // no client certificate
			TLV:    []proxyproto.TLV{{Type: proxyproto.PP2_SUBTYPE_SSL_VERSION, Value: []byte(version)}},
		}.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		tlvs = append(tlvs, ssl)
	}
	if err := h.SetTLVs(tlvs); err != nil {
		t.Fatal(err)
	}
	return h
}

// listen wraps a loopback listener and hands back each accepted conn.
func listen(t *testing.T, trusted []*net.IPNet, timeout time.Duration) (addr string, accepted <-chan net.Conn) {
	t.Helper()
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := Wrap(raw, trusted, timeout)
	t.Cleanup(func() { _ = ln.Close() })
	ch := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			ch <- c
		}
	}()
	return raw.Addr().String(), ch
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func next(t *testing.T, ch <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-ch:
		t.Cleanup(func() { _ = c.Close() })
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no connection accepted")
		return nil
	}
}

func TestReadEdgeFacts(t *testing.T) {
	addr, accepted := listen(t, nets(t, "127.0.0.0/8"), time.Second)
	cli := dial(t, addr)
	if _, err := edgeHeader(t, "irc.moo.local.thanks.computer", "irc", "TLSv1.3").WriteTo(cli); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Write([]byte("NICK moo\r\n")); err != nil {
		t.Fatal(err)
	}
	srv := next(t, accepted)

	f, err := Read(srv)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := Facts{
		Present:  true,
		ClientIP: "176.151.108.50", ClientPort: 51000,
		LocalIP: "203.0.113.7", LocalPort: 6697,
		Authority: "irc.moo.local.thanks.computer", ALPN: "irc",
		TLS: true, TLSVersion: "TLS 1.3",
	}
	if f != want {
		t.Errorf("facts = %+v\n want   %+v", f, want)
	}
	if !Fronted(srv) {
		t.Error("Fronted = false behind a trusted header")
	}
	// The wrapped conn reports the client, not the edge, and the stream
	// starts after the header.
	if host, _, _ := net.SplitHostPort(srv.RemoteAddr().String()); host != "176.151.108.50" {
		t.Errorf("RemoteAddr = %v", srv.RemoteAddr())
	}
	line, err := bufio.NewReader(srv).ReadString('\n')
	if err != nil || line != "NICK moo\r\n" {
		t.Errorf("stream after header = %q err=%v", line, err)
	}
}

func TestReadHeaderWithoutTLVs(t *testing.T) {
	// What the edge sends for IMAP today (and l4proxy's stock v2 header):
	// addresses only. Present, but no hostname to route on.
	addr, accepted := listen(t, nets(t, "127.0.0.0/8"), time.Second)
	cli := dial(t, addr)
	if _, err := edgeHeader(t, "", "", "").WriteTo(cli); err != nil {
		t.Fatal(err)
	}
	f, err := Read(next(t, accepted))
	if err != nil || !f.Present || f.ClientIP != "176.151.108.50" || f.Authority != "" || f.TLS {
		t.Errorf("facts = %+v err=%v", f, err)
	}
}

func TestReadV1Header(t *testing.T) {
	addr, accepted := listen(t, nets(t, "127.0.0.0/8"), time.Second)
	cli := dial(t, addr)
	if _, err := cli.Write([]byte("PROXY TCP4 176.151.108.50 10.0.0.1 51000 1143\r\n")); err != nil {
		t.Fatal(err)
	}
	f, err := Read(next(t, accepted))
	if err != nil || !f.Present || f.ClientIP != "176.151.108.50" || f.LocalPort != 1143 {
		t.Errorf("facts = %+v err=%v", f, err)
	}
}

func TestUntrustedPeerIsNeverBelieved(t *testing.T) {
	// 127.0.0.1 is outside the trust list: SKIP. The header it sends is
	// not parsed — it stays in the stream — and nothing is vouched for.
	addr, accepted := listen(t, nets(t, "10.0.0.0/8"), time.Second)
	cli := dial(t, addr)
	if _, err := edgeHeader(t, "irc.victim.example", "irc", "TLSv1.3").WriteTo(cli); err != nil {
		t.Fatal(err)
	}
	srv := next(t, accepted)
	f, err := Read(srv)
	if err != nil || f != (Facts{}) {
		t.Errorf("facts = %+v err=%v, want zero", f, err)
	}
	if Fronted(srv) {
		t.Error("untrusted peer reported as fronted")
	}
	if host, _, _ := net.SplitHostPort(srv.RemoteAddr().String()); host != "127.0.0.1" {
		t.Errorf("RemoteAddr = %v, want the socket peer", srv.RemoteAddr())
	}
	sig := make([]byte, 12)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := srv.Read(sig); err != nil || string(sig) != "\r\n\r\n\x00\r\nQUIT\n" {
		t.Errorf("first bytes = %q err=%v, want the unparsed v2 signature", sig, err)
	}
}

func TestTrustedPeerWithoutHeader(t *testing.T) {
	addr, accepted := listen(t, nets(t, "127.0.0.0/8"), time.Second)

	t.Run("garbage", func(t *testing.T) {
		cli := dial(t, addr)
		if _, err := cli.Write([]byte("NICK moo\r\nUSER moo 0 * :moo\r\n")); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(next(t, accepted)); !errors.Is(err, ErrNoHeader) {
			t.Errorf("err = %v, want ErrNoHeader", err)
		}
	})

	t.Run("silence is bounded", func(t *testing.T) {
		addr, accepted := listen(t, nets(t, "127.0.0.0/8"), 150*time.Millisecond)
		dial(t, addr)
		srv := next(t, accepted)
		start := time.Now()
		if _, err := Read(srv); !errors.Is(err, ErrNoHeader) {
			t.Errorf("err = %v, want ErrNoHeader", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("Read blocked %v, want ~headerTimeout", d)
		}
	})
}

func TestLocalCommandVouchesForNothing(t *testing.T) {
	// A LOCAL header is the proxy talking for itself (health probes).
	addr, accepted := listen(t, nets(t, "127.0.0.0/8"), time.Second)
	cli := dial(t, addr)
	h := &proxyproto.Header{Version: 2, Command: proxyproto.LOCAL, TransportProtocol: proxyproto.UNSPEC}
	if _, err := h.WriteTo(cli); err != nil {
		t.Fatal(err)
	}
	srv := next(t, accepted)
	f, err := Read(srv)
	if err != nil || f.Present {
		t.Errorf("facts = %+v err=%v, want not present", f, err)
	}
}

func TestUnwrappedListener(t *testing.T) {
	srv, cli := net.Pipe()
	defer srv.Close()
	defer cli.Close()
	if f, err := Read(srv); err != nil || f.Present {
		t.Errorf("bare conn: facts = %+v err=%v", f, err)
	}
	if Fronted(srv) {
		t.Error("a connection nobody fronted reported as fronted")
	}
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if Wrap(raw, nil, 0) != raw {
		t.Error("Wrap with no trusted networks must return the listener untouched")
	}
}

func TestFrontedUnderTLS(t *testing.T) {
	// tls.NewListener wraps the PROXY listener, so an IMAPS session sees
	// a *tls.Conn with the header one layer down. No handshake happens
	// here: only NetConn() is touched.
	srv, cli := net.Pipe()
	t.Cleanup(func() { _ = srv.Close(); _ = cli.Close() })
	go func() {
		_ = cli.SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, _ = cli.Write([]byte("PROXY TCP4 176.151.108.50 10.0.0.1 51000 1143\r\n"))
	}()
	tc := tls.Client(proxyproto.NewConn(srv), &tls.Config{InsecureSkipVerify: true})
	if !Fronted(tc) {
		t.Error("PROXY header under TLS not reported")
	}
}

func TestTLSVersionName(t *testing.T) {
	for in, want := range map[string]string{
		"TLSv1.3": "TLS 1.3", "TLSv1.2": "TLS 1.2", "TLS 1.3": "TLS 1.3", "": "", "SSLv3": "SSLv3",
	} {
		if got := tlsVersionName(in); got != want {
			t.Errorf("tlsVersionName(%q) = %q, want %q", in, got, want)
		}
	}
}
