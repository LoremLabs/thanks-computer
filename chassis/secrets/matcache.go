package secrets

import (
	"sync"
	"time"
)

// materializeCache caches the ENCRYPTED active version + metadata per
// (tenant, requested stack, name) so a MaterializeSecretForOp hit is zero
// DB round trips — on a Postgres runtime the 2-3 point reads per
// secret-using op (every ai:// call, every telemetry flush) were WAN
// round trips. Decryption stays per-call, so cleartext lifetime and the
// zeroing discipline are unchanged: only ciphertext + metadata live here,
// the same material the DB row holds.
//
// Staleness contract: writes through this Store (create / rotate / revoke /
// update-description) flush synchronously after commit; a write from
// another node arrives via the control feed and flushes through the
// dbcache-reload hook (app.go); the TTL bounds the worst case (a node cut
// off from the feed). A stale hit decrypts the PREVIOUS value — the same
// window the SQLite fleet always had between an apply and its applier
// pass. There is a benign race: a materialize that read pre-commit can
// re-fill the cache just after a writer's flush; the TTL bounds that too.
const materializeTTL = 5 * time.Minute

type matEntry struct {
	meta SecretMetadata  // by value; callers get a fresh copy per hit
	es   EncryptedSecret // scan-owned slices; Decrypt never mutates them
	at   time.Time
}

type materializeCache struct {
	mu      sync.Mutex
	entries map[string]matEntry
}

func matKey(tenantID, stack, name string) string {
	return tenantID + "\x00" + stack + "\x00" + name
}

func (c *materializeCache) get(key string, now time.Time) (matEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return matEntry{}, false
	}
	if now.Sub(e.at) > materializeTTL {
		delete(c.entries, key)
		return matEntry{}, false
	}
	return e, true
}

func (c *materializeCache) set(key string, e matEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]matEntry{}
	}
	c.entries[key] = e
}

func (c *materializeCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *materializeCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}
