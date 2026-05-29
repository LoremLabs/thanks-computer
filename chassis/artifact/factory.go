package artifact

import "fmt"

// StoreConfig carries backend-selecting options resolved from chassis
// config. The file backend uses FileDir. Additional backends may extend
// this struct with their own fields; the existing backends and callers are
// unaffected by added fields.
type StoreConfig struct {
	FileDir string
}

// Constructor builds a Store from resolved config.
type Constructor func(StoreConfig) (Store, error)

// registry maps backend name → constructor. Backends self-register via
// init() (see filestore). The "file" backend is built in; the map is the
// extension seam — an additional backend in a separate package registers
// itself the same way, with no change to the runner or this package.
var registry = map[string]Constructor{}

// Register adds a backend constructor. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
func Register(name string, c Constructor) {
	registry[name] = c
}

// Open constructs the named backend. Unknown name is a startup error
// listing what is available.
func Open(name string, cfg StoreConfig) (Store, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("artifact: unknown store %q (available: %v)", name, avail)
	}
	return c(cfg)
}
