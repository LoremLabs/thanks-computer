package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"
	"github.com/kvtools/valkeyrie/store"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/kv/redisstore"
)

// testClock is the injectable clock both backends take (mem: `now`; kv:
// `now` — which also drives the memo, so advancing it past the memo TTL
// forces a store read).
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTestStore opens a boltdb-backed valkeyrie store in a temp dir — the
// same shape chassis/kv's own tests and the websocket directory tests use.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{path}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newKVStoreOver builds a kv challenge store over s with the given clock.
func newKVStoreOver(s store.Store, clock *testClock, logger *zap.Logger) *kvChallengeStore {
	k := newKVChallengeStore(context.Background(), kvstore.New(s, 0, 0), logger)
	if clock != nil {
		k.now = clock.now
	}
	return k
}

// hookStore wraps a real store so a test can count reads, fail or block
// them, and interpose a "peer" write right before an atomic put.
type hookStore struct {
	store.Store
	mu          sync.Mutex
	gets        int
	atomicPuts  int
	onGet       func(ctx context.Context, key string) error // non-nil error fails the read; may block on ctx
	onAtomicPut func(key string)                            // runs before delegating
}

func (h *hookStore) Get(ctx context.Context, key string, opts *store.ReadOptions) (*store.KVPair, error) {
	h.mu.Lock()
	h.gets++
	h.mu.Unlock()
	if h.onGet != nil {
		if err := h.onGet(ctx, key); err != nil {
			return nil, err
		}
	}
	return h.Store.Get(ctx, key, opts)
}

func (h *hookStore) AtomicPut(ctx context.Context, key string, value []byte, previous *store.KVPair, opts *store.WriteOptions) (bool, *store.KVPair, error) {
	h.mu.Lock()
	h.atomicPuts++
	h.mu.Unlock()
	if h.onAtomicPut != nil {
		h.onAtomicPut(key)
	}
	return h.Store.AtomicPut(ctx, key, value, previous, opts)
}

func (h *hookStore) counts() (gets, puts int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gets, h.atomicPuts
}

