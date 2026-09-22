package tcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// Handler speaks one protocol on a connection the head has already
// accepted. Everything before the first protocol byte is the head's:
// the accept gate and caps, the TLS handshake or the edge's PROXY header,
// the connect run that routes the connection and lets the stack refuse
// it, the per-tenant slot, drain. ServeConn gets the byte stream and the
// routing outcome, and turns protocol traffic into events with
// RoutedConn.Emit.
//
// One Handler serves every connection on its listener, concurrently.
// ServeConn returns when the connection is finished — nil for an ordinary
// end (the peer hung up, the protocol said goodbye), an error naming why
// otherwise; the head logs it and closes the socket either way. ctx is
// cancelled at chassis shutdown, and the head closes the socket then too,
// so a blocked Read returns.
type Handler interface {
	ServeConn(ctx context.Context, rc RoutedConn) error
}

// HandlerFactory builds a listener's Handler at bind time.
type HandlerFactory func(pu *processor.Unit) Handler

// LineHandler is the default handler: the newline-delimited protocol the
// head has always spoken.
const LineHandler = "line"

// RoutedConn is one accepted connection as a Handler sees it. Every field
// is the head's own observation or the bus loop's trusted routing outcome
// (event.DispatchResult) — a handler never parses identity out of an
// envelope, and never out of what the client sent.
type RoutedConn struct {
	// Conn is the application byte stream: decrypted on a `;tls` listener,
	// past the PROXY header on a `;proxy=` one. The head closes it when
	// ServeConn returns.
	Conn net.Conn

	RID      string
	Listener string

	Tenant, Stack string // where the connect run routed the connection
	Host          string // @tcp.host — canonical; "" when routed by listener

	TLS              bool
	ALPN, TLSVersion string

	ClientIP              string
	RemotePort, LocalPort int

	// Route is the whole route the connect run promoted; Emit stamps it on
	// every later event.
	Route RouteStamp

	// Emit runs one event for this connection through the pipeline and
	// waits for the outcome. The event enters the stack the connection is
	// pinned to — the route is pre-stamped, not re-decided per event —
	// after the head has checked that route still stands; admission still
	// runs on every event. Any error means the connection is over:
	// ErrRerouted, a *DeniedError, a timeout, a failed run, shutdown.
	Emit func(ctx context.Context, ev Event) (event.DispatchResult, error)

	// AddFuel charges the connection for work the handler does itself,
	// outside any run — answering a keepalive, parsing, fan-out. The head
	// already charges the connect and every byte through Conn; all of it
	// is billed on the connection's next event (see meter). Costs follow
	// docs/advanced/fuel.md: 1 fuel ≈ 100µs of chassis work.
	AddFuel func(fuel int64)
}

// RouteStamp is a connection's pinned route: what `_txc.route.*` carries
// on each of its events.
type RouteStamp struct {
	Tenant, Stack, Ingress string
	HostnameVerified       bool
}

// Event is what a handler contributes to one run. The envelope's source
// is the handler's own name (`@src == "irc"` for a handler registered as
// "irc"; "tcp" for the line handler), its facts land under that name, and
// the connection's `@tcp.*` facts, `@client.ip` and the route ride along,
// chassis-stamped. A handler cannot write anywhere else in `_txc`.
type Event struct {
	// Body is the raw protocol unit (a line, a frame) → `@client.body`,
	// base64. nil leaves the field off.
	Body []byte
	// Facts are the handler's parsed view of it → `@<src>.<key>`. A key may
	// be a dotted path. The line handler has none: `@tcp.*` is the head's.
	Facts map[string]any
}

// ErrRerouted ends a connection whose route no longer stands: its inlet
// was deactivated, its hostname revoked or re-bound, its tenant removed.
var ErrRerouted = errors.New("rerouted")

// DeniedError is the admission gate refusing one event (a suspended
// tenant, a rate limit). The handler says so in its protocol's words and
// closes.
type DeniedError struct {
	Status int
	Reason string
}

func (e *DeniedError) Error() string { return "denied: " + strconv.Itoa(e.Status) + " " + e.Reason }

var handlerRegistry = map[string]HandlerFactory{}

// reservedHandlerNames would collide with an inlet or an event source
// another head owns: a handler's name is its `@src` and, prefixed with
// `_`, the nested stack that opts into it.
var reservedHandlerNames = map[string]bool{
	"tcp": true, "sys": true, "http": true, "web": true, "admin": true,
	"lmtp": true, "mail": true, "imap": true, "dns": true, "llm": true,
	"cron": true, "scheduled": true, "source": true, "room": true, "inspect": true,
	"calendar": true, "contacts": true, "webdav": true, "websocket": true,
	"state": true,
}

// RegisterHandler adds a named protocol handler, selectable per listener
// with `;handler=NAME`. Called from init() in the handler's package. The
// name is load-bearing twice over: events the handler emits carry it as
// `@src`, and a hostname-routed connection enters `<stack>/_NAME`, so a
// stack opts into the protocol by declaring that inlet. A bad, reserved
// or duplicate name is a programming error and panics at init.
func RegisterHandler(name string, f HandlerFactory) {
	if !validHandlerName(name) || reservedHandlerNames[name] {
		panic(fmt.Sprintf("tcp: handler name %q is invalid or reserved (want [a-z][a-z0-9-]*)", name))
	}
	if _, dup := handlerRegistry[name]; dup {
		panic(fmt.Sprintf("tcp: handler %q registered twice", name))
	}
	if f == nil {
		panic(fmt.Sprintf("tcp: handler %q has no factory", name))
	}
	handlerRegistry[name] = f
}

func validHandlerName(name string) bool {
	if name == "" || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// lookupHandler resolves a `;handler=` name; an unknown one lists what
// this build has.
func lookupHandler(name string) (HandlerFactory, error) {
	if f, ok := handlerRegistry[name]; ok {
		return f, nil
	}
	avail := make([]string, 0, len(handlerRegistry))
	for k := range handlerRegistry {
		avail = append(avail, k)
	}
	sort.Strings(avail)
	return nil, fmt.Errorf("unknown handler %q (available: %v)", name, avail)
}

// handlerSrc is the `@src` of the events a handler emits: its name,
// except the line handler, which has always been "tcp".
func handlerSrc(name string) string {
	if name == LineHandler {
		return "tcp"
	}
	return name
}

// handlerInlet is the nested stack a hostname-routed connection enters on
// a listener with this handler: `_tcp` for line, `_NAME` otherwise.
func handlerInlet(name string) string { return "_" + handlerSrc(name) }
