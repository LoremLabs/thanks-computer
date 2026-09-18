package tcp

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/edgeproxy"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/hxid"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
	txtls "github.com/loremlabs/thanks-computer/chassis/tls"
	"github.com/loremlabs/thanks-computer/chassis/units"
)

// MAX_MESSAGE_SIZE is the longest line the head accepts. A longer line is
// consumed and dropped; the connection stays open.
const MAX_MESSAGE_SIZE = units.MB * 10

// limits are the parsed --tcp-* knobs.
type limits struct {
	connectTimeout    time.Duration // the connect run's rule response
	respTimeout       time.Duration // each line's rule response (and our write)
	idleTimeout       time.Duration // silence between lines
	drainTimeout      time.Duration // Stop() waits this long for handlers to unwind
	handshakeTimeout  time.Duration // TLS handshake (;tls) or PROXY header (;proxy=)
	maxConns          int           // open connections on this node; 0 = unlimited
	maxConnsPerTenant int           // open connections per routed tenant; 0 = unlimited
	maxLine           int           // longest accepted line, bytes
	connFuel          int64         // fuel charged per accepted connection; 0 = none
	maxRunFuel        int64         // --max-fuel-per-request; bounds what one run is pre-charged
}

// TCPController is the raw socket head. It owns every accepted net.Conn
// end to end: the accept loop admits it, the connect run through
// _sys/boot decides whether it stays open (and which tenant and stack it
// belongs to), and the listener's protocol Handler — by default the line
// handler, one bounded event per newline-terminated line — serves it
// until the stack hangs up, the peer goes quiet, or the chassis shuts
// down.
//
// A `;tls` listener terminates TLS here and stamps the SNI hostname as
// `@tcp.host`, the connection's routing fact. A `;proxy=` listener sits
// behind an edge that already terminated TLS and fills the same fields
// from the edge's PROXY v2 header (chassis/edgeproxy), so a stack never
// sees which side did the handshake. Routing itself stays in
// `_sys/boot`: detect-tenant reads `@tcp.host`, route promotes it, and
// the head only learns the outcome (event.DispatchResult).
type TCPController struct {
	ctx   context.Context
	pu    *processor.Unit
	lim   limits
	specs []listenerSpec

	// tlsConfig serves managed certificates by SNI for `;tls` listeners
	// (SetTLSConfig, from the bundled cert manager). selfSignedDir is
	// where a `;self-signed` listener keeps its dev certificate.
	tlsConfig     *tls.Config
	selfSigned    *tls.Config
	selfSignedDir string

	// resolver re-checks a pinned connection's route before each of its
	// events (SetResolver). nil: nothing is re-checked.
	resolver RouteResolver

	mu        sync.Mutex
	listeners []net.Listener
	addrs     []net.Addr
	conns     map[*connection]struct{}
	perTenant map[string]int

	wg       sync.WaitGroup // one per accept loop + one per open connection
	stopping atomic.Bool
}

// connection is one accepted socket plus the facts the head stamped on it
// and the tenant the connect run pinned it to.
type connection struct {
	net.Conn
	rid     string
	spec    listenerSpec
	handler Handler
	tls     tlsFacts

	// host is the hostname the client asked for, raw: the SNI we saw or
	// the edge's AUTHORITY. hostSource ("sni" | "edge") is for the log
	// only — it never reaches the envelope. canonHost is host as the
	// routing fact, `@tcp.host`; "" when there is none or it is not a
	// hostname.
	host, hostSource string
	canonHost        string

	tenant, stack string // "" until the connect run routed

	// route is the connect run's outcome, pre-stamped on every later
	// event; revalidate says the resolver produced it and is asked again
	// before each one (pin).
	route      RouteStamp
	revalidate bool

	meter meter
}

// CloseWrite half-closes the stream — the peer reads EOF while we can
// still read — where the transport underneath can: TCP, TLS
// (close_notify), or either behind a PROXY header. A handler that hangs
// up with the peer's bytes unread uses it to say goodbye first; a plain
// Close then would reset the connection, and a reset can cost the peer
// the tail of what we wrote.
func (c *connection) CloseWrite() error {
	switch u := c.Conn.(type) {
	case interface{ CloseWrite() error }:
		return u.CloseWrite()
	case interface{ TCPConn() (*net.TCPConn, bool) }:
		if t, ok := u.TCPConn(); ok {
			return t.CloseWrite()
		}
	}
	return errors.ErrUnsupported
}

