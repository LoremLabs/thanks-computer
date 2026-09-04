package imap

import (
	"sync"
)

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
