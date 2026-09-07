package dns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/kv/redisstore"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// Shared ACME challenge store over the chassis KV.
//
// Why it exists: the `*.stacks` wildcard (and every delegated zone's
// certificate) is issued by ACME DNS-01 against our OWN authoritative DNS.
// The CA may query any nameserver in the NS set, so once there are two
// heads the `_acme-challenge` TXT has to be visible from both — and the
// in-memory store (challenge.go) is per-process. This backend keeps the
// same records in the shared --kvstore (redis in a fleet), so a value
// written on one head (by the bundled solver or the RFC2136 receiver) is
// served by every head.
//
// What it does NOT do: it does not coordinate certificate ISSUANCE across
// heads — that is cert storage (--cert-storage-dsn), a different store in
// a different process. This one only makes two heads SERVE one challenge.
//
// No background goroutine anywhere in this type: no sweeper (the store's
// native TTL reaps keys), no lease refresher (challenges are not leases),
// no pool of workers (every call runs on the caller's goroutine, bounded
// by a context deadline). Reviewers looking for one will not find it.

// ChallengeNamespace is the chassis-reserved KV namespace the challenge
// records live in, scoped under the system tenant:
// `_sys/_txc.acme/<owner fqdn>`. Reserved (`_txc.*`) so author-facing
// writers — txco://kv/* and KV/ seed packs — refuse it, the same idiom the
// websocket directory and the blob index rely on.
const ChallengeNamespace = kvstore.ReservedNamespacePrefix + ".acme"

// challengeTenant is the tenant segment of every challenge key. A fixed,
// reserved tenant rather than the zone owner's, and not as a shortcut: the
// writers have no tenant in hand (the solver gets a name + zone, the
// RFC2136 receiver a wire owner), and a challenge must resolve regardless
// of who owns the zone. `_sys` can never collide with a real tenant.
const challengeTenant = tenants.SystemTenantSlug

// Selector values for --dns-challenge-store.
const (
	ChallengeBackendMemory = "memory"
	ChallengeBackendKV     = "kv"
)

const (
	// challengeMaxValueBytes caps one stored value. An ACME key
	// authorization is 43 bytes; the cap exists because the RFC2136
	// receiver's writer is TSIG-authenticated but external, and on a shared
	// store a runaway value is parked in Redis for ten minutes, not one
	// process's heap.
	challengeMaxValueBytes = 2048

	// challengeMaxEntries caps the values at one owner. ACME places at most
	// two per order; over the cap the earliest-expiring is evicted so a
	// live order still wins.
	challengeMaxEntries = 8

	// challengeCASRounds bounds the outer read-modify-write loop. KV.CAS
	// already retries internally, but against the SAME expected value — a
	// genuinely concurrent peer write comes back as a clean "did not swap"
	// with the value now in the store, and it is THIS loop that re-applies
	// the mutation to that value. See mutate.
	challengeCASRounds = 5

	// challengeWriteTimeout bounds a whole Present/CleanUp (all rounds).
	// Writers are the in-process solver and the RFC2136 receiver — low
	// volume and authenticated — so they may wait for a slow store.
	challengeWriteTimeout = 3 * time.Second

	// challengeReadTimeout bounds ActiveTXT's store call. This deadline is
	// load-bearing, not belt-and-braces: the redis client is configured
	// with a 30s read timeout (redisstore.go), so without a context
	// deadline a stalled store would hold a DNS handler for 30 seconds.
	// The context propagates all the way down (KV.Get → store.Get →
	// go-redis honours ctx for both the pool wait and the socket read), so
	// no goroutine or timer per query is needed to enforce it.
	challengeReadTimeout = 250 * time.Millisecond

	// challengeStoreSlots bounds CONCURRENT in-flight store reads from the
	// query path — acquired non-blocking; when none is free the query is
	// answered as "no challenge". This is the primary defence: what
	// threatens the head under a distinct-name flood (or a slow store) is
	// simultaneous store calls holding handler goroutines and pool
	// connections, which a requests-per-second throttle does not bound.
	challengeStoreSlots = 32

	// challengeMemoTTL is how long a read result (positive OR negative) is
	// reused before the store is asked again. It absorbs the CA's and
	// certmagic's repeated propagation probes and, when the store is down,
	// throttles the warning to one per owner per period.
	challengeMemoTTL = time.Second

	// challengeMemoMax bounds the memo. Only the leftmost label gates a
	// lookup, so `_acme-challenge.<random>.<served zone>` reaches the store
	// for unlimited attacker-chosen names; two generations of this size
	// are dropped wholesale rather than tracked individually.
	challengeMemoMax = 1024

	// challengeTombstoneTTL is the TTL on an emptied owner. CleanUp swaps
	// the last value out to `[]` instead of deleting the key: an
	// unconditional delete races a peer's concurrent Present and wipes it,
	// whereas a CAS to `[]` fails cleanly and retries. The store reaps the
	// empty key shortly after.
	challengeTombstoneTTL = 5 * time.Second
)

