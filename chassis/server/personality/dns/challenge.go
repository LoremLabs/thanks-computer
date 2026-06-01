package dns

import (
	"strings"
	"sync"
	"time"
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
// Seam shape mirrors chassis/auth/registry/dialect.go: core ships the
// interface + an in-memory default; a shared (Postgres) implementation is
// registered out of tree by the overlay so a challenge written on one
// chassis is served by another when Let's Encrypt's validator lands there
// (see ChallengeStoreForDSN).
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

// challengeEntry is one published value with its expiry.
type challengeEntry struct {
	value   string
	expires time.Time
}

// memChallengeStore is the in-tree default: a single-process, in-memory
// store. It is the whole story for single-node deployments (the solver
// and the DNS head share one process, so Present is a direct write the
// next query sees). A fleet needs a shared backend — that lands in a
// downstream overlay behind ChallengeStoreForDSN.
type memChallengeStore struct {
	mu  sync.RWMutex
	ttl time.Duration
	rec map[string][]challengeEntry // key: lowercased FQDN
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
	key := strings.ToLower(fqdn)
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
	key := strings.ToLower(fqdn)
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
	key := strings.ToLower(fqdn)
	now := m.now()
	m.mu.RLock()
	entries := m.rec[key]
	m.mu.RUnlock()
	var out []string
	for _, e := range entries {
		if e.expires.After(now) {
			out = append(out, e.value)
		}
	}
	return out
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

// challengeStoreFactories lets the overlay register a shared backend
// (e.g. Postgres) by DSN scheme, never compiled into core — the same
// "driver registered out of tree" rule as the auth Dialect seam.
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
