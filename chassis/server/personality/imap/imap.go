// Package imap implements the chassis's IMAP head: a personality that
// serves a tenant's durable mailbox store (chassis/imap) to any IMAP4rev1
// client — Apple Mail, Thunderbird, mutt — so a mail client becomes a UI
// over state that stacks project into the store with txco://imap/* ops.
//
// The head is a GENERIC IMAP server: it knows no folder names and no
// product. Reads (LIST/SELECT/FETCH/SEARCH/IDLE) never touch the
// processor; they are served from the index columns cached at append and,
// for BODY[], from a deterministic render of the retained record. Writes
// that reach the store from an op are fanned out to selected sessions
// through a head-local hub (EXISTS / EXPUNGE / FETCH on Poll and IDLE).
//
// Phase 0a scope (authentication first): LOGIN against argon2id account
// rows with per-IP and per-account throttles, a verified-login cache and
// the tenant admission check; NAMESPACE, LIST, STATUS, SELECT, FETCH,
// SEARCH, STORE (flags, local policy), POLL/IDLE, SUBSCRIBE. Mutating
// verbs (CREATE/DELETE/RENAME/APPEND/COPY/MOVE/EXPUNGE) answer NO [CANNOT]
// until the observe/answer lanes land.
//
// IMAP is OFF by default. Both gates must be flipped:
//   - `imap` must appear in `--personalities`
//   - `--imap-listen-addrs` and/or `--imap-tls-addrs` must be non-empty
package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/pires/go-proxyproto"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/auth/throttle"
	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	chimap "github.com/loremlabs/thanks-computer/chassis/imap"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

const (
	// loginCacheTTL is how long a verified (username, hash, password)
	// triple skips argon2id.
	loginCacheTTL = 5 * time.Minute
	loginCacheMax = 10000
)

// Controller owns the IMAP listeners and their go-imap servers. It
// satisfies the server's Start()/Stop() controller contract.
type Controller struct {
	ctx   context.Context
	pu    *processor.Unit
	store *chimap.Store
	fcas  filecas.Store
	ix    blob.Index // tenant sha ownership rows for stored parts; may be nil
	lanes *lanes
	proxy []*net.IPNet // --imap-proxy-protocol trusted sources

	tlsConfig *tls.Config

	servers   []*imapserver.Server
	listeners []net.Listener
	wg        sync.WaitGroup

	loginIP   *throttle.Throttle
	loginAcct *throttle.Throttle
	cache     *loginCache
	conns     *connCounter
	hub       *hub

	logins metric.Int64Counter
}

// NewController constructs (but does not start) an IMAP controller. A nil
// store (imap personality not active) yields an inert controller whose
// Start is a no-op.
func NewController(ctx context.Context, pu *processor.Unit, store *chimap.Store) *Controller {
	var logger *zap.Logger
	if pu != nil {
		logger = pu.Logger
	}
	c := &Controller{
		ctx:   ctx,
		pu:    pu,
		store: store,
		cache: newLoginCache(loginCacheTTL, loginCacheMax),
		hub:   newHub(ctx, store, logger),
	}
	if pu != nil {
		c.loginIP = throttle.New(pu.Conf.IMAPLoginRate, time.Minute)
		c.loginAcct = throttle.New(pu.Conf.IMAPLoginRate, time.Minute)
		c.conns = newConnCounter(pu.Conf.IMAPMaxConnsPerAccount)
		c.lanes = newLanes(ctx, pu)
		for _, cidr := range nonEmpty(pu.Conf.IMAPProxyProtocol) {
			if _, n, err := net.ParseCIDR(cidr); err == nil {
				c.proxy = append(c.proxy, n)
			} else if ip := net.ParseIP(cidr); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				c.proxy = append(c.proxy, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			} else if pu.Logger != nil {
				pu.Logger.Warn("imap: ignoring unparseable --imap-proxy-protocol entry", zap.String("entry", cidr))
			}
		}
	}
	if pu != nil && pu.Mc != nil && pu.Mc.Meter != nil {
		c.logins, _ = pu.Mc.Meter.Int64Counter("chassis.imap.logins",
			metric.WithDescription("IMAP LOGIN attempts by outcome"),
			metric.WithUnit("1"))
	}
	return c
}

// SetFileCAS wires the content store BODY[] renders from. Nil-safe: without
// it, metadata still serves and BODY[] answers NO [UNAVAILABLE].
func (c *Controller) SetFileCAS(f filecas.Store) { c.fcas = f }

// SetTLSConfig wires the certificate source for --imap-tls-addrs (implicit
// TLS) and for STARTTLS on the plaintext listeners.
func (c *Controller) SetTLSConfig(t *tls.Config) { c.tlsConfig = t }