// ChallengeBackend resolves the --dns-challenge-store selector against
// whether the node's --kvstore is fleet-shared. It is the single source of
// truth for BOTH the boot guard in server.go and the constructor here, and
// it is a pure function so the refusal is unit-testable (nothing in the
// repo tests server.Start).
//
// "" and "memory" select the in-process store — correct when this process
// is the only nameserver. "kv" selects the shared store and is refused on a
// node-local --kvstore: a boltdb-backed challenge store would make each
// head serve only the challenges it wrote itself, which is precisely the
// failure the setting exists to remove — and, as a bonus hazard, nothing
// ever lists the namespace, so a boltdb file would accumulate dead keys
// forever (chassis/kv expires boltdb entries lazily, on Get).
func ChallengeBackend(sel string, kvShared bool) (string, error) {
	switch strings.ToLower(strings.TrimSpace(sel)) {
	case "", ChallengeBackendMemory:
		return ChallengeBackendMemory, nil
	case ChallengeBackendKV:
		if !kvShared {
			return "", fmt.Errorf("--dns-challenge-store=kv needs a shared --kvstore (%s): on a node-local store each nameserver would serve only the challenges it wrote itself", redisstore.StoreName)
		}
		return ChallengeBackendKV, nil
	default:
		return "", fmt.Errorf("--dns-challenge-store=%q: want \"\" (in-process), %s, or %s", sel, ChallengeBackendMemory, ChallengeBackendKV)
	}
}

// kvChallengeEntry is one stored value with its own absolute expiry (unix
// seconds). Per-entry expiry rather than a bare string array because the
// KEY's TTL is refreshed by any Present: a value added at minute 9 of an
// abandoned solve would otherwise extend an earlier one to minute 19. With
// per-entry expiry the semantics are byte-for-byte those of the in-memory
// store, which lets one test table drive both backends.
type kvChallengeEntry struct {
	V string `json:"v"`
	X int64  `json:"x"`
}

// kvChallengeStore is the ChallengeStore over the shared KV. See the file
// header for what it is and is not.
type kvChallengeStore struct {
	ctx    context.Context // the controller's; every store call derives its deadline from it
	kv     *kvstore.KV     // an UNCLAMPED handle (kvstore.New(store, 0, 0)) — see selectChallengeStore
	logger *zap.Logger
	ttl    time.Duration // safety expiry per value (challengeStoreTTL)
	now    func() time.Time

	slots        chan struct{}  // bounded concurrent store reads (query path only)
	memo         *challengeMemo // 1s read memo, positive and negative
	readTimeout  time.Duration
	writeTimeout time.Duration
}

func newKVChallengeStore(ctx context.Context, kv *kvstore.KV, logger *zap.Logger) *kvChallengeStore {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &kvChallengeStore{
		ctx:          ctx,
		kv:           kv,
		logger:       logger,
		ttl:          challengeStoreTTL,
		now:          time.Now,
		slots:        make(chan struct{}, challengeStoreSlots),
		memo:         newChallengeMemo(challengeMemoTTL, challengeMemoMax),
		readTimeout:  challengeReadTimeout,
		writeTimeout: challengeWriteTimeout,
	}
}

// Present publishes value at fqdn (idempotent per value; refreshes the
// expiry of an existing one). On any failure it logs at Error and returns
// — it never falls back to a local write: one head answering and one not
// is strictly worse than a clean failure, because the CA validates from
// several vantage points and a partial publish fails unpredictably.
func (s *kvChallengeStore) Present(fqdn, value string) {
	key, ok := challengeKey(fqdn)
	if !ok {
		s.logger.Debug("dns challenge: owner rejected", zap.String("fqdn", fqdn))
		return
	}
	if value == "" || len(value) > challengeMaxValueBytes {
		s.logger.Warn("dns challenge: value rejected",
			zap.String("owner", key), zap.Int("bytes", len(value)), zap.Int("max", challengeMaxValueBytes))
		return
	}
	s.mutate(key, "present", func(live []kvChallengeEntry, now time.Time) ([]kvChallengeEntry, bool) {
		exp := now.Add(s.ttl).Unix()
		for i := range live {
			if live[i].V == value {
				live[i].X = exp // refresh existing
				return live, true
			}
		}
		live = append(live, kvChallengeEntry{V: value, X: exp})
		if len(live) > challengeMaxEntries {
			// Keep the newest: evict the earliest-expiring until under cap.
			sort.SliceStable(live, func(i, j int) bool { return live[i].X < live[j].X })
			drop := len(live) - challengeMaxEntries
			s.logger.Warn("dns challenge: too many values at owner; evicting earliest-expiring",
				zap.String("owner", key), zap.Int("dropped", drop), zap.Int("max", challengeMaxEntries))
			live = live[drop:]
		}
		return live, true
	})
}

