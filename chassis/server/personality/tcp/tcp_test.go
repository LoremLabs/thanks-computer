package tcp

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	txtls "github.com/loremlabs/thanks-computer/chassis/tls"
)

// The head is exercised end to end here: a real listener on 127.0.0.1:0,
// a real client, and a stub behind the bus standing in for the processor.
// Nothing about routing or txcl runs — the stub IS the stack's answer.

// stub answers every envelope the head puts on the bus with the
// DispatchResult the bus loop would have produced.
type stub func(env *event.Envelope) event.DispatchResult

// routed wraps a payload as a run that landed in tenant t1 — what the
// bus loop reports for a hostname or listener that resolved.
func routed(p event.Payload) event.DispatchResult {
	return event.DispatchResult{Payload: p, Tenant: "t1", Stack: "t1/tcp"}
}

type harness struct {
	ctrl   *TCPController
	addr   net.Addr
	cancel context.CancelFunc
}

func newHarness(t *testing.T, tune func(*config.Config), setup func(*TCPController), reply stub) *harness {
	t.Helper()
	conf := config.Config{
		Personalities:         "tcp",
		TCPListenAddrs:        []string{"test=127.0.0.1:0"},
		TCPConnectRespTimeout: "2s",
		TCPRespTimeout:        "2s",
		TCPMaxIdleTimeout:     "2s",
		TCPDrainTimeout:       "2s",
	}
	if tune != nil {
		tune(&conf)
	}
	bus := make(chan *event.Envelope, 8)
	pu := &processor.Unit{Conf: conf, Logger: zap.NewNop(), Bus: bus}
	ctx, cancel := context.WithCancel(context.Background())
	ctrl := NewController(ctx, pu)
	if setup != nil {
		setup(ctrl)
	}
	go func() {
		for {
			select {
			case env := <-bus:
				if env.ResCh != nil || env.ResultCh == nil {
					t.Errorf("the head must dispatch on ResultCh only (ResCh=%v ResultCh=%v)", env.ResCh != nil, env.ResultCh != nil)
				}
				env.ResultCh <- reply(env)
			case <-ctx.Done():
				return
			}
		}
	}()
	ctrl.Start()
	// Same order as the server: cancel the run context, then Stop.
	t.Cleanup(func() { cancel(); ctrl.Stop() })
	addrs := ctrl.Addrs()
	if len(addrs) != 1 {
		t.Fatalf("bound %d listeners, want 1", len(addrs))
	}
	return &harness{ctrl: ctrl, addr: addrs[0], cancel: cancel}
}

// verdict builds a stack answer: write is sent as-is, action "close"
// hangs up. Either may be empty.
func verdict(write, action string) event.Payload {
	raw := "{}"
	if write != "" {
		raw, _ = sjson.Set(raw, "_txc.tcp.res.write", base64.StdEncoding.EncodeToString([]byte(write)))
	}
	if action != "" {
		raw, _ = sjson.Set(raw, "_txc.tcp.res.action", action)
	}
	return event.Payload{Raw: raw, Type: event.JSON}
}

func isConnect(env *event.Envelope) bool {
	return !gjson.Get(env.Payload.Raw, "_txc.client.body").Exists()
}

func lineOf(env *event.Envelope) string {
	b, _ := base64.StdEncoding.DecodeString(gjson.Get(env.Payload.Raw, "_txc.client.body").String())
	return string(b)
}

// greetThenUpper greets on connect and echoes every line upper-cased.
func greetThenUpper(env *event.Envelope) event.DispatchResult {
	if isConnect(env) {
		return routed(verdict("hello\n", ""))
	}
	return routed(verdict(strings.ToUpper(lineOf(env)), ""))
}

func dial(t *testing.T, addr net.Addr) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c, bufio.NewReader(c)
}

func readLineT(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	s, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("read line: %v (got %q)", err, s)
	}
	return s
}

func expectEOF(t *testing.T, r *bufio.Reader) {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("expected clean EOF, got err=%v (bytes=%q)", err, b)
	}
	if len(b) != 0 {
		t.Fatalf("expected nothing more, got %q", b)
	}
}

