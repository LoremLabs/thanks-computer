package websocket

import (
	"context"
	"fmt"
	"sort"

	"go.uber.org/zap"
)

// Relay delivers a send or close to the node that owns a session when that
// node is not this one. Open core defines the seam and ships no backend; a
// backend registered by a build that has a message bus carries the request
// to the owning node's inbox and returns that node's answer.
//
// The seam mirrors chassis/room's Relay: core defines the interface + the
// registry, a backend self-registers via init() + a blank import, and the
// backend reads its own connection details from its own env. Unlike the room
// relay, delivery here is addressed and answered — a `txco://websocket/send`
// reports whether the message reached the socket, so a relay call is a
// request with a reply, not a fire-and-forget publish.
type Relay interface {
	// Node is the address other nodes use to reach THIS node's inbox — the
	// backend's subject-safe rendering of RelayConfig.NodeID. The controller
	// records it in the directory beside every session it owns.
	Node() string
	// Send writes one message to a session that lives on `node`. It returns
	// the owning node's answer as the Registry errors (ErrSessionNotFound
	// when nothing on that node owns the session, ErrSessionClosed,
	// ErrWriteTimeout, ErrMessageTooLarge), ErrWriteTimeout when ctx expires
	// first, or ErrRelayUnavailable when the bus itself is down.
	Send(ctx context.Context, node, tenant, sid string, typ MessageType, data []byte) error
	// Close starts the close handshake on a session that lives on `node`.
	Close(ctx context.Context, node, tenant, sid string, code int, reason string) error
	// Shutdown stops answering for this node and releases the bus
	// connection, bounded by ctx.
	Shutdown(ctx context.Context) error
}

// LocalHandler is the node-local surface a Relay backend calls for the
// requests that arrive at this node's inbox. It resolves sessions on this
// node ONLY — never the cross-node Send — so a request can never bounce
// between nodes.
type LocalHandler interface {
	Send(ctx context.Context, tenant, sid string, typ MessageType, data []byte) error
	Close(ctx context.Context, tenant, sid string, code int, reason string) error
}

// RelayConfig carries the chassis-owned facts a backend cannot obtain on its
// own (the bgservice.Config precedent). Connection details — URL,
// credentials, subject prefix — are the backend's own env.
type RelayConfig struct {
	// NodeID names this chassis (the operator's FQDN, per-Machine on a
	// cloud fleet). The backend derives Node() from it.
	NodeID  string
	Logger  *zap.Logger
	Handler LocalHandler
}

// RelayConstructor builds a Relay: connects, subscribes this node's inbox,
// and wires inbound requests to cfg.Handler.
type RelayConstructor func(cfg RelayConfig) (Relay, error)

var relayRegistry = map[string]RelayConstructor{}

// RegisterRelay adds a named cross-node relay backend. Called from init() in
// the backend package; activated by a blank import. No backend is built in
// open core — the node-local registry is the default and only built-in.
func RegisterRelay(name string, c RelayConstructor) { relayRegistry[name] = c }

// OpenRelay constructs the named relay backend. Unknown name is a hard error
// listing what is available.
func OpenRelay(name string, cfg RelayConfig) (Relay, error) {
	c, ok := relayRegistry[name]
	if !ok {
		avail := make([]string, 0, len(relayRegistry))
		for k := range relayRegistry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("websocket: unknown relay %q (available: %v)", name, avail)
	}
	return c(cfg)
}

// localHandler is the Controller's LocalHandler: lookup + local write only.
type localHandler struct{ c *Controller }

// Local returns the node-local delivery surface a Relay backend serves
// inbound requests through.
func (c *Controller) Local() LocalHandler { return localHandler{c: c} }

func (l localHandler) Send(ctx context.Context, tenant, sid string, typ MessageType, data []byte) error {
	return l.c.sendLocal(ctx, tenant, sid, typ, data)
}

func (l localHandler) Close(ctx context.Context, tenant, sid string, code int, reason string) error {
	return l.c.closeLocal(ctx, tenant, sid, code, reason)
}