// CleanUp removes value at fqdn. Idempotent: an absent or expired value is
// a no-op with no write. Runs the same CAS loop as Present — a peer's
// Present landing between our read and our write must not leave the
// removed value behind until its TTL (see mutate).
func (s *kvChallengeStore) CleanUp(fqdn, value string) {
	key, ok := challengeKey(fqdn)
	if !ok {
		return
	}
	s.mutate(key, "cleanup", func(live []kvChallengeEntry, _ time.Time) ([]kvChallengeEntry, bool) {
		out := make([]kvChallengeEntry, 0, len(live))
		removed := false
		for _, e := range live {
			if e.V == value {
				removed = true
				continue
			}
			out = append(out, e)
		}
		return out, removed
	})
}

// clearAll implements challengeClearer: drop every value at the owner in
// one CAS (the RFC2136 "delete RRset" op).
func (s *kvChallengeStore) clearAll(fqdn string) {
	key, ok := challengeKey(fqdn)
	if !ok {
		return
	}
	s.mutate(key, "clear", func(live []kvChallengeEntry, _ time.Time) ([]kvChallengeEntry, bool) {
		return nil, len(live) > 0
	})
}

// ActiveTXT is the only method on the query path and the only one with a
// tight budget. Defences, in order: memo → bounded concurrent store slots
// → short store timeout → KV.Get. A memo hit costs nothing and is never
// blocked by saturation; the slot is taken BEFORE the deadline is derived
// so a saturated store spends no context machinery at all.
//
// Every failure — throttled, timed out, store error, malformed value,
// rejected name — returns nil, which answerChallenge treats as "no
// challenge" and falls through to the snapshot's normal NXDOMAIN/NODATA.
// Never SERVFAIL, never a panic to recover from.
func (s *kvChallengeStore) ActiveTXT(fqdn string) []string {
	key, ok := challengeKey(fqdn)
	if !ok {
		return nil
	}
	now := s.now()
	if vals, hit := s.memo.get(key, now); hit {
		return vals
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return nil // every slot busy: answer as "no challenge" rather than queue
	}
	defer func() { <-s.slots }()

	ctx, cancel := context.WithTimeout(s.ctx, s.readTimeout)
	defer cancel()
	raw, found, err := s.kv.Get(ctx, challengeTenant, ChallengeNamespace, key)
	if err != nil {
		// Negative memo: one warning per owner per memo period, not one per
		// query, and no repeated store calls while it is down.
		s.memo.set(key, nil, now)
		s.logger.Warn("dns challenge store read failed; answering as no challenge",
			zap.String("owner", key), zap.Error(err))
		return nil
	}
	var vals []string
	if found {
		vals = challengeValues(liveChallengeEntries(decodeChallengeEntries(raw), now))
	}
	s.memo.set(key, vals, now)
	return vals
}