// RouteResolver is the data-plane resolver detect-tenant uses
// (ingress.DBResolver.ResolveErr); the head asks it whether a pinned
// connection's route still stands.
type RouteResolver interface {
	ResolveErr(key ingress.RouteKey) (ingress.RouteTarget, bool, error)
}

// tlsFacts is what the handshake observed — ours, or the one the edge
// reported; provenance for @tcp.tls.*.
type tlsFacts struct {
	enabled bool
	sni     string
	alpn    string
	version string
}

func NewController(ctx context.Context, pu *processor.Unit) *TCPController {
	specs, err := parseListenerSpecs(pu.Conf.TCPListenAddrs)
	if err != nil {
		pu.Logger.Fatal("invalid --tcp-listen-addrs entry", zap.String("err", err.Error()))
	}
	return &TCPController{
		ctx:           ctx,
		pu:            pu,
		lim:           parseLimits(pu),
		specs:         specs,
		selfSignedDir: dataDir(pu.Conf),
		conns:         map[*connection]struct{}{},
		perTenant:     map[string]int{},
	}
}

// parseLimits reads the --tcp-* knobs once. A bad duration warns and
// falls back to its documented default (the websocket head's idiom);
// config.Load already refuses to boot on an unparsable one, so the
// fallback only matters for hand-built Units in tests.
func parseLimits(pu *processor.Unit) limits {
	conf := pu.Conf
	dur := func(name, v string, def time.Duration) time.Duration {
		v = strings.TrimSpace(v)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			if pu.Logger != nil {
				pu.Logger.Warn("invalid "+name+", using default",
					zap.String("value", v), zap.Duration("default", def))
			}
			return def
		}
		return d
	}
	return limits{
		connectTimeout:    dur("tcp-connect-resp-timeout", conf.TCPConnectRespTimeout, 3*time.Second),
		respTimeout:       dur("tcp-resp-timeout", conf.TCPRespTimeout, 10*time.Second),
		idleTimeout:       dur("tcp-max-idle-timeout", conf.TCPMaxIdleTimeout, 5*time.Second),
		drainTimeout:      dur("tcp-drain-timeout", conf.TCPDrainTimeout, 5*time.Second),
		handshakeTimeout:  dur("tcp-handshake-timeout", conf.TCPHandshakeTimeout, 5*time.Second),
		maxConns:          conf.TCPMaxConns,
		maxConnsPerTenant: conf.TCPMaxConnsPerTenant,
		maxLine:           MAX_MESSAGE_SIZE,
		connFuel:          int64(max(conf.TCPConnFuel, 0)),
		maxRunFuel:        int64(max(conf.MaxFuelPerRequest, 0)),
	}
}

// dataDir is where the bundled stores live: the parent of --db-root-dir
// (./chassis/data by default, .txco/dev under `txco dev`). The dev
// certificate sits beside them, so it survives restarts and never lands
// in a workspace's source tree.
func dataDir(conf config.Config) string {
	if p := strings.TrimSpace(conf.DbRoot); p != "" {
		return filepath.Dir(filepath.Clean(p))
	}
	return "./chassis/data"
}

// SetTLSConfig wires the managed certificate source for `;tls` listeners.
// Call before Start.
func (tcp *TCPController) SetTLSConfig(t *tls.Config) { tcp.tlsConfig = t }

// SetResolver wires the resolver the head re-checks pinned routes with.
// Call before Start.
func (tcp *TCPController) SetResolver(r RouteResolver) { tcp.resolver = r }

// WantsManagedTLS reports whether any listener needs the bundled cert
// manager (`;tls` without `;self-signed`), so the server can build it.
func (tcp *TCPController) WantsManagedTLS() bool {
	if !tcp.pu.Conf.HasPersonality("tcp") {
		return false
	}
	for _, s := range tcp.specs {
		if s.TLS && !s.SelfSigned {
			return true
		}
	}
	return false
}

