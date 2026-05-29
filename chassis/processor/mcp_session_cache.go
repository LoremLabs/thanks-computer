package processor

import (
	"sync"
	"time"
)

// mcpSession is one cached MCP session entry. The chassis caches
// server-minted `Mcp-Session-Id` values per (tenant, endpoint) so
// hot paths can skip the `initialize` + `notifications/initialized`
// round-trips and jump straight to `tools/call`.
type mcpSession struct {
	id       string    // server-minted session id
	cachedAt time.Time // when init succeeded; for diagnostics
	lastUsed time.Time // refreshed on every cache hit; TTL is measured from this
}

// mcpSessionCache is the in-memory, per-process MCP session store.
// Nil-safe at the call sites — if pu.MCPSessions is nil, ExecMCPHTTP
// falls back to the always-init behavior (used by tests and any
// non-server context that constructs Unit directly).
//
// Trade-off in this v0.5 design:
//   - Two goroutines hitting the same cold (tenant, endpoint) at once
//     both fire `initialize` independently — one cache write wins,
//     the other's session id is orphaned (the server GCs it). We
//     accept this duplication; a per-key sync.Once guard is a v0.6
//     concern if monitoring shows it's a real cost.
//   - Caches are per-process. In an HA chassis fleet two replicas
//     keep separate sessions for the same (tenant, endpoint). Same
//     trade — accepted v0.5, swappable to a shared backend later
//     (continuation.Runs-style filestore / Postgres seam) without
//     changing callers since they only touch get/put/evict.
//   - now is injectable so TTL-expiry tests don't have to sleep.
type mcpSessionCache struct {
	mu  sync.RWMutex
	m   map[string]mcpSession
	ttl time.Duration
	now func() time.Time
}

// newMCPSessionCache constructs a cache with the given TTL. ttl is
// the maximum gap between `lastUsed` and the current `now()` before
// a cache entry is treated as a miss. 5 minutes is a conservative
// default — the MCP spec doesn't define session expiry and servers
// vary; 5 min covers the common cases (DeepWiki: no observed expiry)
// without leaving stale handles around forever on stricter servers.
func newMCPSessionCache(ttl time.Duration) *mcpSessionCache {
	return &mcpSessionCache{
		m:   make(map[string]mcpSession),
		ttl: ttl,
		now: time.Now,
	}
}

// EnableMCPSessionCache turns on session caching for hot MCP paths.
// The cache type is unexported (it's an implementation detail of
// the processor); this method is the construction surface that
// server.go uses at boot. Idempotent — calling twice replaces the
// underlying cache (dropping any in-flight entries; safe at boot
// time before any traffic flows).
func (pu *Unit) EnableMCPSessionCache(ttl time.Duration) {
	pu.MCPSessions = newMCPSessionCache(ttl)
}

// sessionCacheKey builds the cache key from a (tenant, endpoint)
// pair. Empty tenant is allowed (un-tenanted contexts like _sys
// share a key namespace). The `|` separator is safe because neither
// tenant slugs nor URLs contain it after our usual normalization.
func sessionCacheKey(tenant, endpoint string) string {
	return tenant + "|" + endpoint
}

// get returns the cached session id for `key`, refreshing its
// `lastUsed` timestamp on a hit. Misses (no entry) and TTL-expired
// entries both return ok=false; TTL-expired entries are also
// evicted as a side-effect so the next put doesn't have to.
func (c *mcpSessionCache) get(key string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[key]
	if !ok {
		return "", false
	}
	if c.now().Sub(s.lastUsed) > c.ttl {
		delete(c.m, key)
		return "", false
	}
	s.lastUsed = c.now()
	c.m[key] = s
	return s.id, true
}

// put stores or refreshes the session id for `key`. Overwrites any
// existing entry — the caller is responsible for evicting first if
// the new sid differs (e.g. after a server-minted rotation).
func (c *mcpSessionCache) put(key, sid string) {
	if c == nil || sid == "" {
		return
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = mcpSession{id: sid, cachedAt: now, lastUsed: now}
}

// evict removes the entry for `key`. Called when a downstream MCP
// response signals the server has lost or rejected our session id,
// so the next call re-runs the full initialize lifecycle.
func (c *mcpSessionCache) evict(key string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

// len reports the current cache size. Used by tests to assert
// eviction behavior without poking at the internal map.
func (c *mcpSessionCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}
