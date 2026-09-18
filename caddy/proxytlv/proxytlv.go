// Package proxytlv is a caddy-l4 handler that tells the upstream what a
// TLS-terminating edge saw. It writes a PROXY protocol v2 header ahead of
// the client's stream carrying:
//
//	source / destination   the real client, and the address it dialled
//	PP2_TYPE_AUTHORITY     the SNI hostname
//	PP2_TYPE_ALPN          the negotiated application protocol
//	PP2_TYPE_SSL           "the client leg was TLS", + PP2_SUBTYPE_SSL_VERSION
//
// That header IS the contract; the receiving side needs no knowledge of
// Caddy (HAProxy's `send-proxy-v2-ssl` + `proxy-v2-options authority`
// produces the same thing). This module exists only because caddy-l4's
// `proxy` handler writes PROXY headers without TLVs — delete it the day
// `proxy` can emit them itself.
//
// Place it after `tls` and before `proxy`, and leave `proxy_protocol` off
// the `proxy` handler (it would add a second header):
//
//	route {
//		proxy_protocol { allow 172.16.0.0/12 }   # optional: the hop in front of Caddy
//		tls
//		proxy_tlv
//		proxy { upstream chassis.internal:16697 }
//	}
package proxytlv

import (
	"crypto/tls"
	"fmt"
	"net"
	"sync"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/mholt/caddy-l4/layer4"
	"github.com/mholt/caddy-l4/modules/l4proxyprotocol"
	"github.com/mholt/caddy-l4/modules/l4tls"
	"github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
	"go.uber.org/zap"
)

func init() {
	caddy.RegisterModule(&Handler{})
}

// Handler prepends a PROXY v2 header with TLS TLVs to the client stream.
// It has no options.
type Handler struct {
	logger *zap.Logger
}

// CaddyModule returns the Caddy module information.
func (*Handler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "layer4.handlers.proxy_tlv",
		New: func() caddy.Module { return new(Handler) },
	}
}

// Provision sets up the module.
func (h *Handler) Provision(ctx caddy.Context) error {
	h.logger = ctx.Logger(h)
	return nil
}

// Handle builds the header from what the earlier handlers recorded on the
// connection and hands the next handler a stream that starts with it.
func (h *Handler) Handle(cx *layer4.Connection, next layer4.Handler) error {
	// The addresses a `proxy_protocol` handler learned from the hop in
	// front of Caddy, or the socket's own when there was none.
	down := l4proxyprotocol.GetConn(cx)
	// The innermost terminated TLS session is the client's.
	var cs *tls.ConnectionState
	if states := l4tls.GetConnectionStates(cx); len(states) > 0 {
		cs = states[len(states)-1]
	}
	header, err := Header(down.RemoteAddr(), down.LocalAddr(), cs)
	if err != nil {
		return fmt.Errorf("proxy_tlv: %w", err)
	}
	if h.logger != nil {
		if ce := h.logger.Check(zap.DebugLevel, "prepending PROXY v2 header"); ce != nil {
			ce.Write(zap.String("remote", down.RemoteAddr().String()), zap.Bool("tls", cs != nil), zap.Int("bytes", len(header)))
		}
	}
	// A fresh Connection rather than cx.Wrap: Wrap carries cx's prefetch
	// buffer over and replays it FIRST, which would put client bytes ahead
	// of the header. prefixConn reads from cx itself, so anything buffered
	// there still arrives, in order, after the header. Context carries the
	// vars and replacer along.
	return next.Handle(&layer4.Connection{
		Conn:    &prefixConn{Conn: cx, under: cx.Conn, prefix: header},
		Context: cx.Context,
		Logger:  cx.Logger,
	})
}

// Header formats the PROXY v2 header for one connection. cs nil means the
// client leg was not TLS: addresses only, no TLVs. Exported so the
// receiving side can test against exactly what the edge writes.
func Header(src, dst net.Addr, cs *tls.ConnectionState) ([]byte, error) {
	h := proxyproto.HeaderProxyFromAddrs(2, src, dst)
	if h.TransportProtocol == proxyproto.UNSPEC {
		return nil, fmt.Errorf("unsupported address pair %v → %v", src, dst)
	}
	if cs != nil {
		ssl, err := tlvparse.PP2SSL{
			Client: tlvparse.PP2_BITFIELD_CLIENT_SSL,
			// Non-zero: no client certificate was presented and verified.
			Verify: 1,
			TLV: []proxyproto.TLV{{
				Type:  proxyproto.PP2_SUBTYPE_SSL_VERSION,
				Value: []byte(tls.VersionName(cs.Version)),
			}},
		}.Marshal()
		if err != nil {
			return nil, err
		}
		tlvs := []proxyproto.TLV{ssl}
		if cs.ServerName != "" {
			tlvs = append(tlvs, proxyproto.TLV{Type: proxyproto.PP2_TYPE_AUTHORITY, Value: []byte(cs.ServerName)})
		}
		if cs.NegotiatedProtocol != "" {
			tlvs = append(tlvs, proxyproto.TLV{Type: proxyproto.PP2_TYPE_ALPN, Value: []byte(cs.NegotiatedProtocol)})
		}
		if err := h.SetTLVs(tlvs); err != nil {
			return nil, err
		}
	}
	return h.Format()
}

// prefixConn reads prefix before anything from the wrapped connection.
// Writes pass straight through.
type prefixConn struct {
	net.Conn          // the layer4.Connection: buffered bytes, then the socket
	under    net.Conn // what that Connection wraps (the *tls.Conn)
	mu       sync.Mutex
	prefix   []byte
}

// CloseWrite half-closes the client leg. `proxy` looks for it on the
// connection it is handed and calls it when the upstream hangs up; without
// it a client whose upstream refused it would sit on an open socket until
// its own timeout instead of seeing the close.
func (c *prefixConn) CloseWrite() error {
	if cw, ok := c.under.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (c *prefixConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(p)
}

// UnmarshalCaddyfile sets up the Handler from Caddyfile tokens. Syntax:
//
//	proxy_tlv
func (h *Handler) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	d.Next() // consume wrapper name
	if d.CountRemainingArgs() > 0 {
		return d.ArgErr()
	}
	if d.NextBlock(d.Nesting()) {
		return d.Err("proxy_tlv takes no options")
	}
	return nil
}

// Interface guards
var (
	_ caddy.Provisioner     = (*Handler)(nil)
	_ caddyfile.Unmarshaler = (*Handler)(nil)
	_ layer4.NextHandler    = (*Handler)(nil)
)