// tlsFor picks the certificate source for one listener, minting the dev
// certificate once on first use. nil means the listener cannot serve.
func (tcp *TCPController) tlsFor(spec listenerSpec) *tls.Config {
	if !spec.SelfSigned {
		return tcp.tlsConfig
	}
	if tcp.selfSigned == nil {
		certPath := filepath.Join(tcp.selfSignedDir, "tcp-selfsigned.crt")
		keyPath := filepath.Join(tcp.selfSignedDir, "tcp-selfsigned.key")
		t, minted, err := txtls.LoadOrMintSelfSigned(certPath, keyPath, txtls.DevSelfSignedHosts)
		if err != nil {
			tcp.pu.Logger.Error("tcp: self-signed certificate failed", zap.String("err", err.Error()))
			return nil
		}
		tcp.pu.Logger.Warn("tcp: serving a SELF-SIGNED certificate (;self-signed). Dev only.",
			zap.String("cert", certPath), zap.Bool("minted", minted), zap.Strings("hosts", txtls.DevSelfSignedHosts))
		tcp.selfSigned = t
	}
	return tcp.selfSigned
}

// Start binds every --tcp-listen-addrs entry and starts an accept loop
// per listener. Binding happens synchronously so a port conflict is a
// clear Fatal before anything logs "started", and so Addrs() is valid
// the moment Start returns.
func (tcp *TCPController) Start() {
	if !tcp.pu.Conf.HasPersonality("tcp") {
		return
	}
	seenNames := map[string]string{} // name -> first addr (for collision warning)
	for _, spec := range tcp.specs {
		if prev, dup := seenNames[spec.Name]; dup {
			// Multiple listeners sharing a name still bind fine, but
			// ingress can't tell their traffic apart. Most likely a
			// config typo; warn loudly rather than failing.
			tcp.pu.Logger.Warn("two tcp listeners share a name; ingress routing cannot distinguish them",
				zap.String("name", spec.Name),
				zap.String("first", prev),
				zap.String("second", spec.Addr),
				zap.String("hint", "use name=addr form, e.g. webhooks=:5050,iot=:5051"))
		}
		seenNames[spec.Name] = spec.Addr

		var cfg *tls.Config
		if spec.TLS {
			if cfg = tcp.tlsFor(spec); cfg == nil {
				tcp.pu.Logger.Fatal("tcp listener asks for tls but no certificate source is available",
					zap.String("listen", spec.Addr), zap.String("name", spec.Name),
					zap.String("hint", "the bundled cert manager needs --web-tls-addr or --imap-tls-addrs (with the dns personality); for local development use ';self-signed'"))
			}
		}
		l, err := net.Listen("tcp", spec.Addr)
		if err != nil {
			tcp.pu.Logger.Fatal("tcp port already in use (or otherwise unbindable)",
				zap.String("listen", spec.Addr),
				zap.String("name", spec.Name),
				zap.String("err", err.Error()),
				zap.String("hint", "lsof -iTCP"+spec.Addr+" -sTCP:LISTEN"))
		}
		// A no-op without `;proxy=`. The header read shares the handshake
		// budget: both are "the peer has this long to say who it is".
		l = edgeproxy.Wrap(l, spec.Proxy, tcp.lim.handshakeTimeout)
		if cfg != nil {
			l = tls.NewListener(l, cfg)
		}
		tcp.mu.Lock()
		tcp.listeners = append(tcp.listeners, l)
		tcp.addrs = append(tcp.addrs, l.Addr())
		tcp.mu.Unlock()

		// parseListenerSpec already refused an unknown handler= name.
		newHandler, err := lookupHandler(spec.Handler)
		if err != nil {
			tcp.pu.Logger.Fatal("tcp listener handler", zap.String("name", spec.Name), zap.String("err", err.Error()))
		}
		h := newHandler(tcp.pu)
		if lh, ok := h.(*lineHandler); ok {
			lh.lim = tcp.lim // the head's parsed knobs, not a second parse
		}

		tcp.pu.Logger.Info("tcp controller started",
			zap.String("listen", spec.Addr),
			zap.String("name", spec.Name),
			zap.String("handler", spec.Handler),
			zap.String("inlet", handlerInlet(spec.Handler)),
			zap.Bool("tls", spec.TLS),
			zap.Bool("proxy", len(spec.Proxy) > 0))

		tcp.wg.Add(1)
		go tcp.acceptLoop(l, spec, h)
	}
}

// Addrs reports the bound addresses in --tcp-listen-addrs order, valid
// once Start has returned. Tests bind ":0" and read the port here.
func (tcp *TCPController) Addrs() []net.Addr {
	tcp.mu.Lock()
	defer tcp.mu.Unlock()
	return append([]net.Addr(nil), tcp.addrs...)
}

