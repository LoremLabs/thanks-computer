package kv

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"
)

func newKV(t *testing.T, maxValue int, maxTTL time.Duration) *KV {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{path}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return New(s, maxValue, maxTTL)
}

func TestSetGetDelete(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)

	if err := k.Set(ctx, "t1", "s1", "k", json.RawMessage(`{"a":1}`), 0); err != nil {
		t.Fatal(err)
	}
	v, found, err := k.Get(ctx, "t1", "s1", "k")
	if err != nil || !found || string(v) != `{"a":1}` {
		t.Fatalf("get: v=%s found=%v err=%v", v, found, err)
	}
	if err := k.Delete(ctx, "t1", "s1", "k"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := k.Get(ctx, "t1", "s1", "k"); found {
		t.Fatal("expected miss after delete")
	}
	// deleting a missing key is a success.
	if err := k.Delete(ctx, "t1", "s1", "nope"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestPersistentDefault(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)
	base := time.Unix(1_000_000, 0)
	cur := base
	k.now = func() time.Time { return cur }

	if err := k.Set(ctx, "t1", "s1", "k", json.RawMessage(`"v"`), 0); err != nil {
		t.Fatal(err)
	}
	cur = base.Add(1000 * time.Hour) // far future: a persistent key never expires
	if _, found, _ := k.Get(ctx, "t1", "s1", "k"); !found {
		t.Fatal("persistent key must survive")
	}
}

func TestTTLExpiry(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)
	base := time.Unix(1_000_000, 0)
	cur := base
	k.now = func() time.Time { return cur }

	if err := k.Set(ctx, "t1", "s1", "tmp", json.RawMessage(`"x"`), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := k.Get(ctx, "t1", "s1", "tmp"); !found {
		t.Fatal("present before expiry")
	}
	cur = base.Add(11 * time.Second)
	if _, found, _ := k.Get(ctx, "t1", "s1", "tmp"); found {
		t.Fatal("must be expired")
	}
}

func TestMaxTTLClamp(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 5*time.Second) // ceiling 5s
	base := time.Unix(1_000_000, 0)
	cur := base
	k.now = func() time.Time { return cur }

	if err := k.Set(ctx, "t1", "s1", "k", json.RawMessage(`1`), 100*time.Second); err != nil {
		t.Fatal(err)
	}
	cur = base.Add(3 * time.Second)
	if _, found, _ := k.Get(ctx, "t1", "s1", "k"); !found {
		t.Fatal("within clamped ttl, must be present")
	}
	cur = base.Add(6 * time.Second)
	if _, found, _ := k.Get(ctx, "t1", "s1", "k"); found {
		t.Fatal("past clamped 5s ttl, must be expired")
	}
}

func TestIncr(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)

	if n, err := k.Incr(ctx, "t1", "s1", "c", 1, 0); err != nil || n != 1 {
		t.Fatalf("incr1: n=%d err=%v", n, err)
	}
	if n, err := k.Incr(ctx, "t1", "s1", "c", 2, 0); err != nil || n != 3 {
		t.Fatalf("incr2: n=%d err=%v", n, err)
	}
	if v, found, _ := k.Get(ctx, "t1", "s1", "c"); !found || string(v) != "3" {
		t.Fatalf("counter persisted wrong: v=%s found=%v", v, found)
	}
	// incrementing a non-integer value errors.
	if err := k.Set(ctx, "t1", "s1", "str", json.RawMessage(`"abc"`), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Incr(ctx, "t1", "s1", "str", 1, 0); err == nil {
		t.Fatal("incr on non-integer must error")
	}
}

func TestIncrExpiredResets(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)
	base := time.Unix(1_000_000, 0)
	cur := base
	k.now = func() time.Time { return cur }

	if n, err := k.Incr(ctx, "t1", "s1", "c", 5, 10*time.Second); err != nil || n != 5 {
		t.Fatalf("seed: n=%d err=%v", n, err)
	}
	cur = base.Add(11 * time.Second) // expired
	if n, err := k.Incr(ctx, "t1", "s1", "c", 1, 0); err != nil || n != 1 {
		t.Fatalf("expired counter must reset to delta: n=%d err=%v", n, err)
	}
}

func TestTenantNamespaceIsolation(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)
	_ = k.Set(ctx, "t1", "s1", "k", json.RawMessage(`1`), 0)
	_ = k.Set(ctx, "t1", "s2", "k", json.RawMessage(`2`), 0)
	_ = k.Set(ctx, "t2", "s1", "k", json.RawMessage(`3`), 0)

	for _, c := range []struct {
		tenant, ns, want string
	}{
		{"t1", "s1", "1"}, {"t1", "s2", "2"}, {"t2", "s1", "3"},
	} {
		v, found, _ := k.Get(ctx, c.tenant, c.ns, "k")
		if !found || string(v) != c.want {
			t.Fatalf("%s/%s: v=%s found=%v want %s", c.tenant, c.ns, v, found, c.want)
		}
	}
}

func TestValueSizeCap(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 4, 0) // 4-byte cap
	if err := k.Set(ctx, "t1", "s1", "k", json.RawMessage(`"12345"`), 0); err == nil {
		t.Fatal("oversized value must error")
	}
	if err := k.Set(ctx, "t1", "s1", "k", json.RawMessage(`1`), 0); err != nil {
		t.Fatalf("small value must succeed: %v", err)
	}
}

func TestInvalidSegments(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)
	if err := k.Set(ctx, "t1", "s1", "a/b", json.RawMessage(`1`), 0); err == nil {
		t.Fatal("slash in key must error")
	}
	if err := k.Set(ctx, "t1", "", "k", json.RawMessage(`1`), 0); err == nil {
		t.Fatal("empty namespace must error")
	}
	if _, _, err := k.Get(ctx, "t1", "s1", ""); err == nil {
		t.Fatal("empty key must error")
	}
	if err := k.Set(ctx, "t1", "s1", "k", json.RawMessage(`{bad`), 0); err == nil {
		t.Fatal("invalid JSON value must error")
	}
}