// SetBlobIndex wires the tenant sha-ownership index so message and part
// bytes a client APPENDs are owned by the account's tenant (readable
// through the blob plane by sha). Nil-safe.
func (c *Controller) SetBlobIndex(ix blob.Index) { c.ix = ix }

// Start binds the configured listeners and serves IMAP on each. The
// double-gate (personality string AND non-empty listen addrs) means an
// upgrade cannot acquire a new listener without explicit opt-in.
func (c *Controller) Start() {
	if c.pu == nil || !strings.Contains(c.pu.Conf.Personalities, "imap") {
		return
	}
	if c.store == nil {
		c.pu.Logger.Warn("imap personality enabled but no store opened; head not started")
		return
	}
	plain := nonEmpty(c.pu.Conf.IMAPListenAddrs)
	secure := nonEmpty(c.pu.Conf.IMAPTLSAddrs)
	if len(plain) == 0 && len(secure) == 0 {
		c.pu.Logger.Info("imap personality enabled but no listen addrs; head not started")
		return
	}
	if c.tlsConfig == nil && c.pu.Conf.IMAPSelfSigned {
		hosts := append([]string{}, devSelfSignedHosts...)
		if h := strings.TrimSpace(c.pu.Conf.IMAPHostname); h != "" {
			hosts = append(hosts, h)
		}
		// Stable files next to the index so the certificate survives
		// restarts and can be trusted once in the OS keychain.
		certPath, keyPath := SelfSignedPaths(c.pu.Conf.IMAPDBPath)
		t, minted, err := LoadOrMintSelfSigned(certPath, keyPath, hosts)
		if err != nil {
			c.pu.Logger.Error("imap: self-signed certificate failed; serving plaintext only", zap.String("err", err.Error()))
		} else {
			c.tlsConfig = t
			c.pu.Logger.Warn("imap: serving a SELF-SIGNED certificate (--imap-self-signed): STARTTLS on the plaintext listeners, IMAPS on --imap-tls-addrs. Dev only.",
				zap.String("cert", certPath), zap.Bool("minted", minted), zap.Strings("hosts", hosts))
		}
	}
	if len(secure) > 0 && c.tlsConfig == nil {
		c.pu.Logger.Warn("imap-tls-addrs set but no certificate source (cert files, the bundled cert manager, or --imap-self-signed); IMAPS listeners skipped",
			zap.Strings("addrs", secure))
		secure = nil
	}
	if len(plain) > 0 && c.pu.Conf.IMAPInsecureAuth {
		c.pu.Logger.Warn("imap: LOGIN accepted over plaintext (--imap-insecure-auth); only for loopback dev or behind a TLS-terminating proxy")
	}

	// The store tells the hub about every commit; the hub tells selected
	// sessions. Installed before the first listener so an op's append
	// can never race a SELECT's first Poll.
	c.store.SetOnChange(c.hub.onChange)
	c.hub.start(parseSyncInterval(c.pu.Conf.IMAPSyncInterval, c.pu.Logger))
	c.lanes.start()

	opts := &imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return newSession(c, conn), nil, nil
		},
		Caps: imap.CapSet{
			imap.CapIMAP4rev1:        {},
			imap.CapNamespace:        {},
			imap.CapUIDPlus:          {},
			imap.CapMove:             {},
			imap.CapChildren:         {},
			imap.CapSpecialUse:       {},
			imap.CapCreateSpecialUse: {},
			imap.CapAppendLimit:      {},
			imap.CapLiteralPlus:      {},
		},
		Logger:       &zapLog{logger: c.pu.Logger},
		TLSConfig:    c.tlsConfig, // STARTTLS on plaintext listeners when a cert exists
		InsecureAuth: c.pu.Conf.IMAPInsecureAuth,
	}
	if c.pu.Conf.IMAPWireDebug {
		c.pu.Logger.Warn("imap: wire debug is ON — every command and response, credentials included, is logged at DEBUG")
		opts.DebugWriter = &wireLog{logger: c.pu.Logger}
	}

	bind := func(addr string, secure bool) {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			c.pu.Logger.Fatal("imap socket already in use (or otherwise unbindable)",
				zap.String("addr", addr), zap.Bool("tls", secure), zap.String("err", err.Error()),
				zap.String("hint", "lsof -iTCP"+addr+" -sTCP:LISTEN"))
		}
		if len(c.proxy) > 0 {
			// PROXY header (v1/v2) from a trusted front proxy carries the
			// real client address; from anyone else the connection is
			// served as-is and a header is not honoured. Wraps BEFORE
			// TLS: the header precedes the handshake.
			ln = &proxyproto.Listener{
				Listener: ln,
				ConnPolicy: func(o proxyproto.ConnPolicyOptions) (proxyproto.Policy, error) {
					if c.trustedProxy(o.Upstream) {
						return proxyproto.REQUIRE, nil
					}
					return proxyproto.SKIP, nil
				},
			}
		}
		if secure {
			ln = tls.NewListener(ln, c.tlsConfig)
		}
		srv := imapserver.New(opts)
		c.servers = append(c.servers, srv)
		c.listeners = append(c.listeners, ln)
		c.pu.Logger.Info("imap controller started", zap.String("addr", ln.Addr().String()), zap.Bool("tls", secure))
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			if err := srv.Serve(ln); err != nil && !strings.Contains(err.Error(), "closed") {
				c.pu.Logger.Error("imap serve error", zap.String("addr", addr), zap.String("err", err.Error()))
			}
		}()
	}
	for _, a := range plain {
		bind(a, false)
	}
	for _, a := range secure {
		bind(a, true)
	}
}