func (tcp *TCPController) acceptLoop(l net.Listener, spec listenerSpec, h Handler) {
	defer tcp.wg.Done()
	for {
		c, err := l.Accept()
		if err != nil {
			if tcp.stopping.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			// A transient accept failure (fd exhaustion, say) must not
			// spin the loop or kill the listener: back off and retry.
			tcp.pu.Logger.Warn("tcp accept error",
				zap.String("listen", spec.Addr), zap.String("err", err.Error()))
			select {
			case <-time.After(100 * time.Millisecond):
			case <-tcp.ctx.Done():
				return
			}
			continue
		}
		tcp.admit(c, spec, h)
	}
}

// admit is the accept-time gate. It runs BEFORE the TLS handshake and
// the connect event so a draining or full node answers in one line
// instead of spending a handshake and a pipeline run on a connection it
// is about to drop.
func (tcp *TCPController) admit(c net.Conn, spec listenerSpec, h Handler) {
	// Off the accept loop: on a `;proxy=` listener the first Write (and
	// RemoteAddr) waits for the peer's PROXY header, and a silent peer
	// must not stall accepts. A `;tls` listener gets no line at all — it
	// would cost the handshake we are refusing to spend, and a plaintext
	// line is noise to a TLS client; the close says enough.
	refuse := func(line, why string) {
		tcp.wg.Add(1)
		go func() {
			defer tcp.wg.Done()
			if !spec.TLS {
				_ = c.SetDeadline(time.Now().Add(time.Second))
				_, _ = c.Write([]byte(line + "\n"))
			}
			remote := c.RemoteAddr().String()
			_ = c.Close()
			tcp.pu.Logger.Info("tcp connection refused",
				zap.String("listener", spec.Name),
				zap.String("remote", remote),
				zap.String("why", why))
		}()
	}
	if tcp.stopping.Load() || admission.IsDraining() {
		refuse("503 draining", "draining")
		return
	}
	conn := &connection{Conn: c, rid: hxid.NewTimeSort().String(), spec: spec, handler: h}
	if !tcp.register(conn) {
		refuse("503 too many connections", "capped")
		return
	}
	tcp.wg.Add(1)
	go tcp.serve(conn)
}

// register takes a node slot for the connection; false when the cap is
// hit. 0 = unlimited (the imap connCounter convention).
func (tcp *TCPController) register(c *connection) bool {
	tcp.mu.Lock()
	defer tcp.mu.Unlock()
	if tcp.lim.maxConns > 0 && len(tcp.conns) >= tcp.lim.maxConns {
		return false
	}
	tcp.conns[c] = struct{}{}
	return true
}

// reserve takes a per-tenant slot once the connect run has routed the
// connection; false when the tenant's cap is hit.
func (tcp *TCPController) reserve(tenant string) bool {
	tcp.mu.Lock()
	defer tcp.mu.Unlock()
	if tcp.lim.maxConnsPerTenant > 0 && tcp.perTenant[tenant] >= tcp.lim.maxConnsPerTenant {
		return false
	}
	tcp.perTenant[tenant]++
	return true
}

func (tcp *TCPController) unregister(c *connection) {
	tcp.mu.Lock()
	defer tcp.mu.Unlock()
	delete(tcp.conns, c)
	if c.tenant == "" {
		return
	}
	if tcp.perTenant[c.tenant] <= 1 {
		delete(tcp.perTenant, c.tenant)
		return
	}
	tcp.perTenant[c.tenant]--
}

// handshake completes TLS on a `;tls` listener under a deadline and
// records what the ClientHello said. The SNI becomes the routing
// hostname only after canonicalisation (facts).
func (tcp *TCPController) handshake(c *connection) error {
	tc, ok := c.Conn.(*tls.Conn)
	if !ok {
		return errors.New("tls listener produced a non-TLS connection")
	}
	ctx, cancel := context.WithTimeout(tcp.ctx, tcp.lim.handshakeTimeout)
	defer cancel()
	if err := tc.HandshakeContext(ctx); err != nil {
		return err
	}
	cs := tc.ConnectionState()
	c.tls = tlsFacts{
		enabled: true,
		sni:     cs.ServerName,
		alpn:    cs.NegotiatedProtocol,
		version: tls.VersionName(cs.Version),
	}
	if cs.ServerName != "" {
		c.host, c.hostSource = cs.ServerName, "sni"
	}
	return nil
}

// errNotFronted closes a `;proxy=` connection nobody vouched for.
var errNotFronted = errors.New("no PROXY header from a trusted edge")

