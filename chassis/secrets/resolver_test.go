package secrets

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestResolverMaterializeForOpDirect(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "a", []byte("v1")); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := NewResolver(store, nil)
	got, meta, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if string(got) != "v1" {
		t.Errorf("got %q, want v1", got)
	}
	if meta.VersionNo != 1 {
		t.Errorf("VersionNo = %d, want 1", meta.VersionNo)
	}
}

func TestResolverHonorsStackFallback(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	pay := "payments"
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "a", []byte("tw-value")); err != nil {
		t.Fatalf("create tenant-wide: %v", err)
	}
	if _, err := store.CreateSecret(ctx, "tnt_x", &pay, "STRIPE", "", "a", []byte("stack-value")); err != nil {
		t.Fatalf("create stack: %v", err)
	}

	r := NewResolver(store, nil)

	got, _, _ := r.MaterializeForOp(ctx, "tnt_x", "payments", "STRIPE")
	if string(got) != "stack-value" {
		t.Errorf("stack-scoped wins: got %q, want stack-value", got)
	}
	got, _, _ = r.MaterializeForOp(ctx, "tnt_x", "billing", "STRIPE")
	if string(got) != "tw-value" {
		t.Errorf("fallback: got %q, want tw-value", got)
	}
}

// resolverWithCount is the test-only Resolver shape that counts
// underlying Store hits. Mirrors Resolver but with a swappable
// materialize function so we can intercept.
type resolverWithCount struct {
	materialize func(ctx context.Context, tenantID, stack, name string) ([]byte, *SecretMetadata, error)
	hitCount    int64
}

func (r *resolverWithCount) MaterializeForOp(ctx context.Context, tenantID, stack, name string) ([]byte, *SecretMetadata, error) {
	cache := requestCacheFromContext(ctx)
	if cache != nil {
		if pt, meta, ok := cache.get(tenantID, stack, name); ok {
			return pt, meta, nil
		}
	}
	atomic.AddInt64(&r.hitCount, 1)
	pt, meta, err := r.materialize(ctx, tenantID, stack, name)
	if err != nil {
		return nil, nil, err
	}
	if cache != nil {
		cache.set(tenantID, stack, name, pt, meta)
	}
	return pt, meta, nil
}

// TestResolverCacheDeduplicatesWithCounter is the actual proof: with
// a request cache installed on ctx, 5 calls for the same name hit
// the underlying materialize function exactly once.
func TestResolverCacheDeduplicatesWithCounter(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "a", []byte("v")); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &resolverWithCount{
		materialize: store.MaterializeSecretForOp,
	}

	ctx, cleanup := WithRequestCache(ctx)
	defer cleanup()

	for i := 0; i < 5; i++ {
		got, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if string(got) != "v" {
			t.Errorf("call %d returned %q", i, got)
		}
	}

	if got := atomic.LoadInt64(&r.hitCount); got != 1 {
		t.Errorf("underlying materialize hit %d times, want 1 (cache miss/hit ratio is wrong)", got)
	}
}

func TestResolverCacheScopedByStack(t *testing.T) {
	// A cache hit for (tenant_id, stack=payments, name=STRIPE) must
	// not satisfy a query for (tenant_id, stack=billing, name=STRIPE).
	// Different stacks could resolve to different rows.
	ctx := context.Background()
	store := newTestStore(t)
	pay := "payments"
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "a", []byte("tw")); err != nil {
		t.Fatalf("c1: %v", err)
	}
	if _, err := store.CreateSecret(ctx, "tnt_x", &pay, "STRIPE", "", "a", []byte("pay")); err != nil {
		t.Fatalf("c2: %v", err)
	}

	r := &resolverWithCount{materialize: store.MaterializeSecretForOp}
	ctx, cleanup := WithRequestCache(ctx)
	defer cleanup()

	got, _, _ := r.MaterializeForOp(ctx, "tnt_x", "payments", "STRIPE")
	if string(got) != "pay" {
		t.Errorf("first call: got %q, want pay", got)
	}
	got, _, _ = r.MaterializeForOp(ctx, "tnt_x", "billing", "STRIPE")
	if string(got) != "tw" {
		t.Errorf("second call (different stack): got %q, want tw (fallback) — cache must NOT have served the 'payments' value", got)
	}
	if got := atomic.LoadInt64(&r.hitCount); got != 2 {
		t.Errorf("hitCount = %d, want 2 (different stacks should not share cache slot)", got)
	}
}

