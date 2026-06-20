package room

import (
	"fmt"
	"sort"
)

// Relay fans room Events across fleet nodes. A node publishes its locally-
// created Events via Publish; Events created on OTHER nodes arrive through the
// Deliver callback the constructor was given (the Hub injects them into its
// local subscribers + ring). Single-node chassis register no relay and the Hub
// stays purely in-process.
//
// The seam mirrors the trace Sink/Armable registries: open core defines the
// interface + registry and ships no backend; an overlay (NATS) self-registers
// via init() + a blank import.
type Relay interface {
	// Publish best-effort sends a locally-created Event to other nodes for
	// (tenant, room). Non-blocking by contract — it must drop rather than stall
	// the request path.
	Publish(tenant, room string, ev Event)
	// Close releases backend resources (connections, goroutines).
	Close() error
}

// RelayConfig carries backend-selecting options resolved from chassis config.
// Empty today; a backend reads its own connection details from its own env
// (the same seam discipline as trace.StoreConfig / artifact.StoreConfig).
type RelayConfig struct{}

// Deliver injects an Event received from ANOTHER node into the local Hub. A
// Relay backend MUST filter out its own published Events (origin tagging) so
// Deliver is only ever called for remote Events — otherwise a node would
// double-deliver (locally and via the round-trip) and could loop.
type Deliver func(tenant, room string, ev Event)

// RelayConstructor builds a Relay, wiring deliver to its inbound subscription.
type RelayConstructor func(cfg RelayConfig, deliver Deliver) (Relay, error)

var relayRegistry = map[string]RelayConstructor{}

// RegisterRelay adds a named cross-node relay backend. Called from init() in
// the backend package; activated by blank import. No backend is built in open
// core — the in-process Hub is the default and only built-in.
func RegisterRelay(name string, c RelayConstructor) { relayRegistry[name] = c }

// OpenRelay constructs the named relay backend, wiring deliver to its inbound
// feed. Unknown name is a hard error listing what is available.
func OpenRelay(name string, cfg RelayConfig, deliver Deliver) (Relay, error) {
	c, ok := relayRegistry[name]
	if !ok {
		avail := make([]string, 0, len(relayRegistry))
		for k := range relayRegistry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("room: unknown relay %q (available: %v)", name, avail)
	}
	return c(cfg, deliver)
}
