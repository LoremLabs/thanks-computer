package imap

import "fmt"

// Config carries backend-selecting options resolved from chassis config. The
// bundled "sqlite" backend uses DBPath. Adding a field here doesn't affect
// existing backends (same posture as scheduled.Config / vector.Config).
type Config struct {
	// DBPath is the bundled SQLite backend's file path (--imap-db-path).
	DBPath string
}

// Constructor builds a Store from resolved config. It is expected to open its
// backing DB and call Store.EnsureSchema before returning.
type Constructor func(Config) (*Store, error)

// backends maps backend name → constructor. The bundled "sqlite" backend
// registers itself (sqlite.go init()); an overlay registers a shared
// Postgres backend the same way so a head on one node serves what an op on
// another node projected.
var backends = map[string]Constructor{}

// Register adds a backend constructor. Called from a backend package's init().
func Register(name string, c Constructor) {
	backends[name] = c
}

// Open constructs the named backend. Unknown name is a startup error listing
// what is available (so a misconfigured --imap-store fails loudly).
func Open(name string, cfg Config) (*Store, error) {
	c, ok := backends[name]
	if !ok {
		avail := make([]string, 0, len(backends))
		for k := range backends {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("imap: unknown store %q (available: %v)", name, avail)
	}
	return c(cfg)
}
