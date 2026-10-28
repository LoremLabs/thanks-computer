package gitstore

import "fmt"

// Config carries the backend-selecting options resolved from chassis
// config. The bundled "sqlite" backend uses DBPath; a shared backend
// (the fleet's Postgres overlay) reads its DSN from the environment.
type Config struct {
	// DBPath is the bundled SQLite backend's file path (--git-db-path).
	DBPath string
}

// Constructor builds a Store from resolved config. It opens its backing DB
// and calls Store.EnsureSchema before returning.
type Constructor func(Config) (*Store, error)

var backends = map[string]Constructor{}

// Register adds a backend constructor. Called from a backend package's
// init(): the bundled sqlite one here, an overlay's Postgres one in the
// service binary.
func Register(name string, c Constructor) {
	backends[name] = c
}

// Open constructs the named backend. An unknown name is a startup error
// listing what is available (so a misconfigured --git-store fails loudly).
func Open(name string, cfg Config) (*Store, error) {
	c, ok := backends[name]
	if !ok {
		avail := make([]string, 0, len(backends))
		for k := range backends {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("gitstore: unknown store %q (available: %v)", name, avail)
	}
	return c(cfg)
}