func TestConnectGreetingAndLineEcho(t *testing.T) {
	h := newHarness(t, nil, nil, greetThenUpper)
	c, r := dial(t, h.addr)
	if got := readLineT(t, r); got != "hello\n" {
		t.Fatalf("greeting = %q", got)
	}
	for _, line := range []string{"abc\n", "with\r\n"} {
		if _, err := c.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if got, want := readLineT(t, r), strings.ToUpper(line); got != want {
			t.Fatalf("echo = %q, want %q", got, want)
		}
	}
}

// TestConnectFactsStamped — the connect envelope carries the listener
// name, both socket addresses and the shared client ip, all chassis-set.
func TestConnectFactsStamped(t *testing.T) {
	got := make(chan string, 1)
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		if isConnect(env) {
			got <- env.Payload.Raw
		}
		return routed(verdict("", "close"))
	})
	_, r := dial(t, h.addr)
	expectEOF(t, r)
	raw := <-got
	for path, want := range map[string]string{
		"_txc.src":             "tcp",
		"_txc.tcp.listener":    "test",
		"_txc.client.ip":       "127.0.0.1",
		"_txc.tcp.local.ip":    "127.0.0.1",
		"_txc.tcp.tls.enabled": "false",
	} {
		if v := gjson.Get(raw, path).String(); v != want {
			t.Errorf("%s = %q, want %q (raw %s)", path, v, want, raw)
		}
	}
	for _, path := range []string{"_txc.rid", "_txc.tcp.local.port", "_txc.tcp.remote.port"} {
		if !gjson.Get(raw, path).Exists() {
			t.Errorf("%s missing (raw %s)", path, raw)
		}
	}
}

// TestConnectClose — a stack may refuse the connection on the connect
// run, optionally with a goodbye line first.
func TestConnectClose(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		return routed(verdict("bye\n", "close"))
	})
	_, r := dial(t, h.addr)
	if got := readLineT(t, r); got != "bye\n" {
		t.Fatalf("goodbye = %q", got)
	}
	expectEOF(t, r)
}

// TestConnectSilentAccept — with no write on the connect run nothing is
// echoed (the envelope JSON is a line-run projection only), and the
// connection stays open for lines.
func TestConnectSilentAccept(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		if isConnect(env) {
			return routed(event.Payload{Raw: `{"note":"accepted"}`, Type: event.JSON})
		}
		return routed(verdict("pong\n", ""))
	})
	c, r := dial(t, h.addr)
	if _, err := c.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLineT(t, r); got != "pong\n" {
		t.Fatalf("first bytes = %q, want the pong (connect run must stay silent)", got)
	}
}

// TestConnectDenied — an admission denial on the connect run is rendered
// as one "<status> <reason>" line, then the socket closes.
func TestConnectDenied(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		raw := admission.MarkDenied("{}", admission.Decision{Status: 403, Reason: "nope"}, "t1")
		return routed(event.Payload{Raw: raw, Type: event.JSON})
	})
	_, r := dial(t, h.addr)
	if got := readLineT(t, r); got != "403 nope\n" {
		t.Fatalf("denial = %q", got)
	}
	expectEOF(t, r)
}

// TestLineDenied — a denial mid-session closes after the line, too.
func TestLineDenied(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		if isConnect(env) {
			return routed(verdict("", ""))
		}
		raw := admission.MarkDenied("{}", admission.Decision{Status: 429, Reason: "rate"}, "t1")
		return routed(event.Payload{Raw: raw, Type: event.JSON})
	})
	c, r := dial(t, h.addr)
	if _, err := c.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLineT(t, r); got != "429 rate\n" {
		t.Fatalf("denial = %q", got)
	}
	expectEOF(t, r)
}

// TestConnectRunFailed — a pipeline error on the connect run closes the
// socket without writing anything.
func TestConnectRunFailed(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		return event.DispatchResult{Payload: event.Payload{Raw: `{"err":"boom"}`, Type: event.ErrorStr}, Tenant: "t1", Err: errors.New("boom")}
	})
	_, r := dial(t, h.addr)
	expectEOF(t, r)
}