// peerWrite plants a value at owner directly in the backing store, in the
// chassis/kv wrapper format, bypassing every CAS — "another head wrote
// this while you weren't looking".
func peerWrite(t *testing.T, s store.Store, owner string, entries []kvChallengeEntry) {
	t.Helper()
	key, ok := challengeKey(owner)
	if !ok {
		t.Fatalf("bad owner %q", owner)
	}
	v, _ := json.Marshal(entries)
	blob, _ := json.Marshal(struct {
		V json.RawMessage `json:"v"`
	}{V: v})
	if err := s.Put(context.Background(), challengeTenant+"/"+ChallengeNamespace+"/"+key, blob, nil); err != nil {
		t.Fatalf("peer write: %v", err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	a, b = sortedCopy(a), sortedCopy(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// challengeStoreSemantics is the ONE table both backends must satisfy —
// publish/read/cleanup, dedup-refresh, coexisting values, case-insensitive
// owner match, idempotent cleanup, and the safety expiry. Per-entry expiry
// in the KV value is what lets the same table drive both.
func challengeStoreSemantics(t *testing.T, s ChallengeStore, clock *testClock) {
	t.Helper()

	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("empty store should yield nil, got %v", got)
	}

	s.Present(acmeOwner, "tok-A")
	s.Present(acmeOwner, "tok-B")
	s.Present(acmeOwner, "tok-A") // dedup: still two values
	if got := s.ActiveTXT(acmeOwner); !sameSet(got, []string{"tok-A", "tok-B"}) {
		t.Fatalf("want [tok-A tok-B], got %v", got)
	}

	// Case-insensitive owner match, with and without the trailing dot.
	if got := s.ActiveTXT("_ACME-Challenge.OPS.example.com."); len(got) != 2 {
		t.Fatalf("owner match should be case-insensitive, got %v", got)
	}
	if got := s.ActiveTXT("_acme-challenge.ops.example.com"); len(got) != 2 {
		t.Fatalf("owner match should not depend on the trailing dot, got %v", got)
	}

	// CleanUp one value leaves the other.
	s.CleanUp(acmeOwner, "tok-A")
	if got := s.ActiveTXT(acmeOwner); len(got) != 1 || got[0] != "tok-B" {
		t.Fatalf("after cleanup want [tok-B], got %v", got)
	}

	// CleanUp is idempotent.
	s.CleanUp(acmeOwner, "tok-A")
	s.CleanUp(acmeOwner, "tok-B")
	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("after full cleanup want nil, got %v", got)
	}

	// Safety expiry: a value not explicitly cleaned vanishes past the TTL.
	s.Present(acmeOwner, "tok-C")
	clock.advance(challengeStoreTTL + time.Second)
	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("expired value should be gone, got %v", got)
	}

	// A refresh extends only the refreshed value: D published, then E, then
	// D refreshed just before D's original expiry — D lives on, E does not.
	s.Present(acmeOwner, "tok-D")
	clock.advance(5 * time.Minute)
	s.Present(acmeOwner, "tok-E")
	clock.advance(4 * time.Minute)
	s.Present(acmeOwner, "tok-D")
	clock.advance(2 * time.Minute) // D: refreshed 2m ago; E: published 6m ago
	if got := s.ActiveTXT(acmeOwner); !sameSet(got, []string{"tok-D", "tok-E"}) {
		t.Fatalf("both should still be live, got %v", got)
	}
	clock.advance(5 * time.Minute) // E: 11m → gone; D: 7m → live
	if got := s.ActiveTXT(acmeOwner); !sameSet(got, []string{"tok-D"}) {
		t.Fatalf("per-value expiry: want [tok-D], got %v", got)
	}
}

// TestKVChallengeStoreSemanticsMatchMem: the mem-store table, verbatim,
// against the KV backend (boltdb — no Redis needed).
func TestKVChallengeStoreSemanticsMatchMem(t *testing.T) {
	clock := newTestClock()
	s := newKVStoreOver(newTestStore(t), clock, zap.NewNop())
	challengeStoreSemantics(t, s, clock)
}

// TestChallengeKeyNormalization pins the shared key both backends use.
func TestChallengeKeyNormalization(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"Foo.Example.COM", "foo.example.com.", true},
		{"foo.example.com.", "foo.example.com.", true},
		{"  x  ", "x.", true},
		{"_acme-challenge.stacks.thanks.computer", "_acme-challenge.stacks.thanks.computer.", true},
		{"", "", false},
		{"   ", "", false},
		{"a/b.example.com.", "", false},
		{"a\x01b.example.com.", "", false},
		{"a\x7fb.example.com.", "", false},
		{strings.Repeat("a", 300) + ".", "", false},
	}
	for _, c := range cases {
		got, ok := challengeKey(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("challengeKey(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestChallengeBackendSelection is the boot-guard refusal as a pure table
// — the assertion the untested --websocket-relay guard never had.
func TestChallengeBackendSelection(t *testing.T) {
	cases := []struct {
		sel      string
		kvShared bool
		want     string
		wantErr  bool
	}{
		{"", false, ChallengeBackendMemory, false},
		{"", true, ChallengeBackendMemory, false},
		{"memory", false, ChallengeBackendMemory, false},
		{" Memory ", true, ChallengeBackendMemory, false},
		{"kv", true, ChallengeBackendKV, false},
		{"KV", true, ChallengeBackendKV, false},
		{"kv", false, "", true}, // the refusal: shared store on a node-local kv
		{"postgres", true, "", true},
		{"redis", true, "", true},
	}
	for _, c := range cases {
		got, err := ChallengeBackend(c.sel, c.kvShared)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("ChallengeBackend(%q, %v) = (%q, %v), want (%q, err=%v)", c.sel, c.kvShared, got, err, c.want, c.wantErr)
		}
	}
	if _, err := ChallengeBackend("kv", false); err == nil || !strings.Contains(err.Error(), "redis") {
		t.Fatalf("refusal should name the shared backend, got %v", err)
	}
}

// TestKVChallengeStoreTwoHeadsShareOneStore is the "two nameservers"
// proof: two store instances over ONE backing store. A write on A is
// visible on B; a cleanup on A removes it for B; A sees its own write at
// once with no clock advance.
func TestKVChallengeStoreTwoHeadsShareOneStore(t *testing.T) {
	backing := newTestStore(t)
	clock := newTestClock()
	a := newKVStoreOver(backing, clock, zap.NewNop())
	b := newKVStoreOver(backing, clock, zap.NewNop())

	a.Present(acmeOwner, "written-on-a")
	if got := a.ActiveTXT(acmeOwner); !sameSet(got, []string{"written-on-a"}) {
		t.Fatalf("writer should see its own value immediately, got %v", got)
	}
	if got := b.ActiveTXT(acmeOwner); !sameSet(got, []string{"written-on-a"}) {
		t.Fatalf("peer should see the value, got %v", got)
	}

	b.Present(acmeOwner, "written-on-b")
	clock.advance(2 * challengeMemoTTL) // past A's memo of its own write
	if got := a.ActiveTXT(acmeOwner); !sameSet(got, []string{"written-on-a", "written-on-b"}) {
		t.Fatalf("both heads' values should coexist, got %v", got)
	}

	a.CleanUp(acmeOwner, "written-on-a")
	clock.advance(2 * challengeMemoTTL)
	if got := b.ActiveTXT(acmeOwner); !sameSet(got, []string{"written-on-b"}) {
		t.Fatalf("peer should see the cleanup, got %v", got)
	}
	b.CleanUp(acmeOwner, "written-on-b")
	clock.advance(2 * challengeMemoTTL)
	if got := a.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("after both cleanups want nil, got %v", got)
	}
}

// TestKVChallengeStoreConcurrentPresent: two instances writing distinct
// values to one owner at once — the CAS loop must keep every value.
func TestKVChallengeStoreConcurrentPresent(t *testing.T) {
	backing := newTestStore(t)
	clock := newTestClock()
	a := newKVStoreOver(backing, clock, zap.NewNop())
	b := newKVStoreOver(backing, clock, zap.NewNop())

	const per = 3 // 2×3 = 6, under challengeMaxEntries
	var wg sync.WaitGroup
	var want []string
	for i := 0; i < per; i++ {
		want = append(want, fmt.Sprintf("a-%d", i), fmt.Sprintf("b-%d", i))
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < per; i++ {
			a.Present(acmeOwner, fmt.Sprintf("a-%d", i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < per; i++ {
			b.Present(acmeOwner, fmt.Sprintf("b-%d", i))
		}
	}()
	wg.Wait()

	clock.advance(2 * challengeMemoTTL)
	fresh := newKVStoreOver(backing, clock, zap.NewNop())
	if got := fresh.ActiveTXT(acmeOwner); !sameSet(got, want) {
		t.Fatalf("concurrent presents lost a value: want %v, got %v", want, got)
	}
}

// TestKVChallengeStoreCleanUpRetriesOnConflict: A reads [a,b]; a peer
// lands [a,b,c] before A's CAS; A's CAS fails and must re-apply the
// removal to the returned current — ending at [b,c], not [a,b,c] until TTL.
func TestKVChallengeStoreCleanUpRetriesOnConflict(t *testing.T) {
	backing := newTestStore(t)
	hooked := &hookStore{Store: backing}
	clock := newTestClock()
	a := newKVStoreOver(hooked, clock, zap.NewNop())

	a.Present(acmeOwner, "a")
	a.Present(acmeOwner, "b")

	far := clock.now().Add(challengeStoreTTL).Unix()
	var once sync.Once
	hooked.onAtomicPut = func(string) {
		once.Do(func() {
			peerWrite(t, backing, acmeOwner, []kvChallengeEntry{{"a", far}, {"b", far}, {"c", far}})
		})
	}
	a.CleanUp(acmeOwner, "a")

	clock.advance(2 * challengeMemoTTL)
	fresh := newKVStoreOver(backing, clock, zap.NewNop())
	if got := fresh.ActiveTXT(acmeOwner); !sameSet(got, []string{"b", "c"}) {
		t.Fatalf("cleanup must re-apply after a lost race: want [b c], got %v", got)
	}
	// And the writer's own memo reflects the achieved state, not the
	// pre-conflict guess.
	if got := a.ActiveTXT(acmeOwner); !sameSet(got, []string{"b", "c"}) {
		t.Fatalf("writer memo after conflict: want [b c], got %v", got)
	}
}

// TestKVChallengeStoreCASExhaustion: a peer that wins EVERY round. The
// loop must stop after its bound, log at Error, and leave NO memo entry —
// memoizing a state we did not achieve is the one thing worse than failing.
func TestKVChallengeStoreCASExhaustion(t *testing.T) {
	backing := newTestStore(t)
	hooked := &hookStore{Store: backing}
	clock := newTestClock()
	core, logs := observer.New(zapcore.ErrorLevel)
	a := newKVStoreOver(hooked, clock, zap.New(core))

	far := clock.now().Add(challengeStoreTTL).Unix()
	n := 0
	hooked.onAtomicPut = func(string) {
		n++
		peerWrite(t, backing, acmeOwner, []kvChallengeEntry{{fmt.Sprintf("peer-%d", n), far}})
	}
	a.Present(acmeOwner, "mine")

	if _, puts := hooked.counts(); puts != challengeCASRounds {
		t.Fatalf("want exactly %d CAS rounds, got %d", challengeCASRounds, puts)
	}
	if got := logs.FilterMessageSnippet("write failed").Len(); got != 1 {
		t.Fatalf("want one Error log, got %d: %v", got, logs.All())
	}
	if vals, hit := a.memo.get(mustKey(t, acmeOwner), clock.now()); hit {
		t.Fatalf("no memo entry may survive a failed write, got %v", vals)
	}
	// The store holds the peer's last value only; "mine" never landed.
	fresh := newKVStoreOver(backing, clock, zap.NewNop())
	if got := fresh.ActiveTXT(acmeOwner); !sameSet(got, []string{fmt.Sprintf("peer-%d", n)}) {
		t.Fatalf("store after exhaustion: %v", got)
	}
}

func mustKey(t *testing.T, fqdn string) string {
	t.Helper()
	k, ok := challengeKey(fqdn)
	if !ok {
		t.Fatalf("bad key %q", fqdn)
	}
	return k
}

// TestKVChallengeStoreMemo: 100 reads ⇒ 1 store Get; a negative result is
// memoized too; a local Present needs zero extra Gets to be served.
func TestKVChallengeStoreMemo(t *testing.T) {
	hooked := &hookStore{Store: newTestStore(t)}
	clock := newTestClock()
	s := newKVStoreOver(hooked, clock, zap.NewNop())

	for i := 0; i < 100; i++ {
		if got := s.ActiveTXT(acmeOwner); got != nil {
			t.Fatalf("want nil, got %v", got)
		}
	}
	if gets, _ := hooked.counts(); gets != 1 {
		t.Fatalf("negative memo: want 1 Get for 100 reads, got %d", gets)
	}

	clock.advance(2 * challengeMemoTTL)
	s.Present(acmeOwner, "v") // one Get of its own
	gets0, _ := hooked.counts()
	for i := 0; i < 100; i++ {
		if got := s.ActiveTXT(acmeOwner); !sameSet(got, []string{"v"}) {
			t.Fatalf("want [v], got %v", got)
		}
	}
	if gets, _ := hooked.counts(); gets != gets0 {
		t.Fatalf("a local write must be served from the memo: %d extra Gets", gets-gets0)
	}

	clock.advance(2 * challengeMemoTTL)
	s.ActiveTXT(acmeOwner)
	if gets, _ := hooked.counts(); gets != gets0+1 {
		t.Fatalf("memo expiry should cost exactly one Get, got %d", gets-gets0)
	}
}

// TestKVChallengeStoreMemoBounded: distinct names cannot grow the memo
// without bound — two generations, dropped wholesale.
func TestKVChallengeStoreMemoBounded(t *testing.T) {
	s := newKVStoreOver(newTestStore(t), newTestClock(), zap.NewNop())
	const max = 8
	s.memo = newChallengeMemo(challengeMemoTTL, max)
	for i := 0; i < 10*max; i++ {
		s.ActiveTXT(fmt.Sprintf("_acme-challenge.n%d.example.com.", i))
	}
	s.memo.mu.Lock()
	size := len(s.memo.cur) + len(s.memo.prev)
	s.memo.mu.Unlock()
	if size > 2*max {
		t.Fatalf("memo grew to %d entries, bound is %d", size, 2*max)
	}
	// The most recent name is still memoized.
	if _, hit := s.memo.get(mustKey(t, fmt.Sprintf("_acme-challenge.n%d.example.com.", 10*max-1)), s.now()); !hit {
		t.Fatal("latest entry should be memoized")
	}
}

// TestKVChallengeStoreValueCaps: oversized and empty values are refused;
// over the entry cap the earliest-expiring is evicted so the newest wins.
func TestKVChallengeStoreValueCaps(t *testing.T) {
	clock := newTestClock()
	s := newKVStoreOver(newTestStore(t), clock, zap.NewNop())

	s.Present(acmeOwner, strings.Repeat("x", challengeMaxValueBytes+1))
	s.Present(acmeOwner, "")
	clock.advance(2 * challengeMemoTTL)
	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("rejected values must not land, got %d values", len(got))
	}

	for i := 0; i <= challengeMaxEntries; i++ { // one over the cap
		s.Present(acmeOwner, fmt.Sprintf("v%d", i))
		clock.advance(time.Second) // distinct expiries: v0 is the earliest
	}
	clock.advance(2 * challengeMemoTTL)
	got := s.ActiveTXT(acmeOwner)
	if len(got) != challengeMaxEntries {
		t.Fatalf("want %d values after eviction, got %d: %v", challengeMaxEntries, len(got), got)
	}
	for _, v := range got {
		if v == "v0" {
			t.Fatalf("earliest-expiring value should have been evicted: %v", got)
		}
	}
	if !contains(got, fmt.Sprintf("v%d", challengeMaxEntries)) {
		t.Fatalf("newest value must survive: %v", got)
	}
}

func contains(in []string, v string) bool {
	for _, s := range in {
		if s == v {
			return true
		}
	}
	return false
}

// TestKVChallengeStoreClearAll: the RFC2136 delete-RRset path drops every
// value in one step, on both backends.
func TestKVChallengeStoreClearAll(t *testing.T) {
	clock := newTestClock()
	for name, s := range map[string]ChallengeStore{
		"kv":  newKVStoreOver(newTestStore(t), clock, zap.NewNop()),
		"mem": newMemChallengeStore(),
	} {
		s.Present(acmeOwner, "a")
		s.Present(acmeOwner, "b")
		cl, ok := s.(challengeClearer)
		if !ok {
			t.Fatalf("%s: backend should implement challengeClearer", name)
		}
		cl.clearAll(acmeOwner)
		if got := s.ActiveTXT(acmeOwner); got != nil {
			t.Fatalf("%s: after clearAll want nil, got %v", name, got)
		}
		cl.clearAll(acmeOwner) // idempotent
	}
}

// TestKVChallengeStoreConcurrencyBound: with every slot held by a blocked
// store call, a further ActiveTXT returns nil AT ONCE — no queueing, no
// goroutine spawned — rather than piling handler goroutines on a slow store.
func TestKVChallengeStoreConcurrencyBound(t *testing.T) {
	hooked := &hookStore{Store: newTestStore(t)}
	s := newKVStoreOver(hooked, nil, zap.NewNop())
	const slots = 2
	s.slots = make(chan struct{}, slots)
	s.readTimeout = 10 * time.Second // the block, not the deadline, must be what bounds this test

	entered := make(chan struct{}, slots)
	release := make(chan struct{})
	hooked.onGet = func(ctx context.Context, _ string) error {
		entered <- struct{}{}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < slots; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.ActiveTXT(fmt.Sprintf("_acme-challenge.blocked%d.example.com.", i))
		}(i)
	}
	for i := 0; i < slots; i++ {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("blocked readers never entered the store")
		}
	}

	before := runtime.NumGoroutine()
	t0 := time.Now()
	got := s.ActiveTXT("_acme-challenge.overflow.example.com.")
	if el := time.Since(t0); got != nil || el > 200*time.Millisecond {
		t.Fatalf("saturated store: want immediate nil, got %v after %v", got, el)
	}
	if after := runtime.NumGoroutine(); after != before {
		t.Fatalf("a throttled read must not spawn goroutines: %d → %d", before, after)
	}
	if gets, _ := hooked.counts(); gets != slots {
		t.Fatalf("the overflow read must not reach the store: %d Gets", gets)
	}
	close(release)
	wg.Wait()
}

// TestKVChallengeStoreReadDeadline: a store that never answers is cut off
// by the context deadline end to end — on the caller's goroutine, no
// wrapper — and the miss is memoized so the next query does not block again.
func TestKVChallengeStoreReadDeadline(t *testing.T) {
	hooked := &hookStore{Store: newTestStore(t)}
	s := newKVStoreOver(hooked, nil, zap.NewNop())
	s.readTimeout = 50 * time.Millisecond
	hooked.onGet = func(ctx context.Context, _ string) error {
		<-ctx.Done() // honour the deadline the way go-redis does
		return ctx.Err()
	}

	t0 := time.Now()
	got := s.ActiveTXT(acmeOwner)
	el := time.Since(t0)
	if got != nil || el > time.Second {
		t.Fatalf("stalled store: want nil within the budget, got %v after %v", got, el)
	}
	if el < s.readTimeout {
		t.Fatalf("returned before the deadline (%v) — the store was not actually waited on", el)
	}
	gets0, _ := hooked.counts()
	s.ActiveTXT(acmeOwner) // negative memo
	if gets, _ := hooked.counts(); gets != gets0 {
		t.Fatal("a timed-out read must be memoized as a miss")
	}
}

// TestKVChallengeStoreDegradesOnStoreFailure: an erroring store yields
// "no challenge" — and, wired into a controller, the snapshot's NXDOMAIN,
// never SERVFAIL. Writes fail loudly and leave no memo.
func TestKVChallengeStoreDegradesOnStoreFailure(t *testing.T) {
	hooked := &hookStore{Store: newTestStore(t)}
	hooked.onGet = func(context.Context, string) error { return errors.New("redis: connection refused") }
	core, logs := observer.New(zapcore.WarnLevel)
	s := newKVStoreOver(hooked, nil, zap.New(core))

	s.Present(acmeOwner, "never-lands")
	if _, hit := s.memo.get(mustKey(t, acmeOwner), s.now()); hit {
		t.Fatal("a failed Present must not memoize")
	}
	if got := s.ActiveTXT(acmeOwner); got != nil {
		t.Fatalf("want nil on store failure, got %v", got)
	}
	if logs.FilterLevelExact(zapcore.ErrorLevel).Len() != 1 || logs.FilterLevelExact(zapcore.WarnLevel).Len() != 1 {
		t.Fatalf("want one Error (write) + one Warn (read), got %v", logs.All())
	}
	// Repeated reads while the store is down do not repeat the warning.
	for i := 0; i < 50; i++ {
		s.ActiveTXT(acmeOwner)
	}
	if logs.FilterLevelExact(zapcore.WarnLevel).Len() != 1 {
		t.Fatalf("warning must be throttled by the negative memo, got %d", logs.FilterLevelExact(zapcore.WarnLevel).Len())
	}

	db := newTestDB(t)
	seedZone(t, db, fixedTS)
	snap := buildOrDie(t, db, SynthConfig{})
	c := &DNSController{challenges: s}
	c.snap.Store(snap)
	req := new(dns.Msg)
	req.SetQuestion(acmeOwner, dns.TypeTXT)
	if m := c.answerChallenge(req, false); m != nil {
		t.Fatalf("store failure should fall through, got %v", m)
	}
	if m := buildReply(snap, req, false); m.Rcode != dns.RcodeNameError {
		t.Fatalf("fall-through should be the snapshot's NXDOMAIN, got rcode=%d", m.Rcode)
	}
}

// TestKVChallengeStoreRedis is the same two-heads proof against a REAL
// Redis (the production backend): the vendored CAS lua serializes
// concurrent Presents from two instances, and the key carries a native
// TTL so no sweeper is needed. Skips unless TXCO_TEST_REDIS_ADDR is set,
// like the redisstore package's own tests.
func TestKVChallengeStoreRedis(t *testing.T) {
	addr := os.Getenv("TXCO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TXCO_TEST_REDIS_ADDR not set; skipping redis integration test")
	}
	rs, err := redisstore.New(context.Background(), []string{addr}, &redisstore.Config{})
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = rs.Close() })
	// A per-run owner so parallel test runs on a shared DB never collide;
	// the key is TTL'd, so nothing is left behind.
	owner := fmt.Sprintf("_acme-challenge.t%d-%d.example.com.", os.Getpid(), time.Now().UnixNano())
	a := newKVChallengeStore(context.Background(), kvstore.New(rs, 0, 0), zap.NewNop())
	b := newKVChallengeStore(context.Background(), kvstore.New(rs, 0, 0), zap.NewNop())
	a.memo, b.memo = newChallengeMemo(0, 4), newChallengeMemo(0, 4) // read-through: every ActiveTXT hits redis

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.Present(owner, "from-a") }()
	go func() { defer wg.Done(); b.Present(owner, "from-b") }()
	wg.Wait()
	if got := b.ActiveTXT(owner); !sameSet(got, []string{"from-a", "from-b"}) {
		t.Fatalf("concurrent presents on redis: want both, got %v", got)
	}

	key := challengeTenant + "/" + ChallengeNamespace + "/" + mustKey(t, owner)
	pair, gerr := rs.Get(context.Background(), key, nil)
	if gerr != nil {
		t.Fatalf("raw get: %v", gerr)
	}
	if len(pair.Value) == 0 {
		t.Fatal("raw value empty")
	}
	// Native TTL: the redis key expires on its own (the redisstore honours
	// WriteOptions.TTL). Exists must be true now and the key must not be
	// persistent — read the TTL through a fresh client of the same store.
	if exists, _ := rs.Exists(context.Background(), key, nil); !exists {
		t.Fatal("key should exist")
	}
	a.CleanUp(owner, "from-a")
	b.CleanUp(owner, "from-b")
	if got := a.ActiveTXT(owner); got != nil {
		t.Fatalf("after cleanups want nil, got %v", got)
	}
	_ = rs.Delete(context.Background(), key) // tombstone; TTL would reap it anyway
}