func TestResolverNoCacheWithoutWithRequestCache(t *testing.T) {
	// Without WithRequestCache on the ctx, every call goes through to
	// the underlying materialize. This is the PR 1 unit-test path.
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "a", []byte("v")); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &resolverWithCount{materialize: store.MaterializeSecretForOp}
	for i := 0; i < 3; i++ {
		if _, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&r.hitCount); got != 3 {
		t.Errorf("without cache, hitCount = %d, want 3 (every call should pass through)", got)
	}
}

func TestRequestCacheIndependentOfCallerSlice(t *testing.T) {
	// The cache holds its OWN copy of each cleartext, so the caller
	// can mutate or zero the returned slice without corrupting the
	// cache. Subsequent ops in the same request still see correct
	// cleartext. This is the load-bearing ownership invariant that
	// makes bag.Zero() + cache.zero() safe to compose.
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "a", []byte("super-secret")); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := NewResolver(store, nil)
	ctx, cleanup := WithRequestCache(ctx)
	defer cleanup()

	// First call: cache miss → store decrypt → cache stores its copy.
	pt1, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE")
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if string(pt1) != "super-secret" {
		t.Errorf("first: got %q, want super-secret", pt1)
	}

	// Caller zeros its slice (simulating bag.Zero()).
	Zero(pt1)

	// Second call: cache hit → returns fresh copy. Must NOT be zeroed.
	pt2, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE")
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if string(pt2) != "super-secret" {
		t.Errorf("second (cache hit after caller-zero of first): got %q, want super-secret — cache and caller slices not independent", pt2)
	}

	// And pt1/pt2 must be different slice headers (separate allocations).
	if len(pt1) > 0 && len(pt2) > 0 && &pt1[0] == &pt2[0] {
		t.Errorf("pt1 and pt2 share the same backing array — they must be independent copies")
	}
}

func TestRequestCacheCleanupEmptiesCache(t *testing.T) {
	// After cleanup, the cache's internal map is empty. We verify
	// this indirectly: a subsequent Materialize call (with the now-
	// cleaned cache still installed on ctx) goes through to the
	// store, not to a stale cache entry. Use the counter shim.
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "STRIPE", "", "a", []byte("v")); err != nil {
		t.Fatalf("create: %v", err)
	}

	r := &resolverWithCount{materialize: store.MaterializeSecretForOp}
	ctx, cleanup := WithRequestCache(ctx)

	// First call: store hit.
	if _, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if got := atomic.LoadInt64(&r.hitCount); got != 1 {
		t.Fatalf("first hitCount = %d, want 1", got)
	}

	// Second call (before cleanup): cache hit, NO store call.
	if _, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE"); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := atomic.LoadInt64(&r.hitCount); got != 1 {
		t.Fatalf("after cache hit, hitCount = %d, want 1 (cache should serve)", got)
	}

	// Cleanup wipes the cache.
	cleanup()

	// Third call (after cleanup): cache empty, store hit again.
	if _, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "STRIPE"); err != nil {
		t.Fatalf("third: %v", err)
	}
	if got := atomic.LoadInt64(&r.hitCount); got != 2 {
		t.Errorf("after cleanup, hitCount = %d, want 2 (cache cleared)", got)
	}
}

func TestResolverConcurrentRequestsDontShareCache(t *testing.T) {
	// Two concurrent requests get two separate caches (each call to
	// WithRequestCache returns a fresh cache). Mutual writes don't
	// stomp each other.
	ctx := context.Background()
	store := newTestStore(t)
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "A", "", "a", []byte("aval")); err != nil {
		t.Fatalf("c1: %v", err)
	}
	if _, err := store.CreateSecret(ctx, "tnt_x", nil, "B", "", "a", []byte("bval")); err != nil {
		t.Fatalf("c2: %v", err)
	}

	r := NewResolver(store, nil)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cleanup := WithRequestCache(ctx)
			defer cleanup()
			name := "A"
			want := "aval"
			if i%2 == 1 {
				name = "B"
				want = "bval"
			}
			for j := 0; j < 3; j++ {
				got, _, err := r.MaterializeForOp(ctx, "tnt_x", "", name)
				if err != nil {
					t.Errorf("goroutine %d call %d: %v", i, j, err)
					return
				}
				if string(got) != want {
					t.Errorf("goroutine %d call %d: got %q, want %q", i, j, got, want)
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestResolverMissingSecret(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)
	r := NewResolver(store, nil)
	_, _, err := r.MaterializeForOp(ctx, "tnt_x", "", "NEVER_CREATED")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("expected ErrSecretNotFound, got: %v", err)
	}
}
