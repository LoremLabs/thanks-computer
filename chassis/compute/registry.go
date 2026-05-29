package compute

import (
	"fmt"
	"sort"
)

// EngineConfig carries backend-selecting options resolved from chassis config.
// Engines extend it with their own fields without breaking existing callers
// (same convention as artifact.StoreConfig).
type EngineConfig struct {
	// MaxMemoryMB caps guest memory. Engines that bound memory at
	// construction (e.g. wazero's runtime memory-limit) read it here; 0
	// means "engine default".
	MaxMemoryMB int
}

// Constructor builds an Engine from resolved config.
type Constructor func(EngineConfig) (Engine, error)

// registry maps engine name → constructor. Engines self-register via init()
// (see compute/identity, compute/wazero); the chassis activates one with a
// blank import. This is the extension seam — a new engine in a separate
// package registers itself the same way, with no change here or to callers.
var registry = map[string]Constructor{}

// RegisterEngine adds an engine constructor. Called from an engine package's
// init().
func RegisterEngine(name string, c Constructor) { registry[name] = c }

// OpenEngine constructs the named engine. Unknown name is an error listing
// what is available.
func OpenEngine(name string, cfg EngineConfig) (Engine, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("compute: unknown engine %q (available: %v)", name, avail)
	}
	return c(cfg)
}
