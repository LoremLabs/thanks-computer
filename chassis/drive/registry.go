package drive

import "fmt"

// Config carries backend-selecting options resolved from chassis config for
// the INDEX backend. The bundled "sqlite" backend uses DBPath; every backend
// receives the already-opened object store to hold the bytes. Adding a field
// here doesn't affect existing backends (same posture as contacts.Config).
type Config struct {
	// DBPath is the bundled SQLite backend's file path (--drive-db-path).
	DBPath string
	// Objects is the object store the index points into (--drive-objects).
	Objects ObjectStore
}

// Constructor builds a Store from resolved config. It is expected to open its
// backing DB and call Store.EnsureSchema before returning.
type Constructor func(Config) (*Store, error)

// backends maps index backend name → constructor. The bundled "sqlite"
// backend registers itself (sqlite.go init()); an overlay registers a shared
// Postgres backend the same way so a head on one node serves what an op on
// another node wrote.
var backends = map[string]Constructor{}

// Register adds an index backend constructor. Called from a backend
// package's init().
func Register(name string, c Constructor) {
	backends[name] = c
}

// Open constructs the named index backend. Unknown name is a startup error
// listing what is available (so a misconfigured --drive-store fails loudly).
func Open(name string, cfg Config) (*Store, error) {
	c, ok := backends[name]
	if !ok {
		avail := make([]string, 0, len(backends))
		for k := range backends {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("drive: unknown store %q (available: %v)", name, avail)
	}
	if cfg.Objects == nil {
		return nil, fmt.Errorf("drive: store %q needs an object store", name)
	}
	return c(cfg)
}
