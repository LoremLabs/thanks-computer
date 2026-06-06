package filecas

import "fmt"

// StoreConfig carries backend-selecting options resolved from chassis
// config. The file backend uses FileDir; the S3 fields are the fleet
// overlay seam (unused in open core). CacheBytes/MaxEntryBytes configure
// the LRU that Open wraps around any backend.
type StoreConfig struct {
	FileDir string

	// Reserved for the out-of-tree S3-compatible backend (fleet overlay).
	S3Bucket string
	S3Prefix string

	// CacheBytes is the in-memory LRU budget (bytes) fronting Get. 0
	// disables the cache (pure pass-through to the backend).
	CacheBytes int64
	// MaxEntryBytes is the per-entry cache guard: a blob larger than this
	// is served straight from the backend and never cached, so one large
	// file can't evict the whole cache. 0 means no per-entry guard.
	MaxEntryBytes int64
}

// Constructor builds a backend Store from resolved config.
type Constructor func(StoreConfig) (Store, error)

// registry maps backend name → constructor. Backends self-register via
// init() (see filestore). The "file" backend is built in; an additional
// backend in a separate package registers itself the same way.
var registry = map[string]Constructor{}

// Register adds a backend constructor. Called from a backend package's
// init(); the chassis activates a backend with a blank import.
func Register(name string, c Constructor) {
	registry[name] = c
}

// Open constructs the named backend and, when CacheBytes > 0, wraps it in
// the byte-bounded LRU. Unknown name is a startup error listing what is
// available.
func Open(name string, cfg StoreConfig) (Store, error) {
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry))
		for k := range registry {
			avail = append(avail, k)
		}
		return nil, fmt.Errorf("filecas: unknown store %q (available: %v)", name, avail)
	}
	s, err := c(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.CacheBytes > 0 {
		s = newCachedStore(s, cfg.CacheBytes, cfg.MaxEntryBytes)
	}
	return s, nil
}
