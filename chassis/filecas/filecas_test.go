package filecas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestShardKey(t *testing.T) {
	h := hashOf([]byte("hello")) // valid 64-char lowercase hex
	key, ok := ShardKey(h)
	if !ok {
		t.Fatalf("ShardKey(%s) ok=false", h)
	}
	want := "sha256/" + h[0:2] + "/" + h[2:4] + "/" + h
	if key != want {
		t.Fatalf("ShardKey=%q want %q", key, want)
	}
	for _, bad := range []string{"", "abc", h[:63], h + "0", "g" + h[1:]} {
		if _, ok := ShardKey(bad); ok {
			t.Fatalf("ShardKey(%q) ok=true, want false", bad)
		}
	}
}

func TestVerify(t *testing.T) {
	data := []byte("payload")
	if err := Verify(hashOf(data), data); err != nil {
		t.Fatalf("Verify matching: %v", err)
	}
	if err := Verify(hashOf([]byte("other")), data); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("Verify mismatch err=%v want ErrHashMismatch", err)
	}
}

// fakeBackend is an in-memory Store that records Get calls, for cache tests.
type fakeBackend struct {
	m        map[string][]byte
	getCalls int
}

func newFake() *fakeBackend { return &fakeBackend{m: map[string][]byte{}} }

func (f *fakeBackend) Name() string { return "fake" }
func (f *fakeBackend) Put(_ context.Context, hash string, data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	f.m[hash] = cp
	return nil
}
func (f *fakeBackend) Get(_ context.Context, hash string) ([]byte, error) {
	f.getCalls++
	b, ok := f.m[hash]
	if !ok {
		return nil, ErrNotFound
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}
func (f *fakeBackend) Exists(_ context.Context, hash string) (bool, error) {
	_, ok := f.m[hash]
	return ok, nil
}

func TestCachedCopyOnReturn(t *testing.T) {
	ctx := context.Background()
	fake := newFake()
	c := newCachedStore(fake, 1<<20, 0)
	if err := c.Put(ctx, "h1", []byte("hello")); err != nil {
		t.Fatal(err)
	}

	b1, err := c.Get(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	b1[0] = 'X' // mutate the returned slice; must not corrupt the cache

	b2, err := c.Get(ctx, "h1")
	if err != nil {
		t.Fatal(err)
	}
	if string(b2) != "hello" {
		t.Fatalf("cache corrupted by caller mutation: got %q want hello", b2)
	}
	// Both gets were cache hits (Put pre-warmed) → backend.Get never called.
	if fake.getCalls != 0 {
		t.Fatalf("backend.Get called %d times, want 0 (cache hits)", fake.getCalls)
	}
}

func TestCachedEviction(t *testing.T) {
	ctx := context.Background()
	fake := newFake()
	c := newCachedStore(fake, 10, 0)      // budget = 10 bytes (holds 2 of these)
	_ = c.Put(ctx, "h1", []byte("aaaaa")) // 5  → {h1}
	_ = c.Put(ctx, "h2", []byte("bbbbb")) // 5  → {h2,h1}, cur=10
	_ = c.Put(ctx, "h3", []byte("ccccc")) // 5  → cur=15 → evict LRU tail (h1); {h3,h2}

	// Check retained hits FIRST (a hit only bumps recency, no eviction); the
	// evicted miss goes LAST so re-admission can't cascade into the assertion.
	fake.getCalls = 0
	if _, err := c.Get(ctx, "h3"); err != nil { // cached hit
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "h2"); err != nil { // cached hit
		t.Fatal(err)
	}
	if fake.getCalls != 0 {
		t.Fatalf("retained keys caused %d backend.Get calls, want 0", fake.getCalls)
	}
	if _, err := c.Get(ctx, "h1"); err != nil { // evicted → backend miss
		t.Fatal(err)
	}
	if fake.getCalls != 1 {
		t.Fatalf("evicted key caused %d backend.Get calls, want 1", fake.getCalls)
	}
}

func TestCachedPerEntryGuard(t *testing.T) {
	ctx := context.Background()
	fake := newFake()
	c := newCachedStore(fake, 100, 4)           // per-entry guard = 4 bytes
	_ = c.Put(ctx, "big", []byte("0123456789")) // 10 > 4 → never cached

	fake.getCalls = 0
	if _, err := c.Get(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	if fake.getCalls != 2 {
		t.Fatalf("backend.Get called %d times, want 2 (oversize never cached)", fake.getCalls)
	}
}