// edgeFacts is handshake's counterpart on a `;proxy=` listener: the edge
// terminated TLS and its PROXY v2 header says what it observed. The
// listener is an edge-only door, so a connection without a header from
// the trusted networks — an outsider, or the edge's own LOCAL probe — is
// closed here, before any pipeline run. (A header an outsider sends is
// never parsed; see edgeproxy.Wrap.)
func (tcp *TCPController) edgeFacts(c *connection) error {
	f, err := edgeproxy.Read(c.Conn)
	if err != nil {
		return err
	}
	if !f.Present {
		return errNotFronted
	}
	c.tls = tlsFacts{enabled: f.TLS, alpn: f.ALPN, version: f.TLSVersion}
	if f.TLS {
		c.tls.sni = f.Authority
	}
	if f.Authority != "" {
		c.host, c.hostSource = f.Authority, "edge"
	}
	return nil
}

// stamp writes the facts every event on this connection carries: the
// source, the listener the ingress router keys on, the inlet the
// listener's handler asks for, the socket addresses, and — on TLS — what
// the handshake observed. Chassis-stamped; none of it is author-writable,
// and it goes on after whatever a handler contributed (emit), so none of
// it is handler-writable either. Two hostname fields on purpose:
// `tcp.tls.sni` is the raw observation, `tcp.host` the canonical routing
// fact detect-tenant reads. Both come from our handshake or from the
// edge's PROXY header, and the envelope does not say which. On a
// `;proxy=` listener the socket addresses below are the header's too: the
// real client, and the address it dialled at the edge.
func (tcp *TCPController) stamp(b *jsonx.Builder, c *connection, src string) {
	b.Set("_txc.src", src)
	b.Set("_ts", time.Now().Format(time.RFC3339))
	b.Set("_txc.rid", c.rid)
	// Listener name is what the ingress router keys on when there is no
	// hostname. Operator names come from `name=addr` entries in
	// --tcp-listen-addrs; bare addresses keep the back-compat "default".
	b.Set("_txc.tcp.listener", c.spec.Name)
	// With a hostname, the router enters that stack's inlet for the
	// listener's protocol: `_tcp`, or `_NAME` under `;handler=NAME`.
	b.Set("_txc.tcp.inlet", handlerInlet(c.spec.Handler))
	// Private-fields plumbing: same pattern as the web inlet — chassis
	// config decides whether to stamp.
	if tcp.pu.Conf.DebugPrivate {
		b.Set("_txc.flag_private", true)
	}
	if ra, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		b.Set("_txc.client.ip", ra.IP.String())
		b.Set("_txc.tcp.remote.port", ra.Port)
	}
	// Local addr (port and ip the client connected TO). Rules that want
	// to route on the raw port without operator-side YAML read these.
	if la, ok := c.LocalAddr().(*net.TCPAddr); ok {
		b.Set("_txc.tcp.local.ip", la.IP.String())
		b.Set("_txc.tcp.local.port", la.Port)
	}
	b.Set("_txc.tcp.tls.enabled", c.tls.enabled)
	if c.tls.enabled {
		b.Set("_txc.tcp.tls.version", c.tls.version)
		if c.tls.alpn != "" {
			b.Set("_txc.tcp.tls.alpn", c.tls.alpn)
		}
		if c.tls.sni != "" {
			b.Set("_txc.tcp.tls.sni", c.tls.sni)
		}
	}
	if c.canonHost != "" {
		b.Set("_txc.tcp.host", c.canonHost)
	}
}

