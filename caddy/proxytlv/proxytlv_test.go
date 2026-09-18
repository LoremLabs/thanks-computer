package proxytlv

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/mholt/caddy-l4/layer4"
	"github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
	"go.uber.org/zap"
)

var (
	client = &net.TCPAddr{IP: net.ParseIP("176.151.108.50"), Port: 51000}
	dialed = &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 6697}
)

func ircState() *tls.ConnectionState {
	return &tls.ConnectionState{
		Version:            tls.VersionTLS13,
		ServerName:         "irc.moo.local.thanks.computer",
		NegotiatedProtocol: "irc",
	}
}

// parse reads one header back with the library a receiver would use.
func parse(t *testing.T, r io.Reader) (*proxyproto.Header, map[proxyproto.PP2Type]string, tlvparse.PP2SSL, bool) {
	t.Helper()
	h, err := proxyproto.Read(bufio.NewReader(r))
	if err != nil {
		t.Fatalf("header does not parse: %v", err)
	}
	tlvs, err := h.TLVs()
	if err != nil {
		t.Fatalf("TLVs do not parse: %v", err)
	}
	vals := map[proxyproto.PP2Type]string{}
	for _, tlv := range tlvs {
		vals[tlv.Type] = string(tlv.Value)
	}
	ssl, ok := tlvparse.FindSSL(tlvs)
	return h, vals, ssl, ok
}

func TestHeaderCarriesTheContract(t *testing.T) {
	b, err := Header(client, dialed, ircState())
	if err != nil {
		t.Fatal(err)
	}
	h, vals, ssl, hasSSL := parse(t, bytes.NewReader(b))
	if h.Version != 2 || h.Command != proxyproto.PROXY || h.TransportProtocol != proxyproto.TCPv4 {
		t.Errorf("header = v%d %v %v", h.Version, h.Command, h.TransportProtocol)
	}
	if h.SourceAddr.String() != client.String() || h.DestinationAddr.String() != dialed.String() {
		t.Errorf("addrs = %v → %v", h.SourceAddr, h.DestinationAddr)
	}
	if got := vals[proxyproto.PP2_TYPE_AUTHORITY]; got != "irc.moo.local.thanks.computer" {
		t.Errorf("AUTHORITY = %q", got)
	}
	if got := vals[proxyproto.PP2_TYPE_ALPN]; got != "irc" {
		t.Errorf("ALPN = %q", got)
	}
	if !hasSSL || !ssl.ClientSSL() || ssl.ClientCertConn() || ssl.Verified() {
		t.Errorf("SSL = %+v present=%v, want client TLS, no verified client cert", ssl, hasSSL)
	}
	if v, _ := ssl.SSLVersion(); v != "TLS 1.3" {
		t.Errorf("SSL_VERSION = %q", v)
	}
}

func TestHeaderOmitsWhatWasNotObserved(t *testing.T) {
	t.Run("no SNI, no ALPN", func(t *testing.T) {
		b, err := Header(client, dialed, &tls.ConnectionState{Version: tls.VersionTLS12})
		if err != nil {
			t.Fatal(err)
		}
		_, vals, ssl, hasSSL := parse(t, bytes.NewReader(b))
		if _, ok := vals[proxyproto.PP2_TYPE_AUTHORITY]; ok {
			t.Error("AUTHORITY sent without an SNI")
		}
		if _, ok := vals[proxyproto.PP2_TYPE_ALPN]; ok {
			t.Error("ALPN sent without a negotiated protocol")
		}
		if v, _ := ssl.SSLVersion(); !hasSSL || v != "TLS 1.2" {
			t.Errorf("SSL present=%v version=%q", hasSSL, v)
		}
	})
	t.Run("plaintext client leg", func(t *testing.T) {
		b, err := Header(client, dialed, nil)
		if err != nil {
			t.Fatal(err)
		}
		h, vals, _, hasSSL := parse(t, bytes.NewReader(b))
		if len(vals) != 0 || hasSSL {
			t.Errorf("TLVs = %v ssl=%v, want none", vals, hasSSL)
		}
		if h.SourceAddr.String() != client.String() {
			t.Errorf("source = %v", h.SourceAddr)
		}
	})
	t.Run("ipv6", func(t *testing.T) {
		b, err := Header(&net.TCPAddr{IP: net.ParseIP("2001:db8::1"), Port: 1}, &net.TCPAddr{IP: net.ParseIP("fdaa::3"), Port: 6697}, ircState())
		if err != nil {
			t.Fatal(err)
		}
		if h, _, _, _ := parse(t, bytes.NewReader(b)); h.TransportProtocol != proxyproto.TCPv6 {
			t.Errorf("transport = %v", h.TransportProtocol)
		}
	})
	t.Run("no usable addresses", func(t *testing.T) {
		in, _ := net.Pipe()
		defer in.Close()
		if _, err := Header(in.RemoteAddr(), in.LocalAddr(), ircState()); err == nil {
			t.Error("want an error rather than an address-less header")
		}
	})
}

