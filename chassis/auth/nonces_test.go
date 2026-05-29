package auth

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestNonceStore returns a NonceStore whose clock is fully under the
// test's control via the returned advance closure. Tests that don't care
// about time can ignore advance entirely — the clock simply doesn't move.
func newTestNonceStore(t *testing.T, ttl time.Duration) (*NonceStore, func(time.Duration)) {
	t.Helper()
	var (
		mu  sync.Mutex
		now = time.Unix(1_700_000_000, 0).UTC() // arbitrary fixed epoch
	)
	get := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
	// NewNonceStore reads `now` once during construction to seed
	// rotateAt; swap the function in immediately after so subsequent
	// Use() calls see the controlled clock.
	s := NewNonceStore(ttl)
	s.now = get
	// Reset rotateAt against the controlled clock so the first
	// scheduled rotation aligns with the injected origin, not wall time.
	s.rotateAt = get().Add(s.bucketDur)
	return s, advance
}

func TestNonceFirstUseSucceeds(t *testing.T) {
	s, _ := newTestNonceStore(t, time.Minute)
	if err := s.Use(context.Background(), "k1", "n1"); err != nil {
		t.Errorf("first use returned %v", err)
	}
}

// TestNonceSecondUseSameKeyIsReplay — the core replay-defense guarantee.
func TestNonceSecondUseSameKeyIsReplay(t *testing.T) {
	s, _ := newTestNonceStore(t, time.Minute)
	ctx := context.Background()
	if err := s.Use(ctx, "k1", "n1"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := s.Use(ctx, "k1", "n1"); err != ErrReplay {
		t.Errorf("got %v, want ErrReplay", err)
	}
}

// TestNonceSameNonceDifferentKey: the composite (key_id, nonce) identity
// means two different keys can use the same nonce string.
func TestNonceSameNonceDifferentKey(t *testing.T) {
	s, _ := newTestNonceStore(t, time.Minute)
	ctx := context.Background()
	if err := s.Use(ctx, "k1", "shared-nonce"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := s.Use(ctx, "k2", "shared-nonce"); err != nil {
		t.Errorf("got %v, want nil (different key, same nonce should be fine)", err)
	}
}

// TestNonceEvictionAfterTTL: once enough time has passed that the entry's
// bucket has rotated out, the same (key, nonce) becomes usable again.
func TestNonceEvictionAfterTTL(t *testing.T) {
	ttl := time.Minute
	s, advance := newTestNonceStore(t, ttl)
	ctx := context.Background()
	if err := s.Use(ctx, "k1", "n1"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	// Advance past ttl + bucketDur so every historic bucket has rotated.
	advance(ttl + s.bucketDur + time.Second)
	if err := s.Use(ctx, "k1", "n1"); err != nil {
		t.Errorf("after rotation: got %v, want nil", err)
	}
}

// TestNonceStillReplayBeforeTTL: rotating one bucket forward should not
// drop entries from the still-active window.
func TestNonceStillReplayBeforeTTL(t *testing.T) {
	ttl := time.Minute
	s, advance := newTestNonceStore(t, ttl)
	ctx := context.Background()
	if err := s.Use(ctx, "k1", "n1"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	// Advance one bucket forward — entry is now in a historic bucket,
	// but well within ttl.
	advance(s.bucketDur + time.Second)
	if err := s.Use(ctx, "k1", "n1"); err != ErrReplay {
		t.Errorf("after one bucket advance: got %v, want ErrReplay", err)
	}
}

// TestNonceRotationDropsOldestBucket walks the rotation ring one bucket
// at a time. The entry sits in the bucket that was active at insert
// time; each rotation advances head and clears the new head bucket. The
// entry is evicted only when head wraps back around to its original
// bucket — i.e. after nonceBuckets+1 rotations.
func TestNonceRotationDropsOldestBucket(t *testing.T) {
	ttl := time.Minute
	s, advance := newTestNonceStore(t, ttl)
	ctx := context.Background()
	if err := s.Use(ctx, "k1", "n1"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	for i := 0; i < nonceBuckets+1; i++ {
		advance(s.bucketDur)
		err := s.Use(ctx, "k1", "n1")
		if i < nonceBuckets {
			if err != ErrReplay {
				t.Fatalf("after %d rotations: got %v, want ErrReplay", i+1, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("after %d rotations: got %v, want nil (entry's bucket should have wrapped)", i+1, err)
		}
	}
}

// TestNonceManyBucketsAdvanced: leap forward far more than ttl in one
// step — every historic bucket should clear in one go without a GC walk.
func TestNonceManyBucketsAdvanced(t *testing.T) {
	ttl := time.Minute
	s, advance := newTestNonceStore(t, ttl)
	ctx := context.Background()
	if err := s.Use(ctx, "k1", "n1"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	advance(ttl * 100) // chassis idle for ~1000x ttl
	if err := s.Use(ctx, "k1", "n1"); err != nil {
		t.Errorf("after long idle: got %v, want nil", err)
	}
}

// TestNonceCheckFunc — the closure wrapper preserves replay semantics.
func TestNonceCheckFunc(t *testing.T) {
	s, _ := newTestNonceStore(t, time.Minute)
	check := s.CheckFunc(context.Background())
	if err := check("k1", "n1"); err != nil {
		t.Errorf("first call: %v", err)
	}
	if err := check("k1", "n1"); err != ErrReplay {
		t.Errorf("second call: got %v, want ErrReplay", err)
	}
}

// TestNonceConcurrentDisjoint: N goroutines each inserting unique
// nonces all succeed.
func TestNonceConcurrentDisjoint(t *testing.T) {
	s, _ := newTestNonceStore(t, time.Minute)
	const goroutines = 100
	const perRoutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	var failures int32
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perRoutine; i++ {
				if err := s.Use(context.Background(), "k", nonceID(g, i)); err != nil {
					atomic.AddInt32(&failures, 1)
				}
			}
		}()
	}
	wg.Wait()
	if failures != 0 {
		t.Errorf("%d unexpected replay errors across disjoint nonces", failures)
	}
}

// TestNonceConcurrentSame: N goroutines racing to claim the same nonce —
// exactly one should succeed.
func TestNonceConcurrentSame(t *testing.T) {
	s, _ := newTestNonceStore(t, time.Minute)
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	var successes int32
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			if err := s.Use(context.Background(), "k", "race-target"); err == nil {
				atomic.AddInt32(&successes, 1)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Errorf("got %d successes, want exactly 1", successes)
	}
}

// TestNonceNilSafe: callers may legitimately end up with a nil store
// before construction completes; surface a clean error rather than
// panicking.
func TestNonceNilSafe(t *testing.T) {
	var s *NonceStore
	if err := s.Use(context.Background(), "k", "n"); err == nil {
		t.Errorf("nil receiver: got nil error, want non-nil")
	}
}

func nonceID(g, i int) string {
	return "g" + strconv.Itoa(g) + "-i" + strconv.Itoa(i)
}
