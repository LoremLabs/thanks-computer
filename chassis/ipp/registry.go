package ipp

import "fmt"

// Config carries backend-selecting options resolved from chassis config.
// The bundled "sqlite" backend uses DBPath. Adding a field here doesn't
// affect existing backends (same posture as drive.Config).
type Config struct {
	// DBPath is the bundled SQLite backend's file path (--ipp-db-path).
	DBPath string
}

// Constructor builds a Store from resolved config. It is expected to open
// its backing DB and call Store.EnsureSchema before returning.
type Constructor func(Config) (*Store, error)

// backends maps store backend name → constructor. The bundled "sqlite"
// backend registers itself (sqlite.go init()); an overlay registers a shared
// Postgres backend the same way, so a job committed on one node can be
// handed to the bus by another.
var backends = map[string]Constructor{}

// Register adds a store backend constructor. Called from a backend
// package's init().
func Register(name string, c Constructor) {
	backends[name] = c
}

// Open constructs the named backend. Unknown name is a startup error listing
// what is available (so a misconfigured --ipp-store fails loudly).
func Open(name string, cfg Config) (*Store, error) {
	c, ok := backends[name]
	if !ok {
		avail := make([]string, 0, len(backends))
		for k := range backends {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("ipp: unknown store %q (available: %v)", name, avail)
	}
	return c(cfg)
}
