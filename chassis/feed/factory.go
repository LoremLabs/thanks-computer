package feed

import "fmt"

// SourceConfig carries backend-selecting options resolved from chassis
// config. The file backend uses FileDir. Additional backends may extend
// this struct with their own fields; existing backends and callers are
// unaffected by added fields.
type SourceConfig struct {
	FileDir string
}

// Constructor builds a Source from resolved config.
type Constructor func(SourceConfig) (Source, error)

// registry maps backend name → constructor. Backends self-register via
// init(). The "nop" and "file" backends are built in; the map is the
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
func Open(name string, cfg SourceConfig) (Source, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("feed: unknown source %q (available: %v)", name, avail)
	}
	return c(cfg)
}
