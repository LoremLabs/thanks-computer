package redisstore

// Unit tests for the pure helpers run always. The integration tests need a
// real redis and are gated on TXCO_TEST_REDIS_ADDR (house pattern, like
// TXCO_TEST_PG_DSN for pgruntime): a bare host:port or a redis://|rediss://
// URL. Locally:
//
//	docker run --rm -p 6379:6379 redis:7
//	TXCO_TEST_REDIS_ADDR=localhost:6379 go test ./chassis/kv/redisstore/
//
// or point it at an Upstash DB (rediss://default:<token>@<db>.upstash.io:6379)
// to smoke the managed-redis path (SCAN COUNT respect, EVAL, RESP3).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kvtools/valkeyrie/store"
	goredis "github.com/redis/go-redis/v9"

	"github.com/loremlabs/thanks-computer/chassis/kv"
)

func TestScanPattern(t *testing.T) {
	if got := scanPattern("t1/ns/"); got != "t1/ns/*" {
		t.Fatalf("scanPattern = %q", got)
	}
}

func TestNormalize(t *testing.T) {
	if got := normalize("/t1/ns/k"); got != "t1/ns/k" {
		t.Fatalf("normalize = %q", got)
	}
	if got := normalize("t1/ns/k"); got != "t1/ns/k" {
		t.Fatalf("normalize = %q", got)
	}
}

func TestFormatSec(t *testing.T) {
	if got := formatSec(0); got != "0" {
		t.Fatalf("formatSec(0) = %q", got)
	}
	if got := formatSec(90 * time.Second); got != "90" {
		t.Fatalf("formatSec(90s) = %q", got)
	}
}

// newTestStore connects to TXCO_TEST_REDIS_ADDR (skipping the test when
// unset) and returns the store plus a unique key prefix for isolation on a
// shared test DB. Keys under the prefix are removed on cleanup.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	addr := os.Getenv("TXCO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TXCO_TEST_REDIS_ADDR not set; skipping redis integration test")
	}

	cfg := &Config{}
	if strings.HasPrefix(addr, "redis://") || strings.HasPrefix(addr, "rediss://") {
		opt, err := goredis.ParseURL(addr)
		if err != nil {
			t.Fatalf("parse TXCO_TEST_REDIS_ADDR: %v", err)
		}
		cfg.TLS = opt.TLSConfig
		cfg.Username = opt.Username
		cfg.Password = opt.Password
		cfg.DB = opt.DB
		addr = opt.Addr
	}

	s, err := New(context.Background(), []string{addr}, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	prefix := fmt.Sprintf("kvtest-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		if derr := s.DeleteTree(context.Background(), prefix); derr != nil && !errors.Is(derr, store.ErrKeyNotFound) {
			t.Logf("cleanup DeleteTree: %v", derr)
		}
		_ = s.Close()
	})
	return s, prefix
}

