package trace

import (
	"fmt"
	"sort"
)

// StoreConfig carries backend-selecting options resolved from chassis
// config. Only the file/noop backends are wired in open core; an
// out-of-tree backend (e.g. a queue/object-store shipper for a
// separate-machine admin) registers itself via init() + blank import
// and reads any additional config (endpoint/token) from its OWN env in
// its constructor — the same seam discipline as
// chassis/continuation/factory.go and chassis/artifact/factory.go.
type StoreConfig struct {
	Dir  string
	Mode Mode
}

// Constructor builds a Sink from StoreConfig.
type Constructor func(StoreConfig) (Sink, error)

var registry = map[string]Constructor{}

// Register adds a named Sink backend. Called from init() in the backend
// package; built-ins ("file", "noop") register from this package's own
// init (file.go / noop.go), so no blank import is needed for them.
func Register(name string, c Constructor) { registry[name] = c }

// Open constructs the named backend. Unknown name is a hard error
// (fail-fast at boot) listing what is available.
func Open(name string, cfg StoreConfig) (Sink, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return nil, fmt.Errorf("trace: unknown store %q (available: %v)", name, avail)
	}
	return c(cfg)
}