// TestDefaultJSONProjection — a line run with no explicit write answers
// with the envelope minus its `_`-prefixed keys.
func TestDefaultJSONProjection(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		if isConnect(env) {
			return routed(verdict("", ""))
		}
		raw, _ := sjson.Set(env.Payload.Raw, "hello", "world")
		raw, _ = sjson.Set(raw, "_txc.tcp.res.action", "close")
		return routed(event.Payload{Raw: raw, Type: event.JSON})
	})
	c, r := dial(t, h.addr)
	if _, err := c.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"hello":"world"}` {
		t.Fatalf("projection = %q", got)
	}
}

// TestOverLimitLineDropped — an oversized line is consumed and ignored;
// the connection and the next line are unaffected.
func TestOverLimitLineDropped(t *testing.T) {
	h := newHarness(t, nil, func(c *TCPController) { c.lim.maxLine = 16 }, greetThenUpper)
	c, r := dial(t, h.addr)
	readLineT(t, r) // greeting
	big := strings.Repeat("z", 10_000) + "\n"
	if _, err := c.Write([]byte(big + "ok\n")); err != nil {
		t.Fatal(err)
	}
	if got := readLineT(t, r); got != "OK\n" {
		t.Fatalf("after oversized line got %q, want the next line's echo", got)
	}
}

// TestIdleTimeoutCloses — silence past --tcp-max-idle-timeout ends the
// session.
func TestIdleTimeoutCloses(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.TCPMaxIdleTimeout = "200ms" }, nil, greetThenUpper)
	_, r := dial(t, h.addr)
	readLineT(t, r)
	start := time.Now()
	expectEOF(t, r)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("idle close took %v", time.Since(start))
	}
}

// TestStopClosesOpenConnections — Stop closes an idle session promptly
// instead of waiting for it to time out.
func TestStopClosesOpenConnections(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.TCPMaxIdleTimeout = "30s" }, nil, greetThenUpper)
	_, r := dial(t, h.addr)
	readLineT(t, r)
	start := time.Now()
	h.cancel()
	h.ctrl.Stop()
	expectEOF(t, r)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("stop took %v", time.Since(start))
	}
	if _, err := net.DialTimeout("tcp", h.addr.String(), 500*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after Stop")
	}
}

// TestDrainingRefusesNewConnections — a draining node answers a new
// connection with one 503 line and closes, before any connect run.
func TestDrainingRefusesNewConnections(t *testing.T) {
	runs := make(chan struct{}, 8)
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		runs <- struct{}{}
		return routed(verdict("", ""))
	})
	admission.SetDraining(true)
	t.Cleanup(func() { admission.SetDraining(false) })
	_, r := dial(t, h.addr)
	if got := readLineT(t, r); got != "503 draining\n" {
		t.Fatalf("got %q", got)
	}
	expectEOF(t, r)
	select {
	case <-runs:
		t.Fatal("a draining node must not spend a pipeline run on the connection")
	default:
	}
}

// TestNodeCapRefuses — --tcp-max-conns bounds open sessions per node; a
// slot frees when its session ends.
func TestNodeCapRefuses(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.TCPMaxConns = 1 }, nil, greetThenUpper)
	c1, r1 := dial(t, h.addr)
	readLineT(t, r1)

	_, r2 := dial(t, h.addr)
	if got := readLineT(t, r2); got != "503 too many connections\n" {
		t.Fatalf("second connection got %q", got)
	}
	expectEOF(t, r2)

	_ = c1.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c3, err := net.Dial("tcp", h.addr.String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c3.SetDeadline(time.Now().Add(time.Second))
		got, _ := bufio.NewReader(c3).ReadString('\n')
		_ = c3.Close()
		if got == "hello\n" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never freed; last answer %q", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestPersonalityOff — without "tcp" in --personalities nothing binds.
func TestPersonalityOff(t *testing.T) {
	pu := &processor.Unit{Conf: config.Config{Personalities: "web", TCPListenAddrs: []string{"127.0.0.1:0"}}, Logger: zap.NewNop()}
	ctrl := NewController(context.Background(), pu)
	ctrl.Start()
	if n := len(ctrl.Addrs()); n != 0 {
		t.Fatalf("bound %d listeners with the personality off", n)
	}
	ctrl.Stop()
}

// TestUnroutedConnectionClosed — a connect run that never left _sys
// (unknown hostname, no listener entry) fails closed: no bytes, no
// session.
func TestUnroutedConnectionClosed(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		if !isConnect(env) {
			t.Error("a line must never reach the bus on an unrouted connection")
		}
		// The greeting the stack wrote must not leak either: unrouted
		// means nothing was said to the peer.
		return event.DispatchResult{Payload: verdict("hello\n", ""), Tenant: "_sys"}
	})
	_, r := dial(t, h.addr)
	expectEOF(t, r)
}

// TestRouteUnavailableClosed — a transient routing failure is not a
// route: the connection closes instead of guessing a tenant.
func TestRouteUnavailableClosed(t *testing.T) {
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		return event.DispatchResult{Payload: event.Payload{Raw: `{"_txc":{"route":{"unavailable":true},"halt":true}}`, Type: event.JSON}, Tenant: "_sys"}
	})
	_, r := dial(t, h.addr)
	expectEOF(t, r)
}

// TestPerTenantCap — --tcp-max-conns-per-tenant counts routed sessions
// per tenant; another tenant is unaffected.
func TestPerTenantCap(t *testing.T) {
	var n atomic.Int32
	h := newHarness(t, func(c *config.Config) { c.TCPMaxConnsPerTenant = 1 }, nil, func(env *event.Envelope) event.DispatchResult {
		tenant := "t1"
		if isConnect(env) && n.Add(1) == 3 {
			tenant = "t2"
		}
		return event.DispatchResult{Payload: verdict("hello "+tenant+"\n", ""), Tenant: tenant, Stack: tenant + "/tcp"}
	})
	_, r1 := dial(t, h.addr)
	if got := readLineT(t, r1); got != "hello t1\n" {
		t.Fatalf("first: %q", got)
	}
	_, r2 := dial(t, h.addr)
	if got := readLineT(t, r2); got != "503 too many connections\n" {
		t.Fatalf("second t1 connection got %q", got)
	}
	expectEOF(t, r2)
	_, r3 := dial(t, h.addr)
	if got := readLineT(t, r3); got != "hello t2\n" {
		t.Fatalf("t2 connection got %q", got)
	}
}

// devTLS is a self-signed server config for the dev-local names, with
// ALPN "irc" offered so the negotiated protocol is observable.
func devTLS(t *testing.T) *tls.Config {
	t.Helper()
	cfg, err := txtls.SelfSignedTLS(txtls.DevSelfSignedHosts)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NextProtos = []string{"irc"}
	return cfg
}

func dialTLS(t *testing.T, addr net.Addr, serverName string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := tls.Dial("tcp", addr.String(), &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test client against the dev certificate
		ServerName:         serverName,
		NextProtos:         []string{"irc"},
	})
	if err != nil {
		t.Fatalf("tls dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	return c, bufio.NewReader(c)
}

// TestTLSListenerStampsHost — on a ;tls listener the handshake's SNI
// becomes @tcp.host (canonical) with @tcp.tls.* as provenance, and the
// session runs over TLS.
func TestTLSListenerStampsHost(t *testing.T) {
	got := make(chan string, 1)
	h := newHarness(t,
		func(c *config.Config) { c.TCPListenAddrs = []string{"irc=127.0.0.1:0;tls"} },
		func(c *TCPController) { c.SetTLSConfig(devTLS(t)) },
		func(env *event.Envelope) event.DispatchResult {
			if isConnect(env) {
				got <- env.Payload.Raw
			}
			return greetThenUpper(env)
		})
	c, r := dialTLS(t, h.addr, "IRC.Foo.local.thanks.computer")
	if got := readLineT(t, r); got != "hello\n" {
		t.Fatalf("greeting over tls = %q", got)
	}
	_, _ = c.Write([]byte("ping\n"))
	if got := readLineT(t, r); got != "PING\n" {
		t.Fatalf("echo over tls = %q", got)
	}
	raw := <-got
	for path, want := range map[string]string{
		"_txc.tcp.host":        "irc.foo.local.thanks.computer",
		"_txc.tcp.tls.sni":     "IRC.Foo.local.thanks.computer",
		"_txc.tcp.tls.enabled": "true",
		"_txc.tcp.tls.alpn":    "irc",
		"_txc.tcp.tls.version": "TLS 1.3",
		"_txc.tcp.listener":    "irc",
	} {
		if v := gjson.Get(raw, path).String(); v != want {
			t.Errorf("%s = %q, want %q (raw %s)", path, v, want, raw)
		}
	}
}

// TestTLSListenerNoSNI — without SNI there is no @tcp.host; routing then
// depends on the listener entry alone (here: none → the stub still
// answers, so the facts are what we check).
func TestTLSListenerNoSNI(t *testing.T) {
	got := make(chan string, 1)
	h := newHarness(t,
		func(c *config.Config) { c.TCPListenAddrs = []string{"irc=127.0.0.1:0;tls"} },
		func(c *TCPController) { c.SetTLSConfig(devTLS(t)) },
		func(env *event.Envelope) event.DispatchResult {
			if isConnect(env) {
				got <- env.Payload.Raw
			}
			return routed(verdict("", "close"))
		})
	_, r := dialTLS(t, h.addr, "")
	expectEOF(t, r)
	raw := <-got
	if gjson.Get(raw, "_txc.tcp.host").Exists() || gjson.Get(raw, "_txc.tcp.tls.sni").Exists() {
		t.Errorf("no SNI must stamp no hostname; raw %s", raw)
	}
	if !gjson.Get(raw, "_txc.tcp.tls.enabled").Bool() {
		t.Errorf("tls.enabled should still be true; raw %s", raw)
	}
}

// TestTLSHandshakeTimeout — a client that never speaks is dropped after
// --tcp-handshake-timeout, and no connect run is spent on it.
func TestTLSHandshakeTimeout(t *testing.T) {
	runs := make(chan struct{}, 8)
	h := newHarness(t,
		func(c *config.Config) {
			c.TCPListenAddrs = []string{"irc=127.0.0.1:0;tls"}
			c.TCPHandshakeTimeout = "200ms"
		},
		func(c *TCPController) { c.SetTLSConfig(devTLS(t)) },
		func(env *event.Envelope) event.DispatchResult {
			runs <- struct{}{}
			return greetThenUpper(env)
		})
	_, r := dial(t, h.addr) // plain TCP: never sends a ClientHello
	start := time.Now()
	expectEOF(t, r)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("handshake timeout took %v", time.Since(start))
	}
	select {
	case <-runs:
		t.Fatal("no connect run before the handshake completes")
	default:
	}
}

// TestSelfSignedListener — ;self-signed mints a dev certificate beside
// the data dir and serves it.
func TestSelfSignedListener(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t,
		func(c *config.Config) { c.TCPListenAddrs = []string{"dev=127.0.0.1:0;self-signed"} },
		func(c *TCPController) { c.selfSignedDir = dir },
		greetThenUpper)
	_, r := dialTLS(t, h.addr, "pony.local.thanks.computer")
	if got := readLineT(t, r); got != "hello\n" {
		t.Fatalf("greeting = %q", got)
	}
	for _, f := range []string{"tcp-selfsigned.crt", "tcp-selfsigned.key"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s not written: %v", f, err)
		}
	}
	if h.ctrl.WantsManagedTLS() {
		t.Error("a self-signed listener must not ask for the managed cert manager")
	}
}

func TestWantsManagedTLS(t *testing.T) {
	for _, tc := range []struct {
		addrs []string
		want  bool
	}{
		{[]string{":5050"}, false},
		{[]string{"a=:1;self-signed"}, false},
		{[]string{"a=:1", "b=:2;tls"}, true},
	} {
		pu := &processor.Unit{Conf: config.Config{Personalities: "tcp", TCPListenAddrs: tc.addrs}, Logger: zap.NewNop()}
		if got := NewController(context.Background(), pu).WantsManagedTLS(); got != tc.want {
			t.Errorf("%v: WantsManagedTLS = %v, want %v", tc.addrs, got, tc.want)
		}
	}
}
