package kv

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
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

func TestListKeysPage(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)

	// Empty namespace → empty page, no cursor.
	keys, next, err := k.ListKeysPage(ctx, "t1", "subs", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 || next != "" {
		t.Fatalf("empty ns: keys=%v next=%q", keys, next)
	}

	want := []string{"a@x", "b@x", "c@x", "d@x", "e@x"}
	// Insert out of order to prove the page sorts.
	for _, kk := range []string{"c@x", "a@x", "e@x", "b@x", "d@x"} {
		if err := k.Set(ctx, "t1", "subs", kk, json.RawMessage(`{"v":1}`), 0); err != nil {
			t.Fatal(err)
		}
	}

	// Windowed: 2 per page, follow the cursor — each key once, sorted, 3 pages (2+2+1).
	var got []string
	cursor := ""
	pages := 0
	for {
		page, nxt, err := k.ListKeysPage(ctx, "t1", "subs", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 2 {
			t.Fatalf("page over limit: %v", page)
		}
		got = append(got, page...)
		cursor = nxt
		pages++
		if cursor == "" {
			break
		}
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("collected %v, want %v", got, want)
	}
	if pages != 3 {
		t.Fatalf("expected 3 pages, got %d", pages)
	}

	// A limit above the max is clamped (not an error): one call returns all 5.
	all, next2, err := k.ListKeysPage(ctx, "t1", "subs", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all, want) || next2 != "" {
		t.Fatalf("clamped page: keys=%v next=%q", all, next2)
	}

	// A cursor at/after the last key yields an empty final page.
	tail, next3, err := k.ListKeysPage(ctx, "t1", "subs", "e@x", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 0 || next3 != "" {
		t.Fatalf("past-end: keys=%v next=%q", tail, next3)
	}

	// A different namespace is isolated (namespace IS the prefix).
	other, _, err := k.ListKeysPage(ctx, "t1", "elsewhere", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("namespace bleed: %v", other)
	}
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

func TestIncrDecrements(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)
	if n, err := k.Incr(ctx, "t1", "s1", "c", 5, 0); err != nil || n != 5 {
		t.Fatalf("seed: n=%d err=%v", n, err)
	}
	if n, err := k.Incr(ctx, "t1", "s1", "c", -2, 0); err != nil || n != 3 {
		t.Fatalf("decrement by -2: n=%d want 3", n)
	}
	if n, err := k.Incr(ctx, "t1", "s1", "c", -10, 0); err != nil || n != -7 {
		t.Fatalf("decrement past zero: n=%d want -7", n)
	}
}

func TestCAS(t *testing.T) {
	ctx := context.Background()
	k := newKV(t, 0, 0)

	// set-if-absent (expectAbsent): first creates, second refuses.
	if sw, _, err := k.CAS(ctx, "t1", "s1", "lock", true, nil, json.RawMessage(`"held"`), 0); err != nil || !sw {
		t.Fatalf("create-if-absent should swap: sw=%v err=%v", sw, err)
	}
	if sw, cur, _ := k.CAS(ctx, "t1", "s1", "lock", true, nil, json.RawMessage(`"again"`), 0); sw || string(cur) != `"held"` {
		t.Fatalf("second create-if-absent must fail and report current: sw=%v cur=%s", sw, cur)
	}

	// value-match: correct `expected` swaps.
	if sw, _, err := k.CAS(ctx, "t1", "s1", "lock", false, json.RawMessage(`"held"`), json.RawMessage(`"taken"`), 0); err != nil || !sw {
		t.Fatalf("value-match should swap: sw=%v err=%v", sw, err)
	}
	if v, _, _ := k.Get(ctx, "t1", "s1", "lock"); string(v) != `"taken"` {
		t.Fatalf("after swap value=%s want \"taken\"", v)
	}

	// value-match: stale `expected` does NOT swap and returns the real current.
	if sw, cur, _ := k.CAS(ctx, "t1", "s1", "lock", false, json.RawMessage(`"held"`), json.RawMessage(`"nope"`), 0); sw || string(cur) != `"taken"` {
		t.Fatalf("stale expected must not swap: sw=%v cur=%s", sw, cur)
	}

	// value-match on an absent key can't match → no swap.
	if sw, _, _ := k.CAS(ctx, "t1", "s1", "ghost", false, json.RawMessage(`"x"`), json.RawMessage(`"y"`), 0); sw {
		t.Fatal("value-mode CAS on an absent key must not swap")
	}
}