// addrConn gives a pipe TCP addresses — what `proxy_protocol` leaves on
// the connection for us (l4proxyprotocol.GetConn).
type addrConn struct {
	net.Conn
	remote, local net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }
func (c addrConn) LocalAddr() net.Addr  { return c.local }

// handle runs the handler over cx and returns what `next` was given.
func handle(t *testing.T, cx *layer4.Connection) *layer4.Connection {
	t.Helper()
	var got *layer4.Connection
	err := (&Handler{logger: zap.NewNop()}).Handle(cx, layer4.HandlerFunc(func(c *layer4.Connection) error {
		got = c
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestHandlePutsTheHeaderFirst(t *testing.T) {
	in, out := net.Pipe()
	defer in.Close()
	defer out.Close()

	// "NICK " was already prefetched by a matcher; "moo\r\n" is still on
	// the wire. The header must come before both.
	cx := layer4.WrapConnection(in, []byte("NICK "), zap.NewNop())
	cx.SetVar("l4.proxy_protocol.conn", addrConn{Conn: in, remote: client, local: dialed})
	cx.SetVar("tls_connection_states", []*tls.ConnectionState{
		{ServerName: "outer.example"}, // an outer hop, were there one: not the client's
		ircState(),
	})
	cx.SetVar("marker", "kept")

	go func() {
		_ = out.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = out.Write([]byte("moo\r\n"))
		_ = out.Close()
	}()

	next := handle(t, cx)
	if next.GetVar("marker") != "kept" {
		t.Error("connection vars were not carried to the next handler")
	}
	r := bufio.NewReader(next)
	h, err := proxyproto.Read(r)
	if err != nil {
		t.Fatalf("stream does not start with a PROXY header: %v", err)
	}
	if h.SourceAddr.String() != client.String() {
		t.Errorf("source = %v, want the address proxy_protocol learned", h.SourceAddr)
	}
	tlvs, _ := h.TLVs()
	var authority string
	for _, tlv := range tlvs {
		if tlv.Type == proxyproto.PP2_TYPE_AUTHORITY {
			authority = string(tlv.Value)
		}
	}
	if authority != "irc.moo.local.thanks.computer" {
		t.Errorf("AUTHORITY = %q, want the innermost TLS session's SNI", authority)
	}
	rest, err := io.ReadAll(r)
	if err != nil || string(rest) != "NICK moo\r\n" {
		t.Errorf("after the header: %q err=%v, want the client's bytes once, in order", rest, err)
	}
}

func TestHandleSmallReads(t *testing.T) {
	// A reader with a tiny buffer still gets the whole header, in order.
	in, out := net.Pipe()
	defer in.Close()
	cx := layer4.WrapConnection(in, nil, zap.NewNop())
	cx.SetVar("l4.proxy_protocol.conn", addrConn{Conn: in, remote: client, local: dialed})
	cx.SetVar("tls_connection_states", []*tls.ConnectionState{ircState()})
	go func() { _, _ = out.Write([]byte("x")); _ = out.Close() }()

	want, _ := Header(client, dialed, ircState())
	next := handle(t, cx)
	var got []byte
	buf := make([]byte, 3)
	for {
		n, err := next.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	if !bytes.Equal(got, append(want, 'x')) {
		t.Errorf("stream = %q\n want    %q", got, append(want, 'x'))
	}
}

func TestHandleWritesPassThroughAndHalfClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	next := handle(t, layer4.WrapConnection(srv, nil, zap.NewNop()))
	if _, err := next.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	// `proxy` half-closes the client leg through this interface when the
	// upstream hangs up; the client must see EOF, not a hung socket.
	cw, ok := next.Conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the connection handed to `proxy` lost CloseWrite")
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(cli)
	if err != nil || string(got) != "hello\n" {
		t.Errorf("client read %q err=%v, want the write then EOF", got, err)
	}
}

func TestUnmarshalCaddyfile(t *testing.T) {
	for in, wantErr := range map[string]bool{
		"proxy_tlv":            false,
		"proxy_tlv v2":         true,
		"proxy_tlv {\n foo\n}": true,
		"proxy_tlv {\n}":       false,
	} {
		err := new(Handler).UnmarshalCaddyfile(caddyfile.NewTestDispenser(in))
		if (err != nil) != wantErr {
			t.Errorf("%q: err=%v, wantErr=%v", in, err, wantErr)
		}
	}
}
