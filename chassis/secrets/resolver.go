package secrets

import (
	"context"
	"fmt"
	"sync"
)

// Resolver is the thin layer between PR 3's processor splice and the
// Store. It exists to add per-request memoization on top of the
// Store's per-call materialization: a NAME used by multiple ops in
// one request decrypts ONCE, not N times.
//
// Resolver is constructed by chassis/app/app.go at boot when a master
// key is configured (PR 2 wiring). It's held on the processor.Unit
// for the request path to consult. If no master key is configured,
// the chassis holds a nil Resolver and the PR 3 splice fails loud
// with `secret_store_unavailable` whenever an op declares secrets.
//
// SlugToID, when non-nil, lets callers materialize by tenant slug
// (the runtime identity pinned on request context) instead of by
// tenant_id (the immutable storage key in the tenant_secrets table).
// app.go wires this to tenants.Store.LookupBySlug at boot. If nil,
// MaterializeForOpSlug returns an error.
type Resolver struct {
	store    *Store
	slugToID func(ctx context.Context, slug string) (string, error)
}

// NewResolver returns a Resolver over the given Store. The Store
// must have a non-nil MasterKeyProvider — pass nil only if you want
// every MaterializeForOp call to fail (e.g. in tests that exercise
// the "feature off" path).
//
// slugToID is optional; pass nil if the caller will only use
// MaterializeForOp (tenant_id form) and never MaterializeForOpSlug.
// The processor splice (PR 3) requires it; admin handlers (PR 4)
// will too once they're wired.
func NewResolver(store *Store, slugToID func(ctx context.Context, slug string) (string, error)) *Resolver {
	return &Resolver{store: store, slugToID: slugToID}
}

// Store exposes the Resolver's underlying Store for admin CRUD.
// The Resolver is the read-side façade (with caching); the Store
// is the full backend (create / rotate / revoke / etc.). PR 4's
// admin endpoints construct nothing of their own; they just call
// methods on this Store.
func (r *Resolver) Store() *Store { return r.store }

// MaterializeForOp decrypts (tenant, stack, name) → cleartext.
// Honors the stack-scoped → tenant-wide fallback in design §2.
//
// If a per-request cache is installed on ctx via WithRequestCache,
// the result is memoized under a (tenantID, stack-or-fallback, name)
// key so subsequent calls in the same request decrypt zero times.
// The cache is INTENTIONALLY NOT thread-safe-shared across requests
// — it's request-scoped only — and Get/Set are guarded by a per-
// cache mutex so concurrent ops within one request stay safe.
//
// **Slice ownership**: each call returns a slice that the caller
// owns and may zero independently. The cache holds its own private
// copy, so a caller's `bag.Zero()` on its returned slice does not
// corrupt the cache's entry — subsequent ops in the same request
// asking for the same name still get correct cleartext. The cache's
// cleanup wipes the cache's copies once at request end.
//
// Without a cache on ctx, the call goes straight to the Store.
//
// Caller is responsible for zeroing the returned slice when done
// (SecretBag.Zero does this automatically for the PR 3 wiring).
func (r *Resolver) MaterializeForOp(ctx context.Context,
	tenantID, stack, name string,
) ([]byte, *SecretMetadata, error) {
	cache := requestCacheFromContext(ctx)

	if cache != nil {
		if cached, meta, ok := cache.get(tenantID, stack, name); ok {
			// Return a fresh copy so the caller's zero is independent
			// of the cache's storage.
			out := make([]byte, len(cached))
			copy(out, cached)
			return out, meta, nil
		}
	}

	pt, meta, err := r.store.MaterializeSecretForOp(ctx, tenantID, stack, name)
	if err != nil {
		return nil, nil, err
	}

	if cache != nil {
		// Cache stores its own copy. Caller keeps `pt` to own/zero.
		cachedCopy := make([]byte, len(pt))
		copy(cachedCopy, pt)
		cache.set(tenantID, stack, name, cachedCopy, meta)
	}
	return pt, meta, nil
}

// MaterializeForOpSlug is the slug-keyed variant of MaterializeForOp.
// Resolves the slug to a tenant_id via the constructor-supplied
// SlugToID callback, then defers to MaterializeForOp. Used by the
// processor splice, which has the slug pinned on context but not
// the tenant_id.
//
// The slug→id resolution is NOT cached separately — it's a covered
// point read against the in-memory dbcache mirror; the per-request
// cache deduplicates the much-more-expensive Materialize call.
func (r *Resolver) MaterializeForOpSlug(ctx context.Context,
	tenantSlug, stack, name string,
) ([]byte, *SecretMetadata, error) {
	if r.slugToID == nil {
		return nil, nil, fmt.Errorf("secrets: resolver has no slug-to-id mapping (constructor was passed nil)")
	}
	if tenantSlug == "" {
		return nil, nil, fmt.Errorf("secrets: empty tenant slug")
	}
	tenantID, err := r.slugToID(ctx, tenantSlug)
	if err != nil {
		return nil, nil, fmt.Errorf("secrets: resolve tenant slug %q: %w", tenantSlug, err)
	}
	return r.MaterializeForOp(ctx, tenantID, stack, name)
}

// --- per-request cache ---

type requestCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	cleartext []byte
	meta      *SecretMetadata
}

func newRequestCache() *requestCache {
	return &requestCache{entries: map[string]cacheEntry{}}
}

func (c *requestCache) key(tenantID, stack, name string) string {
	return tenantID + "\x00" + stack + "\x00" + name
}

func (c *requestCache) get(tenantID, stack, name string) ([]byte, *SecretMetadata, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[c.key(tenantID, stack, name)]
	if !ok {
		return nil, nil, false
	}
	return e.cleartext, e.meta, true
}

func (c *requestCache) set(tenantID, stack, name string, cleartext []byte, meta *SecretMetadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[c.key(tenantID, stack, name)] = cacheEntry{cleartext: cleartext, meta: meta}
}

// zero wipes every cached cleartext and clears the map. Called from
// PR 3's processor splice via the per-request cleanup chain (same
// defer that fires SecretBag.Zero on every exit path).
func (c *requestCache) zero() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		Zero(e.cleartext)
		delete(c.entries, k)
	}
}

type ctxKey int

const ctxKeyRequestCache ctxKey = 1

// WithRequestCache returns a context carrying a fresh per-request
// cache. The returned cleanup zeroes every cached cleartext.
//
// Each call returns a NEW cache: callers should not pass the same
// cache across requests. Use EnsureRequestCache from the processor
// splice — that's the right idiom for "install at outermost Run,
// skip on recursive inner Runs that share the cache".
func WithRequestCache(ctx context.Context) (context.Context, func()) {
	c := newRequestCache()
	return context.WithValue(ctx, ctxKeyRequestCache, c), c.zero
}

// EnsureRequestCache installs a request cache on ctx if one isn't
// already present, returning the (possibly new) ctx and a cleanup
// closure that the caller defers.
//
// On the outermost call (no cache yet), this is equivalent to
// WithRequestCache. On a recursive call (cache already installed),
// it returns the ctx unchanged and a no-op cleanup. Net effect: the
// outermost Run owns the cache lifetime; inner Runs reuse the same
// cache for memoization across stage jumps without prematurely
// zeroing it.
func EnsureRequestCache(ctx context.Context) (context.Context, func()) {
	if requestCacheFromContext(ctx) != nil {
		return ctx, func() {}
	}
	return WithRequestCache(ctx)
}

func requestCacheFromContext(ctx context.Context) *requestCache {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(ctxKeyRequestCache).(*requestCache); ok {
		return v
	}
	return nil
}
