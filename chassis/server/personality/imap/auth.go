package imap

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// loginCache remembers recently verified (username, hash, password)
// triples so a desktop client's 5–10 parallel LOGINs per wake cost one
// argon2id verify, not ten. The key is a hash of all three, so a rotated
// password (new pw_hash) misses by construction, and a stale entry can
// only ever admit a password that was correct for that hash.
type loginCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]time.Time
	now     func() time.Time
}

func newLoginCache(ttl time.Duration, max int) *loginCache {
	return &loginCache{ttl: ttl, max: max, entries: make(map[string]time.Time), now: time.Now}
}

func loginKey(username, pwHash, password string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + pwHash + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

func (c *loginCache) hit(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.entries[key]
	if !ok {
		return false
	}
	if c.now().After(exp) {
		delete(c.entries, key)
		return false
	}
	return true
}

func (c *loginCache) put(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= c.max {
		// Cheap eviction: drop expired entries; if still full, drop
		// everything (a rare burst of distinct credentials is not worth
		// an LRU).
		for k, exp := range c.entries {
			if now.After(exp) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= c.max {
			c.entries = make(map[string]time.Time)
		}
	}
	c.entries[key] = now.Add(c.ttl)
}

// connCounter caps simultaneous authenticated connections per account
// (--imap-max-conns-per-account). 0 = unlimited.
type connCounter struct {
	mu  sync.Mutex
	max int
	n   map[string]int
}

func newConnCounter(max int) *connCounter {
	return &connCounter{max: max, n: make(map[string]int)}
}

func (c *connCounter) acquire(username string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.max > 0 && c.n[username] >= c.max {
		return false
	}
	c.n[username]++
	return true
}

func (c *connCounter) release(username string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n[username] <= 1 {
		delete(c.n, username)
		return
	}
	c.n[username]--
}
