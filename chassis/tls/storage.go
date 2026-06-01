package tls

import (
	"strings"

	"github.com/caddyserver/certmagic"
)

// Cert/account storage seam. The bundled default is the filesystem; a
// shared backend (e.g. Postgres, so any node loads the same certs + ACME
// account and certmagic's storage lock serialises issuance) is registered
// out of tree by a downstream overlay — the same "driver registered out of
// tree" rule as chassis/auth/registry/dialect.go. Core compiles only the
// file backend.

// storageFactories maps a DSN scheme (e.g. "postgres") to a certmagic
// storage constructor. Empty in core; populated by an overlay init().
var storageFactories = map[string]func(dsn string) (certmagic.Storage, error){}

// RegisterStorage registers a certmagic.Storage factory for a DSN scheme.
// Called from a downstream overlay; core registers nothing.
func RegisterStorage(scheme string, f func(dsn string) (certmagic.Storage, error)) {
	storageFactories[strings.ToLower(strings.TrimSpace(scheme))] = f
}

// storageForDSN selects the storage backend. An empty DSN (or any scheme
// without a registered factory) yields file storage at path — the safe,
// single-node default. A recognised scheme builds the registered backend.
func storageForDSN(dsn, path string) (certmagic.Storage, error) {
	s := strings.TrimSpace(dsn)
	if s != "" {
		scheme := s
		if i := strings.Index(s, ":"); i >= 0 {
			scheme = s[:i]
		}
		if f, ok := storageFactories[strings.ToLower(scheme)]; ok {
			return f(s)
		}
	}
	if path == "" {
		path = "acme"
	}
	return &certmagic.FileStorage{Path: path}, nil
}
