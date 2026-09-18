package tcp

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/admission"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
)

// "hello" is a handler registered for these tests only: each test swaps in
// the ServeConn it wants.
var helloServe atomic.Value // func(context.Context, RoutedConn) error

func init() {
	RegisterHandler("hello", func(*processor.Unit) Handler { return helloHandler{} })
}

type helloHandler struct{}

func (helloHandler) ServeConn(ctx context.Context, rc RoutedConn) error {
	return helloServe.Load().(func(context.Context, RoutedConn) error)(ctx, rc)
}

func withHandler(name string) func(*config.Config) {
	return func(c *config.Config) { c.TCPListenAddrs = []string{"test=127.0.0.1:0;handler=" + name} }
}

// inHello is a run that landed in a stack's `_hello` inlet by hostname.
func inHello(p event.Payload) event.DispatchResult {
	return event.DispatchResult{Payload: p, Tenant: "t1", Stack: "shop/_hello",
		Ingress: "host:hello.example", HostnameVerified: true}
}

// TestHandlerGetsTrustedRouteAndEmitsUnderItsSource — the seam end to
// end: the connect run is still a `tcp` event, asking for the handler's
// inlet; the handler is handed the DispatchResult's tenant and stack; and
// what it emits is an event under its own source, carrying its facts, the
// head's facts and the pinned route pre-stamped.
func TestHandlerGetsTrustedRouteAndEmitsUnderItsSource(t *testing.T) {
	helloServe.Store(func(ctx context.Context, rc RoutedConn) error {
		fmt.Fprintf(rc.Conn, "hello %s %s %s\n", rc.Tenant, rc.Stack, rc.Listener)
		res, err := rc.Emit(ctx, Event{Body: []byte("ping"), Facts: map[string]any{"n": 1, "who.name": "x"}})
		if err != nil {
			return err
		}
		fmt.Fprintf(rc.Conn, "%s\n", gjson.Get(res.Payload.Raw, "_txc.hello.res.say").String())
		return nil
	})
	connect, emitted := make(chan *event.Envelope, 1), make(chan *event.Envelope, 1)
	h := newHarness(t, withHandler("hello"), nil, func(env *event.Envelope) event.DispatchResult {
		if env.Src == "tcp" {
			connect <- env
			return inHello(verdict("welcome\n", ""))
		}
		emitted <- env
		return inHello(event.Payload{Raw: `{"_txc":{"hello":{"res":{"say":"hi"}}}}`, Type: event.JSON})
	})
	_, r := dial(t, h.addr)
	for _, want := range []string{"welcome\n", "hello t1 shop/_hello test\n", "hi\n"} {
		if got := readLineT(t, r); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	expectEOF(t, r)

	c := (<-connect).Payload.Raw
	if got := gjson.Get(c, "_txc.tcp.inlet").String(); got != "_hello" {
		t.Errorf("connect run must ask for the handler's inlet, got %q", got)
	}
	if gjson.Get(c, "_txc.route").Exists() {
		t.Errorf("the connect run is routed by detect-tenant, not pre-stamped: %s", c)
	}

	env := <-emitted
	if env.Src != "hello" {
		t.Errorf("envelope src = %q", env.Src)
	}
	e := env.Payload.Raw
	for path, want := range map[string]string{
		"_txc.src":                     "hello",
		"_txc.hello.n":                 "1",
		"_txc.hello.who.name":          "x",
		"_txc.client.body":             base64.StdEncoding.EncodeToString([]byte("ping")),
		"_txc.client.ip":               "127.0.0.1",
		"_txc.tcp.listener":            "test",
		"_txc.tcp.inlet":               "_hello",
		"_txc.route.tenant":            "t1",
		"_txc.route.stack":             "shop/_hello",
		"_txc.route.to":                "shop/_hello/0",
		"_txc.route.ingress":           "host:hello.example",
		"_txc.route.hostname_verified": "true",
	} {
		if got := gjson.Get(e, path).String(); got != want {
			t.Errorf("%s = %q, want %q (raw %s)", path, got, want, e)
		}
	}
}

// TestLineEventsCarryThePinnedRoute — the line handler goes through the
// same Emit: a line is pre-stamped with the route its connect run chose.
func TestLineEventsCarryThePinnedRoute(t *testing.T) {
	line := make(chan string, 1)
	h := newHarness(t, nil, nil, func(env *event.Envelope) event.DispatchResult {
		if !isConnect(env) {
			line <- env.Payload.Raw
		}
		return greetThenUpper(env)
	})
	c, r := dial(t, h.addr)
	readLineT(t, r)
	if _, err := c.Write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	readLineT(t, r)
	raw := <-line
	for path, want := range map[string]string{
		"_txc.src": "tcp", "_txc.tcp.inlet": "_tcp",
		"_txc.route.tenant": "t1", "_txc.route.stack": "t1/tcp", "_txc.route.to": "t1/tcp/0",
	} {
		if got := gjson.Get(raw, path).String(); got != want {
			t.Errorf("%s = %q, want %q (raw %s)", path, got, want, raw)
		}
	}
}

// flipResolver answers every lookup from a switchable state.
type flipResolver struct {
	state atomic.Value // "agree" | "miss" | "err"
	keys  chan ingress.RouteKey
}

func (f *flipResolver) ResolveErr(key ingress.RouteKey) (ingress.RouteTarget, bool, error) {
	select {
	case f.keys <- key:
	default:
	}
	switch f.state.Load().(string) {
	case "agree":
		return ingress.RouteTarget{Tenant: "t1", Stack: "t1/tcp"}, true, nil
	case "err":
		return ingress.RouteTarget{}, false, errors.New("mirror busy")
	}
	return ingress.RouteTarget{}, false, nil
}

// TestEmitRechecksTheRoute — pinning must not outlive the opt-in. A route
// the resolver vouched for is asked about again before each event, and
// the connection closes — without running anything — once the answer
// changes. A lookup that merely fails proves nothing, and a route the
// resolver never produced (an operator's own boot rule) is not its to
// withdraw.
func TestEmitRechecksTheRoute(t *testing.T) {
	for name, tc := range map[string]struct {
		atPin, later string
		closes       bool
	}{
		"inlet withdrawn":           {"agree", "miss", true},
		"lookup fails":              {"agree", "err", false},
		"route was the operator's":  {"miss", "miss", false},
		"resolver unsure at accept": {"err", "miss", false},
	} {
		t.Run(name, func(t *testing.T) {
			fr := &flipResolver{keys: make(chan ingress.RouteKey, 1)}
			fr.state.Store(tc.atPin)
			var lines atomic.Int32
			h := newHarness(t, nil, func(c *TCPController) { c.SetResolver(fr) }, func(env *event.Envelope) event.DispatchResult {
				if !isConnect(env) {
					lines.Add(1)
				}
				return greetThenUpper(env)
			})
			c, r := dial(t, h.addr)
			readLineT(t, r)
			if key := <-fr.keys; key != (ingress.RouteKey{Src: "tcp", Listener: "test", Inlet: "_tcp"}) {
				t.Errorf("resolver asked with %+v", key)
			}
			if _, err := c.Write([]byte("one\n")); err != nil {
				t.Fatal(err)
			}
			if got := readLineT(t, r); got != "ONE\n" {
				t.Fatalf("echo = %q", got)
			}
			fr.state.Store(tc.later)
			if _, err := c.Write([]byte("two\n")); err != nil {
				t.Fatal(err)
			}
			if !tc.closes {
				if got := readLineT(t, r); got != "TWO\n" {
					t.Fatalf("echo = %q", got)
				}
				return
			}
			expectEOF(t, r)
			if n := lines.Load(); n != 1 {
				t.Errorf("a withdrawn route must not run the event: %d line runs, want 1", n)
			}
		})
	}
}

// TestHandlerSeesDenial — the admission gate's refusal of one event
// reaches the handler as a *DeniedError, never as a payload to interpret.
func TestHandlerSeesDenial(t *testing.T) {
	helloServe.Store(func(ctx context.Context, rc RoutedConn) error {
		_, err := rc.Emit(ctx, Event{})
		var denied *DeniedError
		if errors.As(err, &denied) {
			fmt.Fprintf(rc.Conn, "denied %d %s\n", denied.Status, denied.Reason)
		}
		return err
	})
	h := newHarness(t, withHandler("hello"), nil, func(env *event.Envelope) event.DispatchResult {
		if env.Src == "tcp" {
			return inHello(verdict("", ""))
		}
		raw := admission.MarkDenied("{}", admission.Decision{Status: 429, Reason: "rate"}, "t1")
		return inHello(event.Payload{Raw: raw, Type: event.JSON})
	})
	_, r := dial(t, h.addr)
	if got := readLineT(t, r); got != "denied 429 rate\n" {
		t.Fatalf("got %q", got)
	}
	expectEOF(t, r)
}

// TestHandlerPanicIsContained — a handler that panics loses its
// connection, not the chassis: the next connection is served.
func TestHandlerPanicIsContained(t *testing.T) {
	var n atomic.Int32
	helloServe.Store(func(ctx context.Context, rc RoutedConn) error {
		if n.Add(1) == 1 {
			panic("protocol bug")
		}
		_, err := rc.Conn.Write([]byte("ok\n"))
		return err
	})
	h := newHarness(t, withHandler("hello"), nil, func(env *event.Envelope) event.DispatchResult {
		return inHello(verdict("", ""))
	})
	_, r := dial(t, h.addr)
	expectEOF(t, r)
	_, r = dial(t, h.addr)
	if got := readLineT(t, r); got != "ok\n" {
		t.Fatalf("second connection got %q", got)
	}
}

// TestEchoHandler — `;handler=echo`: the connect run greets (or refuses)
// like on any listener, bytes come back unframed, the handler hangs up
// after 500, and the stack hears about it once, under `@src == "echo"`.
func TestEchoHandler(t *testing.T) {
	for name, tc := range map[string]struct {
		send       []string
		halfClose  bool
		wantBytes  int
		wantReason string
	}{
		"limit": {send: []string{"abc", strings.Repeat("x", 600)}, wantBytes: 500, wantReason: "limit"},
		"eof":   {send: []string{"hi"}, halfClose: true, wantBytes: 2, wantReason: "eof"},
	} {
		t.Run(name, func(t *testing.T) {
			closed := make(chan *event.Envelope, 1)
			inEcho := func(p event.Payload) event.DispatchResult {
				return event.DispatchResult{Payload: p, Tenant: "t1", Stack: "shop/_echo"}
			}
			h := newHarness(t, withHandler("echo"), nil, func(env *event.Envelope) event.DispatchResult {
				if env.Src == "tcp" {
					if got := gjson.Get(env.Payload.Raw, "_txc.tcp.inlet").String(); got != "_echo" {
						t.Errorf("inlet = %q", got)
					}
					return inEcho(verdict("echo ready\n", ""))
				}
				closed <- env
				return inEcho(verdict("", ""))
			})
			c, r := dial(t, h.addr)
			if got := readLineT(t, r); got != "echo ready\n" {
				t.Fatalf("greeting = %q", got)
			}
			sent := ""
			for _, s := range tc.send {
				if _, err := c.Write([]byte(s)); err != nil {
					t.Fatal(err)
				}
				sent += s
			}
			if tc.halfClose {
				_ = c.(*net.TCPConn).CloseWrite()
			}
			got := readAllLenient(r)
			if want := sent[:tc.wantBytes]; got != want {
				t.Fatalf("echoed %d bytes %q, want %d", len(got), got, len(want))
			}
			env := <-closed
			if env.Src != "echo" {
				t.Errorf("envelope src = %q", env.Src)
			}
			for path, want := range map[string]string{
				"_txc.src": "echo", "_txc.echo.phase": "close", "_txc.echo.reason": tc.wantReason,
				"_txc.echo.bytes": fmt.Sprint(tc.wantBytes), "_txc.route.to": "shop/_echo/0",
			} {
				if got := gjson.Get(env.Payload.Raw, path).String(); got != want {
					t.Errorf("%s = %q, want %q", path, got, want)
				}
			}
		})
	}
}

// readAllLenient reads to the end of the stream. The echo handler hangs up
// with the client's surplus unread, which the kernel may report as a
// reset after the echoed bytes; what arrived before it is what counts.
func readAllLenient(r *bufio.Reader) string {
	b, _ := io.ReadAll(r)
	return string(b)
}

// TestRegisterHandlerRefusesBadNames — a handler's name becomes an event
// source and an inlet stack name, so it cannot be one another head owns.
func TestRegisterHandlerRefusesBadNames(t *testing.T) {
	for _, name := range []string{"", "Tcp", "a_b", "9a", "a/b", "tcp", "mail", "http", "websocket", "line", "echo"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("RegisterHandler(%q) must panic", name)
				}
			}()
			RegisterHandler(name, func(*processor.Unit) Handler { return helloHandler{} })
		}()
	}
	if got := handlerInlet("line") + " " + handlerInlet("echo") + " " + handlerSrc("line") + " " + handlerSrc("echo"); got != "_tcp _echo tcp echo" {
		t.Errorf("name mapping = %q", got)
	}
}