// mutate is the one read-modify-write loop behind Present, CleanUp and
// clearAll. fn gets the live (unexpired) entries and returns the next set,
// or write=false for an idempotent no-op that needs no round trip.
//
// The outer loop is what makes concurrent writers from two heads safe:
// KV.CAS reports a lost race as (swapped=false, current, nil) — success-
// shaped, not an error — handing back the value now in the store. Treating
// that as "done" would silently drop the peer's value (Present) or leave
// ours behind until its TTL (CleanUp), so each round re-applies fn to
// `current` and tries again; the failed CAS already returned it, so a
// retry costs no extra read.
//
// On exhaustion or a store error: log at Error, drop the memo entry (never
// memoize a state we did not achieve — certmagic's propagation check would
// go green on this head while a peer serves nothing) and return.
func (s *kvChallengeStore) mutate(key, op string, fn func(live []kvChallengeEntry, now time.Time) (next []kvChallengeEntry, write bool)) {
	ctx, cancel := context.WithTimeout(s.ctx, s.writeTimeout)
	defer cancel()

	raw, found, err := s.kv.Get(ctx, challengeTenant, ChallengeNamespace, key)
	if err != nil {
		s.fail(key, op, "read", err)
		return
	}
	for round := 0; round < challengeCASRounds; round++ {
		now := s.now()
		live := liveChallengeEntries(decodeChallengeEntries(raw), now)
		next, write := fn(live, now)
		if !write {
			return
		}
		newRaw := json.RawMessage("[]")
		ttl := challengeTombstoneTTL
		if len(next) > 0 {
			b, merr := json.Marshal(next)
			if merr != nil {
				s.fail(key, op, "encode", merr)
				return
			}
			newRaw = b
			ttl = maxChallengeRemaining(next, now)
		}
		swapped, current, cerr := s.kv.CAS(ctx, challengeTenant, ChallengeNamespace, key, !found, raw, newRaw, ttl)
		if cerr != nil {
			s.fail(key, op, "cas", cerr)
			return
		}
		if swapped {
			// The achieved state, memoized at once: the writing head answers
			// its own challenge with no round trip and no lag.
			s.memo.set(key, challengeValues(next), now)
			return
		}
		// Lost the race to a peer. Re-derive from what is there now.
		raw, found = current, current != nil
	}
	s.fail(key, op, "cas", errors.New("contention: a peer won every round"))
}

func (s *kvChallengeStore) fail(key, op, stage string, err error) {
	s.memo.del(key)
	s.logger.Error("dns challenge store write failed; this head cannot serve the challenge",
		zap.String("op", op), zap.String("stage", stage), zap.String("owner", key), zap.Error(err))
}

// decodeChallengeEntries parses a stored value. A malformed value decodes
// to nothing: the next write replaces it (its raw bytes are still what the
// CAS is conditioned on, so the replacement stays atomic).
func decodeChallengeEntries(raw json.RawMessage) []kvChallengeEntry {
	if len(raw) == 0 {
		return nil
	}
	var out []kvChallengeEntry
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// liveChallengeEntries drops expired entries and empty values. Live iff
// the expiry is strictly after now — the in-memory store's rule.
func liveChallengeEntries(in []kvChallengeEntry, now time.Time) []kvChallengeEntry {
	if len(in) == 0 {
		return nil
	}
	ts := now.Unix()
	out := make([]kvChallengeEntry, 0, len(in))
	for _, e := range in {
		if e.V != "" && e.X > ts {
			out = append(out, e)
		}
	}
	return out
}

func challengeValues(in []kvChallengeEntry) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.V)
	}
	return out
}

// maxChallengeRemaining is the key TTL: the longest remaining lifetime
// among the entries, floored at one second so a value expiring within the
// second still lands with a TTL the store accepts.
func maxChallengeRemaining(in []kvChallengeEntry, now time.Time) time.Duration {
	var max int64
	for _, e := range in {
		if e.X > max {
			max = e.X
		}
	}
	d := time.Duration(max-now.Unix()) * time.Second
	if d < time.Second {
		d = time.Second
	}
	return d
}

// challengeMemo is a size-bounded, two-generation read memo: when the
// current generation fills, it becomes the previous and a fresh one
// starts; the one before that is dropped wholesale. Entries are only ~1s
// useful, so nothing finer is worth the bookkeeping.
type challengeMemo struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	cur  map[string]memoEntry
	prev map[string]memoEntry
}

type memoEntry struct {
	vals  []string
	until time.Time
}

func newChallengeMemo(ttl time.Duration, max int) *challengeMemo {
	if max < 2 {
		max = 2
	}
	return &challengeMemo{ttl: ttl, max: max, cur: make(map[string]memoEntry)}
}

// get returns the memoized values (nil for a negative entry) and whether
// the entry is live. Callers must treat the slice as read-only.
func (m *challengeMemo) get(key string, now time.Time) ([]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.cur[key]; ok {
		return e.vals, e.until.After(now)
	}
	if e, ok := m.prev[key]; ok && e.until.After(now) {
		return e.vals, true
	}
	return nil, false
}

func (m *challengeMemo) set(key string, vals []string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.cur[key]; !exists && len(m.cur) >= m.max {
		m.prev, m.cur = m.cur, make(map[string]memoEntry, m.max/4)
	}
	m.cur[key] = memoEntry{vals: vals, until: now.Add(m.ttl)}
}

func (m *challengeMemo) del(key string) {
	m.mu.Lock()
	delete(m.cur, key)
	delete(m.prev, key)
	m.mu.Unlock()
}
