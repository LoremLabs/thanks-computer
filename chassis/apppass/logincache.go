package apppass

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// LoginCache remembers recently verified (username, hash, password)
// triples so a desktop client's 5–10 parallel LOGINs per wake — or a CalDAV
// client's dozens of Basic-authenticated requests per refresh — cost one
// argon2id verify, not one each. The key is a hash of all three, so a
// rotated password (new pw_hash) misses by construction, and a stale entry
// can only ever admit a password that was correct for that hash.
type LoginCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]time.Time
	now     func() time.Time
}

// NewLoginCache builds a cache holding at most max entries for ttl each.
func NewLoginCache(ttl time.Duration, max int) *LoginCache {
	return &LoginCache{ttl: ttl, max: max, entries: make(map[string]time.Time), now: time.Now}
}

// LoginKey is the cache key for one verified triple.
func LoginKey(username, pwHash, password string) string {
	sum := sha256.Sum256([]byte(username + "\x00" + pwHash + "\x00" + password))
	return hex.EncodeToString(sum[:])
}

// Hit reports whether key was verified within the TTL.
func (c *LoginCache) Hit(key string) bool {
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

// Put records a verified key.
func (c *LoginCache) Put(key string) {
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

// SetClock pins the cache's clock (tests).
func (c *LoginCache) SetClock(now func() time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}