// serve owns one admitted connection from handshake to close.
func (tcp *TCPController) serve(c *connection) {
	defer tcp.wg.Done()
	defer tcp.unregister(c)
	defer func() { _ = c.Close() }()

	switch {
	case c.spec.TLS:
		if err := tcp.handshake(c); err != nil {
			tcp.pu.Logger.Info("tcp tls handshake failed",
				zap.String("rid", c.rid), zap.String("listener", c.spec.Name),
				zap.String("remote", c.RemoteAddr().String()), zap.String("err", err.Error()))
			return
		}
	case len(c.spec.Proxy) > 0:
		if err := tcp.edgeFacts(c); err != nil {
			tcp.pu.Logger.Info("tcp connection not from the edge; closed",
				zap.String("rid", c.rid), zap.String("listener", c.spec.Name),
				zap.String("remote", c.RemoteAddr().String()), zap.String("err", err.Error()))
			return
		}
	}
	if host, ok := tenants.CanonicalizeHost(c.host); ok {
		c.canonHost = host
	}

	b := jsonx.New()
	tcp.stamp(b, c, "tcp")
	// The connection's own cost rides its runs (meter): accepting it is
	// charged here, on the connect run.
	c.meter.add(tcp.lim.connFuel)
	tcp.precharge(b, c)
	payload := b.String()
	if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
		tcp.pu.Logger.Debug("tcp connection", zap.String("payload", payload))
	}

	// Connect run: the stack's first look at the connection, before any
	// protocol byte, and the one place the connection is routed — whatever
	// the listener's handler, this is a `tcp` event answered on
	// `@tcp.res.*`. The head trusts the bus loop's DispatchResult for the
	// outcome, never the envelope. Accept is implicit — the connection
	// stays open unless the run left it in _sys (unrouted: fail closed),
	// was denied, failed, or asked to close — so adding a listener never
	// forces every stack to write an accept rule. A `@tcp.res.write` here
	// is the greeting; nothing else is echoed.
	res, err := tcp.dispatch(c, "tcp", payload, tcp.lim.connectTimeout)
	if err != nil {
		tcp.decided(c, "failed")
		return
	}
	if res.Tenant == "" || res.Tenant == tenants.SystemTenantSlug ||
		gjson.Get(res.Payload.Raw, "_txc.route.unavailable").Bool() {
		tcp.decided(c, "unrouted")
		return
	}
	v := verdictWriter{
		conn: c, rid: c.rid, logger: tcp.pu.Logger, timeout: tcp.lim.respTimeout,
		hidePrivate: !strings.Contains(tcp.pu.Conf.WebDebug, "SHOW_PRIVATE_VARS"),
	}
	c.tenant, c.stack = res.Tenant, res.Stack
	if !tcp.reserve(c.tenant) {
		c.tenant = "" // never reserved; unregister must not release
		v.write([]byte("503 too many connections\n"))
		tcp.decided(c, "capped")
		return
	}
	// Shared admission gate denial: TCP has no standard rejection, so
	// write a short "<status> <reason>" line and close.
	if status, reason, denied := admission.Denied(res.Payload.Raw); denied {
		v.write([]byte(strconv.Itoa(status) + " " + reason + "\n"))
		tcp.decided(c, "denied")
		return
	}
	if why := v.apply(res.Payload.Raw, false); why != "" {
		tcp.decided(c, why)
		return
	}
	tcp.pin(c, res)
	tcp.decided(c, "accepted")

	// From here the bytes are the handler's.
	ctx := context.WithValue(tcp.ctx, config.CtxKeyRid, c.rid)
	why := "done"
	if err := tcp.serveProtocol(ctx, c); err != nil {
		why = err.Error()
	}
	in, out, fuel, unbilled := c.meter.totals()
	tcp.pu.Logger.Info("tcp connection closed", zap.String("rid", c.rid),
		zap.String("handler", c.spec.Handler), zap.String("why", why),
		zap.Int64("bytes_in", in), zap.Int64("bytes_out", out),
		zap.Int64("conn_fuel", fuel), zap.Int64("conn_fuel_unbilled", unbilled))
}

// serveProtocol runs the listener's handler on an accepted connection. A
// handler is protocol code reading a stranger's bytes; if it panics, that
// connection is lost, not the chassis.
func (tcp *TCPController) serveProtocol(ctx context.Context, c *connection) (err error) {
	defer func() {
		if r := recover(); r != nil {
			tcp.pu.Logger.Error("tcp handler panicked", zap.String("rid", c.rid),
				zap.String("handler", c.spec.Handler), zap.Any("panic", r),
				zap.ByteString("stack", debug.Stack()))
			err = errors.New("handler panicked")
		}
	}()
	return c.handler.ServeConn(ctx, tcp.routedConn(c))
}

// pin records the route the connect run chose. Every later event on the
// connection carries it pre-stamped (emit), so an event under a handler's
// own `@src` lands in the same stack without detect-tenant having to know
// that source, and what the head checked is what runs.
//
// Pinning must not outlive the opt-in, though: if the resolver vouches
// for this route now, it is asked again before each event, and the
// connection closes once it stops agreeing — the inlet deactivated, the
// hostname revoked or re-bound. A route the resolver does not reproduce
// came from an operator's own `_sys/boot` rule; nothing here can re-check
// that, so it stands for the connection's life, like a websocket
// session's.
func (tcp *TCPController) pin(c *connection, res event.DispatchResult) {
	c.route = RouteStamp{
		Tenant: res.Tenant, Stack: res.Stack,
		Ingress: res.Ingress, HostnameVerified: res.HostnameVerified,
	}
	stands, err := tcp.routeStands(c)
	c.revalidate = stands && err == nil
}

