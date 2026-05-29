// Package throttle is a small per-key fixed-window rate limiter.
// It exists to gate the chassis's unsigned credential-mint endpoints
// (`/auth/dev/enroll`, `/auth/invitations/consume`) against brute-force
// probing without dragging in a full rate-limit library.
//
// Sizing rationale: state is one int + one timestamp per active key
// (typically the caller's IP). Lazy cleanup on every Allow evicts
// buckets older than 2× the window so the map stays bounded under
// churn even when long-lived attackers cycle through IPs.
//
// What this is NOT:
//   - distributed (single process; restart resets counters)
//   - sliding-window (boundary-burst behaviour is acceptable for our
//     anti-abuse threat model)
//   - per-user (caller carries no identity at the gate; that's the
//     point of throttling unsigned endpoints)
package throttle

import (
	"sync"
	"time"
)

// Throttle is a per-key fixed-window counter. Zero value is not
// usable — call New.
type Throttle struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	limit   int
	window  time.Duration
}

// bucket is the per-key counter state. Two fields, both written
// under Throttle.mu — no per-bucket lock.
type bucket struct {
	count   int       // attempts admitted in the current window
	resetAt time.Time // when this bucket's count clears
}

// New returns a Throttle that admits up to `limit` attempts per
// `window` per key.
//
// limit == 0 (or negative) disables the throttle: every Allow call
// returns ok=true. This is the cheap kill switch used by tests and
// by operators setting TXCO_THROTTLE_DISABLED=1.
//
// window must be positive when limit > 0; a zero window would make
// every attempt count toward the same eternal bucket. The constructor
// clamps to 1s in that case rather than panicking — defensive against
// env-misconfiguration without surfacing an error to startup.
func New(limit int, window time.Duration) *Throttle {
	if limit < 0 {
		limit = 0
	}
	if limit > 0 && window <= 0 {
		window = time.Second
	}
	return &Throttle{
		buckets: make(map[string]*bucket),
		limit:   limit,
		window:  window,
	}
}

// Allow records an attempt for key. Returns (true, 0) when the
// attempt is below threshold (and the count has been bumped); returns
// (false, retryAfter) when the bucket is exhausted, where retryAfter
// is the remaining duration in the current window.
//
// Disabled throttles (limit==0) always return (true, 0) without
// touching state.
//
// Side effect: lazy cleanup of stale buckets. On each call we sweep
// the map and drop any bucket whose resetAt is more than `window`
// in the past — that's twice the active-window age, so we never
// evict a bucket that's still counting. Cleanup is O(n) in map size
// but n is bounded by recent unique callers, which is small in
// practice.
func (t *Throttle) Allow(key string) (bool, time.Duration) {
	if t.limit == 0 {
		return true, 0
	}
	now := time.Now()

	t.mu.Lock()
	defer t.mu.Unlock()

	t.gcLocked(now)

	b, ok := t.buckets[key]
	if !ok || now.After(b.resetAt) {
		// Fresh bucket (or stale-rolled): start a new window.
		t.buckets[key] = &bucket{
			count:   1,
			resetAt: now.Add(t.window),
		}
		return true, 0
	}
	if b.count >= t.limit {
		return false, b.resetAt.Sub(now)
	}
	b.count++
	return true, 0
}

// gcLocked drops buckets whose window ended >= one full window ago.
// Cheaper than a separate sweep goroutine and the upper bound on
// memory is bounded by the number of distinct keys seen in the last
// 2× window, which is small for typical traffic.
//
// Must be called with t.mu held.
func (t *Throttle) gcLocked(now time.Time) {
	cutoff := now.Add(-t.window)
	for k, b := range t.buckets {
		if b.resetAt.Before(cutoff) {
			delete(t.buckets, k)
		}
	}
}

// Size returns the number of live buckets. Exported for tests that
// assert the lazy cleanup actually evicts; not intended for callers
// to inspect at runtime.
func (t *Throttle) Size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buckets)
}
