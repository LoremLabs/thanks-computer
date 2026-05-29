package trace

import (
	"context"
	"fmt"
	"sort"
)

// Armable is the read-side seam for *live* trace streams. Unlike Reader
// (which serves the persistent archive — newest-N + Get-by-rid), an
// Armable delivers each request's closed trace to one or more
// subscribers as soon as End fires. Backends that have no live-stream
// story (file, noop) register no Armable; backends that do (e.g. a
// NATS subscriber overlay) register one and the admin server exposes
// GET /traces/stream against it.
//
// The seam lives on the *subscriber* side — i.e. an Armable knows how
// to obtain a feed of closed traces, regardless of how the chassis
// emits them. In the NATS overlay the publisher is a separate
// trace.Sink and the Armable is a NATS subscriber; multi-node fan-in
// is automatic (every chassis publishes on the same subject hierarchy
// and the admin subscriber receives them all).
type Armable interface {
	// Subscribe returns a per-request subscription delivering closed
	// traces newer than sinceCursor (opaque to the client; empty =
	// "from now on"). buf is the channel buffer hint; backends may
	// clamp. The subscription is bound to ctx and drains on
	// cancellation.
	Subscribe(ctx context.Context, sinceCursor string, buf int) (Subscription, error)

	// Close drains any backend-side resources (NATS connections,
	// background goroutines). Respect the ctx deadline.
	Close(ctx context.Context) error
}

// Subscription is the per-request handle the admin endpoint reads
// from. Events() yields closed traces in arrival order (best-effort —
// the bus may not preserve cross-node ordering; clients sort on
// display by RequestDetail timestamps). Close() releases the
// subscription; backends MUST tolerate Close() being called multiple
// times.
type Subscription interface {
	Events() <-chan ClosedTrace
	Close()
}

// ClosedTrace is what flows live to admin clients on End. It carries
// the same fields Reader.Get would return (so the admin handler can
// map to its wire struct verbatim) plus an opaque per-subscription
// Cursor the client echoes back on reconnect.
type ClosedTrace struct {
	RequestDetail        // embedded: same shape as Reader.Get
	Cursor        string // monotonic per-subscription; opaque to client
}

// ArmableConstructor builds an Armable from StoreConfig (same config
// the Sink/Reader sides get).
type ArmableConstructor func(StoreConfig) (Armable, error)

var armableRegistry = map[string]ArmableConstructor{}

// RegisterArmable registers a named live-stream backend. Called from
// init() in the backend package; backends are activated by blank
// import (same discipline as Sink/Reader).
func RegisterArmable(name string, c ArmableConstructor) { armableRegistry[name] = c }

// OpenArmable constructs the named live-stream backend. Returns an
// error if the backend has no Armable registered — the admin server
// uses this to gate /traces/stream registration: backends without an
// Armable (file, noop) silently skip the route, backends with one
// (nats) get the route mounted.
func OpenArmable(name string, cfg StoreConfig) (Armable, error) {
	c, ok := armableRegistry[name]
	if !ok {
		avail := make([]string, 0, len(armableRegistry))
		for k := range armableRegistry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("trace: no Armable registered for %q (available: %v)", name, avail)
	}
	return c(cfg)
}