// Stop closes the listeners and every connection. go-imap's Close is
// immediate (no drain); an IDLE client reconnects on its own.
func (c *Controller) Stop() {
	if c.pu == nil || !strings.Contains(c.pu.Conf.Personalities, "imap") || len(c.servers) == 0 {
		return
	}
	c.pu.Logger.Info("calling imap controller stop")
	for _, s := range c.servers {
		if err := s.Close(); err != nil {
			c.pu.Logger.Warn("imap close error", zap.String("err", err.Error()))
		}
	}
	c.wg.Wait()
	c.hub.stopTicker()
	c.lanes.stop()
	c.pu.Logger.Info("imap controller stopped")
}

// parseSyncInterval reads --imap-sync-interval: "" or a parse error ⇒ the
// 15s default (warned); "0" disables the tick.
func parseSyncInterval(v string, log *zap.Logger) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 15 * time.Second
	}
	if v == "0" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		if log != nil {
			log.Warn("invalid imap-sync-interval, using 15s", zap.String("value", v), zap.String("err", err.Error()))
		}
		return 15 * time.Second
	}
	return d
}

// trustedProxy reports whether a peer address is inside
// --imap-proxy-protocol.
func (c *Controller) trustedProxy(a net.Addr) bool {
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
	for _, n := range c.proxy {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// boundAddrs returns the actual bound addresses (tests bind ":0").
func (c *Controller) boundAddrs() []string {
	out := make([]string, 0, len(c.listeners))
	for _, ln := range c.listeners {
		if ln != nil {
			out = append(out, ln.Addr().String())
		}
	}
	return out
}

// noteLogin records a LOGIN outcome: a metric, and one Info line per
// attempt (what every mail server logs — the first thing to read when a
// client "can't verify the account").
func (c *Controller) noteLogin(outcome, user, ip string, tlsOn bool) {
	if c.logins != nil {
		c.logins.Add(c.ctx, 1, metric.WithAttributes(attrOutcome(outcome)))
	}
	if c.pu != nil && c.pu.Logger != nil {
		c.pu.Logger.Info("imap login",
			zap.String("user", user), zap.String("ip", ip), zap.String("outcome", outcome), zap.Bool("tls", tlsOn))
	}
}

// SelfSignedPaths derives the dev certificate + key file paths from the
// index path (`imap.db` → `imap-selfsigned.crt` / `.key` beside it).
func SelfSignedPaths(dbPath string) (certPath, keyPath string) {
	dir := filepath.Dir(dbPath)
	if dbPath == "" {
		dir = "./chassis/data"
	}
	return filepath.Join(dir, "imap-selfsigned.crt"), filepath.Join(dir, "imap-selfsigned.key")
}

// nonEmpty drops blank entries (viper's []string parsing can yield a
// single "" element when the flag is set explicitly empty).
func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// zapLog adapts the chassis logger to go-imap's Logger.
type zapLog struct{ logger *zap.Logger }

func (z *zapLog) Printf(format string, v ...interface{}) {
	msg := fmt.Sprintf(format, v...)
	switch {
	case strings.Contains(msg, "failed to write greeting") && strings.Contains(msg, "EOF"):
		// The client hung up before the first byte reached it: on a TLS
		// listener that is a handshake the client abandoned — almost always
		// a certificate it does not trust (dev: trust the self-signed one
		// once). Not worth a WARN per attempt; mail clients retry.
		z.logger.Info("imap: client closed the connection before the greeting (TLS handshake abandoned — a certificate the client does not trust?)")
	case strings.Contains(msg, "failed to read command") && strings.Contains(msg, "EOF"):
		z.logger.Debug("imap: client disconnected")
	default:
		z.logger.Warn("imap lib: " + msg)
	}
}

// wireLog is go-imap's DebugWriter: raw protocol bytes, one DEBUG line per
// write (lines are CRLF-terminated on the wire; trailing space trimmed).
type wireLog struct{ logger *zap.Logger }

func (w *wireLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			w.logger.Debug("imap wire: " + line)
		}
	}
	return len(p), nil
}