// routeKey is the key detect-tenant resolved the connect run with.
func (c *connection) routeKey() ingress.RouteKey {
	return ingress.RouteKey{
		Src:      "tcp",
		Hostname: c.canonHost,
		Listener: c.spec.Name,
		Inlet:    handlerInlet(c.spec.Handler),
	}
}

// routeStands asks the resolver whether the connection's pinned route is
// still the answer. err is a transient lookup failure: not evidence
// either way.
func (tcp *TCPController) routeStands(c *connection) (bool, error) {
	if tcp.resolver == nil {
		return false, nil
	}
	t, ok, err := tcp.resolver.ResolveErr(c.routeKey())
	if err != nil {
		return false, err
	}
	return ok && t.Tenant == c.route.Tenant && t.Stack == c.route.Stack, nil
}

func (tcp *TCPController) routedConn(c *connection) RoutedConn {
	rc := RoutedConn{
		Conn:       c,
		RID:        c.rid,
		Listener:   c.spec.Name,
		Tenant:     c.route.Tenant,
		Stack:      c.route.Stack,
		Host:       c.canonHost,
		TLS:        c.tls.enabled,
		ALPN:       c.tls.alpn,
		TLSVersion: c.tls.version,
		Route:      c.route,
		Emit: func(ctx context.Context, ev Event) (event.DispatchResult, error) {
			return tcp.emit(ctx, c, ev)
		},
		AddFuel: c.meter.add,
	}
	if ra, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		rc.ClientIP, rc.RemotePort = ra.IP.String(), ra.Port
	}
	if la, ok := c.LocalAddr().(*net.TCPAddr); ok {
		rc.LocalPort = la.Port
	}
	return rc
}

// emit is RoutedConn.Emit: one event on a pinned connection.
func (tcp *TCPController) emit(ctx context.Context, c *connection, ev Event) (event.DispatchResult, error) {
	src := handlerSrc(c.spec.Handler)
	if src == "tcp" && len(ev.Facts) > 0 {
		return event.DispatchResult{}, errors.New("tcp: the line handler has no facts of its own; @tcp.* is the head's")
	}
	if c.revalidate {
		// A transient lookup failure is not evidence the route changed;
		// the next event asks again.
		if stands, err := tcp.routeStands(c); err == nil && !stands {
			return event.DispatchResult{}, ErrRerouted
		}
	}

	// Handler facts first, the head's on top: nothing a handler passes can
	// displace a chassis-stamped field.
	b := jsonx.New()
	for k, val := range ev.Facts {
		b.Set("_txc."+src+"."+k, val)
	}
	if ev.Body != nil {
		b.Set("_txc.client.body", base64.StdEncoding.EncodeToString(ev.Body))
	}
	tcp.stamp(b, c, src)
	tcp.precharge(b, c)
	// Pre-stamped the websocket head's way: detect-tenant sees a route
	// already proposed and leaves it, boot/100 promotes it.
	b.Set("_txc.route.tenant", c.route.Tenant)
	b.Set("_txc.route.stack", c.route.Stack)
	b.Set("_txc.route.ingress", c.route.Ingress)
	b.Set("_txc.route.hostname_verified", c.route.HostnameVerified)
	b.Set("_txc.route.to", c.route.Stack+"/0")

	res, err := tcp.dispatchCtx(ctx, c, src, b.String(), tcp.lim.respTimeout)
	if err != nil {
		return event.DispatchResult{}, err
	}
	// The run is the last word: a tenant removed since the last event
	// never leaves _sys, and whatever _sys/boot produced is not this
	// connection's answer.
	if res.Tenant != c.route.Tenant || res.Stack != c.route.Stack {
		tcp.pu.Logger.Info("tcp event rerouted", zap.String("rid", c.rid),
			zap.String("tenant", c.route.Tenant), zap.String("stack", c.route.Stack),
			zap.String("now_tenant", res.Tenant), zap.String("now_stack", res.Stack))
		return event.DispatchResult{}, ErrRerouted
	}
	if status, reason, denied := admission.Denied(res.Payload.Raw); denied {
		return event.DispatchResult{}, &DeniedError{Status: status, Reason: reason}
	}
	return res, nil
}

