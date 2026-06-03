package usage

import (
	"fmt"
	"sort"

	"go.uber.org/zap"
)

// SinkConfig carries the node/runtime context a sink may need, resolved
// from chassis config. It is deliberately generic — no billing, quota,
// or transport vocabulary — so the seam stays unopinionated: a backend
// reads any backend-specific settings (a DSN, periods) from its own env
// in its constructor, the same discipline as continuation/trace
// StoreConfig. The bundled "zap" sink uses only Logger; the other fields
// are there for an out-of-tree sink that aggregates per node.
type SinkConfig struct {
	// Epoch is a per-process token that changes every boot — the
	// counter-reset boundary for a sink that keeps cumulative state.
	Epoch string
	// NodeID is a stable identity for this chassis (FQDN-ish), for
	// attribution when many nodes write to one store.
	NodeID string
	// DataDir is where a sink may put a node-local database.
	DataDir string
	// Logger is the chassis logger; a sink may emit observability lines.
	Logger *zap.Logger
}

// Constructor builds a Sink from resolved config. Called by Open.
type Constructor func(SinkConfig) (Sink, error)

// registry maps sink name → constructor. The bundled "zap" sink
// self-registers from this package's init (usage.go); an out-of-tree
// sink (file/Kafka/OTEL/rollup) registers from its own init and the
// chassis activates it with a blank import — no change here required.
var registry = map[string]Constructor{}

// Register adds a sink constructor. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
func Register(name string, c Constructor) {
	registry[name] = c
}

// Open constructs the named sink. Unknown name is a startup error
// listing what is available (sorted for a stable message).
func Open(name string, cfg SinkConfig) (Sink, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("usage: unknown sink %q (available: %v)", name, avail)
	}
	return c(cfg)
}
