package filecas

import (
	"container/list"
	"context"
	"sync"
)

// cachedStore fronts a backend Store with a byte-bounded LRU keyed by
// content hash. Values are content-addressed (immutable), so there is no
// invalidation — only eviction under memory pressure.
//
// Memory safety: Go slices are mutable references, so the cache never hands
// out its backing array. Get returns a COPY; Put pre-warms with a COPY of
// the caller-owned input. A blob larger than maxEntry is served from the
// backend but never cached, so one large file can't evict everything.
type cachedStore struct {
	backend  Store
	maxBytes int64
	maxEntry int64 // per-entry cache guard; 0 = no guard

	mu       sync.Mutex
	ll       *list.List               // front = most-recently-used
	items    map[string]*list.Element // hash → element holding *cacheItem
	curBytes int64
}

type cacheItem struct {
	hash string
	data []byte // owned by the cache; never mutated, never exposed
}

func newCachedStore(backend Store, maxBytes, maxEntry int64) *cachedStore {
	return &cachedStore{
		backend:  backend,
		maxBytes: maxBytes,
		maxEntry: maxEntry,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
	}
}

func (c *cachedStore) Name() string { return c.backend.Name() }

// unwrap exposes the wrapped backend so the package-level streaming helpers
// (PutReader/GetReader/BlobPath) can discover its capabilities through this
// decorator. Streaming deliberately bypasses the byte LRU: blobs big enough
// to stream are exactly the ones the per-entry guard would refuse anyway.
func (c *cachedStore) unwrap() Store { return c.backend }

func (c *cachedStore) Exists(ctx context.Context, hash string) (bool, error) {
	c.mu.Lock()
	_, ok := c.items[hash]
	c.mu.Unlock()
	if ok {
		return true, nil
	}
	return c.backend.Exists(ctx, hash)
}

func (c *cachedStore) Get(ctx context.Context, hash string) ([]byte, error) {
	if b, ok := c.lookup(hash); ok {
		return b, nil
	}
	data, err := c.backend.Get(ctx, hash)
	if err != nil {
		return nil, err
	}
	// backend.Get returns a freshly-allocated, caller-owned slice; the
	// cache may take it directly, but the caller gets a copy so it can
	// never mutate the cached buffer.
	c.admit(hash, data)
	return cloneBytes(data), nil
}

func (c *cachedStore) Put(ctx context.Context, hash string, data []byte) error {
	if err := c.backend.Put(ctx, hash, data); err != nil {
		return err
	}
	// The caller owns `data` and may mutate it after Put returns — admit a
	// copy so the cache holds a stable, private buffer.
	c.admit(hash, cloneBytes(data))
	return nil
}

// lookup returns a COPY of the cached bytes and bumps recency.
func (c *cachedStore) lookup(hash string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[hash]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return cloneBytes(el.Value.(*cacheItem).data), true
}

// admit caches owned (which the cache now owns and must never be mutated by
// the caller), evicting the LRU tail until within budget. Oversize blobs
// are not cached.
func (c *cachedStore) admit(hash string, owned []byte) {
	n := int64(len(owned))
	if c.maxEntry > 0 && n > c.maxEntry {
		return
	}
	if n > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[hash]; ok {
		c.ll.MoveToFront(el) // already present; refresh recency, keep bytes
		return
	}
	el := c.ll.PushFront(&cacheItem{hash: hash, data: owned})
	c.items[hash] = el
	c.curBytes += n
	for c.curBytes > c.maxBytes {
		tail := c.ll.Back()
		if tail == nil {
			break
		}
		it := tail.Value.(*cacheItem)
		c.ll.Remove(tail)
		delete(c.items, it.hash)
		c.curBytes -= int64(len(it.data))
	}
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

var _ Store = (*cachedStore)(nil)