// decided is the one structured line per connection at accept-decision
// time. Never payload bytes.
func (tcp *TCPController) decided(c *connection, decision string) {
	tcp.pu.Logger.Info("tcp connection "+decision,
		zap.String("rid", c.rid),
		zap.String("listener", c.spec.Name),
		zap.String("handler", c.spec.Handler),
		zap.String("remote", c.RemoteAddr().String()),
		zap.Bool("tls", c.tls.enabled),
		zap.String("host", c.host),
		zap.String("host_source", c.hostSource),
		zap.String("tenant", c.tenant),
		zap.String("stack", c.stack),
		zap.String("decision", decision))
}

var (
	errStopping   = errors.New("shutdown")
	errRunTimeout = errors.New("response timeout")
	errRunFailed  = errors.New("run failed")
)

// dispatch runs one event through the bus and waits for the bus loop's
// DispatchResult. An error means the connection is finished: the run
// timed out, the chassis is shutting down, or the pipeline failed.
func (tcp *TCPController) dispatch(c *connection, src, payload string, timeout time.Duration) (event.DispatchResult, error) {
	return tcp.dispatchCtx(tcp.ctx, c, src, payload, timeout)
}

// dispatchCtx is dispatch under a handler's context, which derives from
// tcp.ctx so chassis shutdown cancels an in-flight wait either way.
func (tcp *TCPController) dispatchCtx(parent context.Context, c *connection, src, payload string, timeout time.Duration) (event.DispatchResult, error) {
	if tcp.stopping.Load() {
		return event.DispatchResult{}, errStopping
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ctx = context.WithValue(ctx, config.CtxKeyRid, c.rid)

	// Buffered: an answer that lands after our timeout is dropped instead
	// of parking the bus loop on a send nobody reads.
	resCh := make(chan event.DispatchResult, 1)
	env := event.PackageJSON(ctx, payload, nil, src)
	env.ResultCh = resCh
	select {
	case tcp.pu.Bus <- env:
	case <-ctx.Done():
		tcp.pu.Logger.Info("tcp dispatch abandoned", zap.String("rid", c.rid), zap.String("err", ctx.Err().Error()))
		return event.DispatchResult{}, tcp.waitErr()
	}
	if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
		tcp.pu.Logger.Debug("sent to processors", zap.String("payload", payload))
	}
	select {
	case res := <-resCh:
		if res.Err != nil {
			tcp.pu.Logger.Warn("tcp run failed", zap.String("rid", c.rid), zap.String("err", res.Err.Error()))
			return event.DispatchResult{}, errRunFailed
		}
		if tcp.pu.Logger.Core().Enabled(zap.DebugLevel) {
			tcp.pu.Logger.Debug("tcp res", zap.String("response", res.Payload.Raw),
				zap.String("tenant", res.Tenant), zap.String("stack", res.Stack))
		}
		return res, nil
	case <-ctx.Done():
		return event.DispatchResult{}, tcp.waitErr()
	}
}

// waitErr names why a wait on the bus ended early.
func (tcp *TCPController) waitErr() error {
	if tcp.ctx.Err() != nil {
		return errStopping
	}
	return errRunTimeout
}

// Stop closes the listeners, then every open connection, and waits for
// their handlers to unwind, bounded by --tcp-drain-timeout. The server
// has already run the in-flight drain and cancelled ctx by the time this
// is called, so any run still waiting on the bus is already unblocking.
func (tcp *TCPController) Stop() {
	if !tcp.pu.Conf.HasPersonality("tcp") {
		return
	}
	tcp.pu.Logger.Info("calling tcp controller stop")
	tcp.stopping.Store(true)

	tcp.mu.Lock()
	for _, l := range tcp.listeners {
		if err := l.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			tcp.pu.Logger.Error("listeners close error", zap.String("err", err.Error()))
		}
	}
	live := make([]*connection, 0, len(tcp.conns))
	for c := range tcp.conns {
		live = append(live, c)
	}
	tcp.mu.Unlock()
	for _, c := range live {
		_ = c.Close()
	}

	done := make(chan struct{})
	go func() {
		tcp.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		tcp.pu.Logger.Info("tcp controller stopped", zap.Int("connections_closed", len(live)))
	case <-time.After(tcp.lim.drainTimeout):
		tcp.mu.Lock()
		left := len(tcp.conns)
		tcp.mu.Unlock()
		tcp.pu.Logger.Warn("tcp drain timeout; abandoning stragglers",
			zap.Duration("drain_timeout", tcp.lim.drainTimeout), zap.Int("connections", left))
	}
}
