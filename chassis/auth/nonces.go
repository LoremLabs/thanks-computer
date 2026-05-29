package auth

import (
	"context"
	"errors"
	"sync"
	"time"
)

// NonceStore backs replay-defense. The middleware calls Use on every
// signed request; ErrReplay means the (key, nonce) pair was already
// seen within its TTL.
//
// Backed by an in-process sliding-window structure: N+1 buckets that
// rotate every ttl/N. Inserts go into the head bucket; lookups scan all
// buckets; on rotation, the oldest bucket is cleared wholesale, which
// gives O(1) eviction without a GC walk.
//
// Per the design note in db/schema/sqlite/0005_auth.sql, nonces are
// local-only replay protection — never replicated, never persisted.
// Restart loss is bounded by the signature's created/expires freshness
// window (≤ Skew, typically 60s), so memory-only is the natural fit.
type NonceStore struct {
	ttl       time.Duration
	bucketDur time.Duration
	now       func() time.Time

	mu       sync.Mutex
	buckets  []map[string]struct{}
	head     int
	rotateAt time.Time
}

// ErrReplay signals the nonce was already used for this key.
var ErrReplay = errors.New("nonce_replay")

// nonceBuckets is the number of historic buckets retained alongside the
// active head bucket. With N=6 and ttl=10m each bucket covers ~100s, and
// worst-case retention is ttl + bucketDur ≈ 11.67 min.
const nonceBuckets = 6

// NewNonceStore returns a NonceStore that retains (key_id, nonce) pairs
// for at least ttl. ttl ≤ 0 falls back to 10 minutes.
func NewNonceStore(ttl time.Duration) *NonceStore {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	s := &NonceStore{
		ttl:       ttl,
		bucketDur: ttl / nonceBuckets,
		now:       time.Now,
		buckets:   make([]map[string]struct{}, nonceBuckets+1),
	}
	for i := range s.buckets {
		s.buckets[i] = make(map[string]struct{})
	}
	s.rotateAt = s.now().Add(s.bucketDur)
	return s
}

// Use records the nonce as seen. Returns nil on success, ErrReplay if
// the same (key, nonce) pair is already on file (within TTL).
//
// The ctx argument is unused — the in-memory path has no I/O — but stays
// on the signature so the middleware closure at chassis/auth/middleware.go
// doesn't need to change.
func (s *NonceStore) Use(_ context.Context, keyID, nonce string) error {
	if s == nil {
		return errors.New("nonces: nil store")
	}

	// \x00 is a safe separator: key IDs are identifiers and nonces are
	// typically base64 — neither contains NUL — so the composite key is
	// unambiguous.
	key := keyID + "\x00" + nonce

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	for !now.Before(s.rotateAt) {
		s.head = (s.head + 1) % len(s.buckets)
		s.buckets[s.head] = make(map[string]struct{})
		s.rotateAt = s.rotateAt.Add(s.bucketDur)
	}

	for _, b := range s.buckets {
		if _, ok := b[key]; ok {
			return ErrReplay
		}
	}
	s.buckets[s.head][key] = struct{}{}
	return nil
}

// CheckFunc returns a closure that wraps Use, matching the signature
// expected by signature.VerifyOptions.NonceCheck.
func (s *NonceStore) CheckFunc(ctx context.Context) func(keyID, nonce string) error {
	return func(keyID, nonce string) error { return s.Use(ctx, keyID, nonce) }
}