func TestPutGetExistsDelete(t *testing.T) {
	s, prefix := newTestStore(t)
	ctx := context.Background()
	key := prefix + "/ns/alpha"

	if _, err := s.Get(ctx, key, nil); !errors.Is(err, store.ErrKeyNotFound) {
		t.Fatalf("Get missing: want ErrKeyNotFound, got %v", err)
	}
	if err := s.Put(ctx, key, []byte(`{"v":1}`), nil); err != nil {
		t.Fatalf("Put: %v", err)
	}
	pair, err := s.Get(ctx, key, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Raw storage contract: bytes come back verbatim (no codec envelope) —
	// this is what keeps the store byte-compatible with data written by the
	// previous kvtools/redis RawCodec path.
	if string(pair.Value) != `{"v":1}` {
		t.Fatalf("Get value = %q", pair.Value)
	}
	ok, err := s.Exists(ctx, key, nil)
	if err != nil || !ok {
		t.Fatalf("Exists = %v, %v", ok, err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok, _ := s.Exists(ctx, key, nil); ok {
		t.Fatal("key still exists after Delete")
	}
}

func TestListPaginatesScanAndMGet(t *testing.T) {
	s, prefix := newTestStore(t)
	ctx := context.Background()

	// Force multi-cursor SCAN paging and multi-chunk MGET with few keys.
	oldScan, oldChunk := scanCount, mgetChunk
	scanCount, mgetChunk = 3, 2
	t.Cleanup(func() { scanCount, mgetChunk = oldScan, oldChunk })

	want := map[string]string{}
	for i := 0; i < 10; i++ {
		k := fmt.Sprintf("%s/ns/a%02d", prefix, i)
		v := fmt.Sprintf(`{"v":%d}`, i)
		want[k] = v
		if err := s.Put(ctx, k, []byte(v), nil); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	// Neighbor namespace must not leak into the listing.
	for i := 0; i < 3; i++ {
		k := fmt.Sprintf("%s/other/b%02d", prefix, i)
		if err := s.Put(ctx, k, []byte(`{"v":true}`), nil); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	pairs, err := s.List(ctx, prefix+"/ns/", nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pairs) != len(want) {
		t.Fatalf("List returned %d pairs, want %d", len(pairs), len(want))
	}
	for _, p := range pairs {
		if want[p.Key] != string(p.Value) {
			t.Fatalf("List pair %q = %q, want %q", p.Key, p.Value, want[p.Key])
		}
	}

	if _, err := s.List(ctx, prefix+"/empty/", nil); !errors.Is(err, store.ErrKeyNotFound) {
		t.Fatalf("List empty namespace: want ErrKeyNotFound, got %v", err)
	}
}

func TestAtomicPutAndDelete(t *testing.T) {
	s, prefix := newTestStore(t)
	ctx := context.Background()
	key := prefix + "/ns/cas"

	// Create (previous == nil).
	ok, created, err := s.AtomicPut(ctx, key, []byte(`"v1"`), nil, nil)
	if err != nil || !ok {
		t.Fatalf("AtomicPut create = %v, %v", ok, err)
	}
	// Duplicate create fails.
	if _, _, err := s.AtomicPut(ctx, key, []byte(`"v1b"`), nil, nil); !errors.Is(err, store.ErrKeyExists) {
		t.Fatalf("duplicate create: want ErrKeyExists, got %v", err)
	}
	// Swap with the correct previous value.
	ok, swapped, err := s.AtomicPut(ctx, key, []byte(`"v2"`), created, nil)
	if err != nil || !ok {
		t.Fatalf("AtomicPut swap = %v, %v", ok, err)
	}
	// Swap with a STALE previous must fail — this is the compare the
	// upstream LastIndex-based script never actually performed.
	if _, _, err := s.AtomicPut(ctx, key, []byte(`"v3"`), created, nil); !errors.Is(err, store.ErrKeyModified) {
		t.Fatalf("stale swap: want ErrKeyModified, got %v", err)
	}
	// CAS on a missing key reports ErrKeyNotFound.
	if _, _, err := s.AtomicPut(ctx, prefix+"/ns/ghost", []byte(`"x"`), created, nil); !errors.Is(err, store.ErrKeyNotFound) {
		t.Fatalf("cas on missing key: want ErrKeyNotFound, got %v", err)
	}
	// Stale AtomicDelete must fail; current one must delete.
	if _, err := s.AtomicDelete(ctx, key, created); !errors.Is(err, store.ErrKeyModified) {
		t.Fatalf("stale AtomicDelete: want ErrKeyModified, got %v", err)
	}
	if ok, err := s.AtomicDelete(ctx, key, swapped); err != nil || !ok {
		t.Fatalf("AtomicDelete = %v, %v", ok, err)
	}
	if ok, _ := s.Exists(ctx, key, nil); ok {
		t.Fatal("key still exists after AtomicDelete")
	}
}

func TestNativeTTLExpires(t *testing.T) {
	s, prefix := newTestStore(t)
	ctx := context.Background()
	key := prefix + "/ns/ttl"

	if err := s.Put(ctx, key, []byte(`"soon"`), &store.WriteOptions{TTL: time.Second}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ok, _ := s.Exists(ctx, key, nil); !ok {
		t.Fatal("key missing right after Put with TTL")
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := s.Get(ctx, key, nil); !errors.Is(err, store.ErrKeyNotFound) {
		t.Fatalf("after TTL: want ErrKeyNotFound, got %v", err)
	}
}

// TestWrapperIncrIsAtomic drives the chassis kv wrapper's Incr concurrently
// through this store. Every increment must land: with the upstream
// LastIndex-based lua compare (vacuously true on raw values) concurrent
// increments silently lost updates; the value-compare script makes losers
// get ErrKeyModified and retry. Contention errors from the wrapper's bounded
// retry are re-driven by the test so the expected total stays exact.
func TestWrapperIncrIsAtomic(t *testing.T) {
	s, prefix := newTestStore(t)
	k := kv.New(s, 0, 0)
	ctx := context.Background()

	const workers = 4
	const perWorker = 25

	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			done := 0
			for done < perWorker {
				_, err := k.Incr(ctx, prefix, "ns", "counter", 1, 0)
				if err != nil {
					if strings.Contains(err.Error(), "contention") {
						continue // bounded-retry exhaustion under load; try again
					}
					errCh <- err
					return
				}
				done++
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("Incr: %v", err)
	}

	val, found, err := k.Get(ctx, prefix, "ns", "counter")
	if err != nil || !found {
		t.Fatalf("Get counter = found=%v, %v", found, err)
	}
	if want := fmt.Sprintf("%d", workers*perWorker); string(val) != want {
		t.Fatalf("counter = %s, want %s (lost updates)", val, want)
	}
}

// TestWrapperRawFormat pins the on-wire format end-to-end: what the chassis
// kv wrapper stores must be its raw JSON envelope, exactly as the previous
// kvtools/redis RawCodec path wrote it — existing production data depends on
// this staying byte-identical.
func TestWrapperRawFormat(t *testing.T) {
	s, prefix := newTestStore(t)
	k := kv.New(s, 0, 0)
	ctx := context.Background()

	if err := k.Set(ctx, prefix, "ns", "fmt", []byte(`{"a":1}`), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	raw, err := s.client.Get(ctx, prefix+"/ns/fmt").Result()
	if err != nil {
		t.Fatalf("raw GET: %v", err)
	}
	if raw != `{"v":{"a":1}}` {
		t.Fatalf("stored bytes = %q, want the plain wrapper envelope", raw)
	}
}
