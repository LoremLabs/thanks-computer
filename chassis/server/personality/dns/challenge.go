package dns

import (
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// ChallengeStore holds the short-lived `_acme-challenge` TXT records the
// DNS head serves while a cert is being issued via ACME DNS-01. It is the
// shared substrate for Phase 3: the in-process ACME solver (bundled in
// chassis/tls) AND the RFC2136 UPDATE receiver (the Caddy deploy) both
// write through it, and the query path reads it.
//
// These records are deliberately OUTSIDE the ZoneSnapshot / dbcache
// reload cycle: they live for seconds-to-minutes and churn during
// issuance, so routing them through config-apply would be both too slow
// and semantically wrong (they are not durable config). The store is a
// small, lock-guarded, self-expiring map consulted on the query hot path
// — but only for the `_acme-challenge.*` name, so normal lookups never
// touch it.
//
// Two backends ship in core: the in-memory default (one process is the
// only nameserver) and a KV-backed store over the shared --kvstore
// (challenge_kv.go), so a challenge written on one head is served by every
// other — Let's Encrypt validates from several vantage points and may ask
// any nameserver in the NS set. Selected by --dns-challenge-store.
//
// Both backends key on challengeKey, so a name written by one and read by
// the other (or by answerChallenge, which keys with dns.Fqdn) cannot miss
// on a trailing-dot or case difference.
type ChallengeStore interface {
	// Present publishes a challenge value at the given owner FQDN. Idempotent
	// per (fqdn, value); multiple distinct values may coexist (ACME can
	// place two during a single order). The value self-expires as a safety
	// net for an abandoned solve; the normal lifecycle is an explicit CleanUp.
	Present(fqdn, value string)

	// CleanUp removes a specific (fqdn, value). Idempotent — removing an
	// absent value, or one already expired, is a no-op.
	CleanUp(fqdn, value string)

	// ActiveTXT returns the live (unexpired) challenge values for an exact
	// owner FQDN, or nil. fqdn is matched case-insensitively as a
	// trailing-dot FQDN.
	ActiveTXT(fqdn string) []string
}

const (
	// challengeRecordTTL is the TTL on the served challenge RR. Kept tiny so
	// a resolver never caches a challenge past its CleanUp.
	challengeRecordTTL uint32 = 1

	// challengeStoreTTL is the in-memory safety expiry: certmagic/Caddy call
	// CleanUp explicitly when a solve finishes, but a crashed or abandoned
	// solve must not leave a record served forever.
	challengeStoreTTL = 10 * time.Minute

	// acmeChallengeLabel is the leftmost label ACME DNS-01 uses. We only
	// ever serve from / accept writes for names under this label.
	acmeChallengeLabel = "_acme-challenge."
)

// isACMEChallengeName reports whether qname (a lowercased FQDN) is an ACME
// DNS-01 challenge owner — i.e. its leftmost label is `_acme-challenge`.
func isACMEChallengeName(qname string) bool {
	return strings.HasPrefix(qname, acmeChallengeLabel)
}

// challengeKeyMax bounds a stored owner name. Mirrors chassis/kv's per-
// segment cap (segMax) so the KV backend never composes a key the store
// refuses; the in-memory backend applies the same bound so both agree on
// which names are storable. A legal wire name is ≤255 bytes, but an escaped
// presentation form (\DDD) can exceed it — those are rejected, not stored.
const challengeKeyMax = 256

// challengeKey normalizes an owner name to the form every backend keys on:
// lowercased, trailing-dot FQDN — the exact form answerChallenge derives
// from a query (strings.ToLower(dns.Fqdn(q.Name))) — and validated with
// chassis/kv's segment rules (non-empty, bounded, no '/' and no control
// characters) so a name that passes here is a legal KV key segment. ok is
// false for a name no backend will store or serve.
func challengeKey(fqdn string) (string, bool) {
	k := strings.ToLower(strings.TrimSpace(fqdn))
	if k == "" {
		return "", false
	}
	k = dns.Fqdn(k)
	if len(k) > challengeKeyMax {
		return "", false
	}
	for _, r := range k {
		if r == '/' || r < 0x20 || r == 0x7f {
			return "", false
		}
	}
	return k, true
}

// challengeClearer is an optional extension a backend implements when it
// can drop every value at an owner in one step. The RFC2136 receiver's
// "delete RRset" op (update.go) prefers it over enumerate-then-CleanUp:
// on the shared backend that is one CAS instead of N+1 round trips, and it
// cannot miss a value a peer published between the enumerate and the
// deletes. Both in-tree backends implement it.
type challengeClearer interface {
	clearAll(fqdn string)
}

// challengeEntry is one published value with its expiry.
type challengeEntry struct {
	value   string
	expires time.Time
}

// memChallengeStore is the in-tree default: a single-process, in-memory
// store. It is the whole story when one process is the only nameserver
// (the solver and the DNS head share it, so Present is a direct write the
// next query sees). Two or more nameservers need the shared KV backend
// (kvChallengeStore, --dns-challenge-store=kv).
type memChallengeStore struct {
	mu  sync.RWMutex
	ttl time.Duration
	rec map[string][]challengeEntry // key: challengeKey(fqdn)
	now func() time.Time            // injectable for tests
}

func newMemChallengeStore() *memChallengeStore {
	return &memChallengeStore{
		ttl: challengeStoreTTL,
		rec: map[string][]challengeEntry{},
		now: time.Now,
	}
}

func (m *memChallengeStore) Present(fqdn, value string) {
	key, ok := challengeKey(fqdn)
	if !ok {
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	live := m.prune(m.rec[key], now)
	for i := range live {
		if live[i].value == value { // refresh existing
			live[i].expires = now.Add(m.ttl)
			m.rec[key] = live
			return
		}
	}
	m.rec[key] = append(live, challengeEntry{value: value, expires: now.Add(m.ttl)})
}

func (m *memChallengeStore) CleanUp(fqdn, value string) {
	key, ok := challengeKey(fqdn)
	if !ok {
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	live := m.prune(m.rec[key], now)
	out := live[:0]
	for _, e := range live {
		if e.value != value {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		delete(m.rec, key)
		return
	}
	m.rec[key] = out
}

func (m *memChallengeStore) ActiveTXT(fqdn string) []string {
	key, ok := challengeKey(fqdn)
	if !ok {
		return nil
	}
	now := m.now()
	// Collect under the read lock: prune compacts the backing array in place
	// under the write lock, so iterating a copied slice header outside the
	// lock races with a concurrent Present/CleanUp.
	m.mu.RLock()
	var out []string
	for _, e := range m.rec[key] {
		if e.expires.After(now) {
			out = append(out, e.value)
		}
	}
	m.mu.RUnlock()
	return out
}

// clearAll implements challengeClearer: drop every value at the owner.
func (m *memChallengeStore) clearAll(fqdn string) {
	key, ok := challengeKey(fqdn)
	if !ok {
		return
	}
	m.mu.Lock()
	delete(m.rec, key)
	m.mu.Unlock()
}

// prune drops expired entries. Caller holds the write lock.
func (m *memChallengeStore) prune(entries []challengeEntry, now time.Time) []challengeEntry {
	out := entries[:0]
	for _, e := range entries {
		if e.expires.After(now) {
			out = append(out, e)
		}
	}
	return out
}

// challengeStoreFactories lets an overlay register a backend by DSN scheme,
// never compiled into core — the same "driver registered out of tree" rule
// as the auth Dialect seam.
//
// NOT WIRED: nothing in core calls ChallengeStoreForDSN (the in-tree
// backends are selected by --dns-challenge-store, see ChallengeBackend in
// challenge_kv.go). It stays as an extension point only. Note its
// fallback: an unregistered scheme silently yields the in-memory store —
// exactly the one-nameserver-sees-it downgrade the boot guard in
// server.go exists to refuse. Wire it through ChallengeBackend, not around
// it, if it is ever used.
var challengeStoreFactories = map[string]func(dsn string) (ChallengeStore, error){}

// RegisterChallengeStore registers a factory for a DSN scheme (e.g.
// "postgres"). Called from an overlay init(); core registers nothing.
func RegisterChallengeStore(scheme string, f func(dsn string) (ChallengeStore, error)) {
	challengeStoreFactories[strings.ToLower(strings.TrimSpace(scheme))] = f
}

// ChallengeStoreForDSN selects the challenge backend. An empty DSN (the
// default) or any scheme without a registered factory yields the in-memory
// store — the safe, single-node default. A recognised scheme builds the
// registered backend.
func ChallengeStoreForDSN(dsn string) (ChallengeStore, error) {
	s := strings.TrimSpace(dsn)
	if s == "" {
		return newMemChallengeStore(), nil
	}
	scheme := s
	if i := strings.Index(s, ":"); i >= 0 {
		scheme = s[:i]
	}
	if f, ok := challengeStoreFactories[strings.ToLower(scheme)]; ok {
		return f(s)
	}
	return newMemChallengeStore(), nil
}
