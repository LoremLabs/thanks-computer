// Package edgeproxy is the chassis side of the trusted-edge contract:
//
//	trusted edge | PROXY v2 (+ AUTHORITY, SSL, ALPN TLVs) → TCP inlet
//
// An edge that terminates TLS in front of the chassis (Caddy today) has
// already seen everything an inlet would learn from its own handshake —
// the real client address, the SNI hostname, the negotiated ALPN and TLS
// version. It hands them over in the PROXY protocol header it writes
// before the first byte of the stream, and this package is the one place
// that header is accepted and read.
//
// The trust boundary is the peer address: a header is REQUIRED from the
// operator's trusted CIDRs and never honoured from anyone else (their
// connection is served as-is, header bytes and all). So a parsed header
// is proof the connection came through the front door, and its contents
// are chassis facts, not client input.
//
// Leaf package: no chassis imports, so any head (imap, tcp, …) can use it.
package edgeproxy

import (
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
)

// DefaultHeaderTimeout bounds how long a trusted peer may take to send
// its header. The edge writes it with the first packet; anything slower
// is a broken or hostile peer holding a socket open.
const DefaultHeaderTimeout = 5 * time.Second

// ErrNoHeader is Read's answer for a trusted peer that sent no (or an
// unparseable) PROXY header. The stream is unusable — every read on it
// fails — so the caller closes.
var ErrNoHeader = errors.New("edgeproxy: trusted peer sent no valid PROXY header")

// ParseTrusted turns operator entries (CIDRs, or bare IPs meaning /32 or
// /128) into networks. Blank entries are dropped; entries that parse as
// neither come back in bad so the caller decides whether that is a
// warning (imap) or a refusal to start (tcp).
func ParseTrusted(entries []string) (nets []*net.IPNet, bad []string) {
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, n, err := net.ParseCIDR(e); err == nil {
			nets = append(nets, n)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		bad = append(bad, e)
	}
	return nets, bad
}

// Trusted reports whether a socket peer address is inside nets.
func Trusted(nets []*net.IPNet, a net.Addr) bool {
	if a == nil {
		return false
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		host = a.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// Wrap makes ln accept a PROXY header (v1 or v2) from trusted peers:
// REQUIRE from inside trusted, SKIP for everyone else — their connection
// is handed back untouched and a header they send is just stream bytes.
// Wrap BEFORE tls.NewListener: the header precedes the handshake.
// headerTimeout <= 0 means DefaultHeaderTimeout. With no trusted networks
// ln is returned as is.
func Wrap(ln net.Listener, trusted []*net.IPNet, headerTimeout time.Duration) net.Listener {
	if len(trusted) == 0 {
		return ln
	}
	if headerTimeout <= 0 {
		headerTimeout = DefaultHeaderTimeout
	}
	return &proxyproto.Listener{
		Listener:          ln,
		ReadHeaderTimeout: headerTimeout,
		ConnPolicy: func(o proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
			if Trusted(trusted, o.Upstream) {
				return proxyproto.REQUIRE, nil
			}
			return proxyproto.SKIP, nil
		},
	}
}

// Facts is what a trusted edge said about one connection. The zero value
// (Present false) means nobody vouched for it: an untrusted peer, an
// unwrapped listener, or the edge's own LOCAL health probe.
type Facts struct {
	Present bool

	// The real client and the address it dialled, from the header. The
	// wrapped conn's RemoteAddr/LocalAddr already report these too.
	ClientIP   string
	ClientPort int
	LocalIP    string
	LocalPort  int

	Authority  string // PP2_TYPE_AUTHORITY: the hostname the client asked for (SNI), raw
	ALPN       string // PP2_TYPE_ALPN
	TLS        bool   // PP2_TYPE_SSL says the client leg was TLS
	TLSVersion string // PP2_SUBTYPE_SSL_VERSION, in crypto/tls spelling ("TLS 1.3")
}

// Read reports what the edge said about conn, blocking until the header
// has arrived (bounded by Wrap's headerTimeout). Call it off the accept
// loop. A conn from a listener Wrap never touched, or from an untrusted
// peer, yields the zero Facts and no error.
func Read(conn net.Conn) (Facts, error) {
	pc := unwrap(conn)
	if pc == nil {
		return Facts{}, nil
	}
	// ProxyHeader parses at most once; later calls are free.
	h := pc.ProxyHeader()
	if h == nil {
		return Facts{}, ErrNoHeader
	}
	if h.Command.IsLocal() {
		return Facts{}, nil
	}
	f := Facts{Present: true}
	if src, dst, ok := h.TCPAddrs(); ok {
		f.ClientIP, f.ClientPort = src.IP.String(), src.Port
		f.LocalIP, f.LocalPort = dst.IP.String(), dst.Port
	}
	tlvs, err := h.TLVs()
	if err != nil {
		// The edge is ours; a header it mangled is a bug to surface, not
		// a connection to serve on half its facts.
		return Facts{}, err
	}
	for _, t := range tlvs {
		switch t.Type {
		case proxyproto.PP2_TYPE_AUTHORITY:
			f.Authority = string(t.Value)
		case proxyproto.PP2_TYPE_ALPN:
			f.ALPN = string(t.Value)
		}
	}
	if ssl, ok := tlvparse.FindSSL(tlvs); ok && ssl.ClientSSL() {
		f.TLS = true
		if v, ok := ssl.SSLVersion(); ok {
			f.TLSVersion = tlsVersionName(v)
		}
	}
	return f, nil
}

// Fronted reports whether a trusted edge presented a PROXY header on
// conn — the cheap form of Read for heads that only need the yes/no.
func Fronted(conn net.Conn) bool {
	pc := unwrap(conn)
	return pc != nil && pc.ProxyHeader() != nil
}

// unwrap finds the proxyproto layer. One layer of TLS is peeled because a
// TLS listener wraps the PROXY listener, not the other way round.
func unwrap(conn net.Conn) *proxyproto.Conn {
	if tc, ok := conn.(*tls.Conn); ok {
		conn = tc.NetConn()
	}
	pc, _ := conn.(*proxyproto.Conn)
	return pc
}

// tlsVersionName maps the spellings edges use ("TLSv1.3", haproxy's and
// OpenSSL's) onto crypto/tls's ("TLS 1.3"), so a stack sees one form no
// matter which side terminated TLS.
func tlsVersionName(v string) string {
	if rest, ok := strings.CutPrefix(v, "TLSv"); ok {
		return "TLS " + rest
	}
	return v
}
