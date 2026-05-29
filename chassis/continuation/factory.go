package continuation

import "fmt"

// StoreConfig carries backend-selecting options resolved from chassis
// config. Only the file backend is wired in open core; the S3 fields are
// reserved so an out-of-tree backend can register itself without changing
// this signature.
type StoreConfig struct {
	FileDir string

	// Reserved for an out-of-tree S3-compatible backend (enterprise seam).
	S3Bucket string
	S3Prefix string
}

// Constructor builds a Store from resolved config.
type Constructor func(StoreConfig) (Store, error)

// registry maps backend name → constructor. Backends self-register via
// init() (see filestore). Open core registers only "file"; the map is the
// reserved seam for an out-of-tree "s3" added later behind a build tag or
// separate module — no change to the runner or this package required.
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
		return nil, fmt.Errorf("continuation: unknown store %q (available: %v)", name, avail)
	}
	return c(cfg)
}
