// Package kv is a thin, tenant-scoped wrapper over the configured key-value
// store backend that backs the op-writable key-value ops (txco://kv/*). It is the only
// general, mutable, op-writable persistence in the chassis — distinct from
// the immutable filecas/continuation/artifact stores and the secret store.
//
// The same store.Store interface is satisfied by boltdb (embedded, on-disk)
// and redis (shared, networked — native TTL + atomic ops), selected by
// --kvstore. This wrapper adds the three things the raw store doesn't give us:
//
//   - Tenant + namespace scoping: every key is composed as
//     <tenant>/<namespace>/<userkey>. The tenant comes from the trusted
//     request scope (never the mutable _txc.tenant); the namespace is an
//     organizational prefix (default = the routed stack), not a security
//     boundary.
//   - JSON values: callers store/retrieve arbitrary JSON, not opaque bytes.
//   - Uniform TTL: values are wrapped with an optional expiry and
//     lazy-expired on read, AND WriteOptions.TTL is passed so native
//     backends (redis) also GC. Persistent keys (the default) carry no
//     expiry. A configurable max-TTL clamps requested TTLs downward.
package kv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kvtools/valkeyrie/store"
)

// segMax bounds each key segment (tenant / namespace / userkey).
const segMax = 256

// casAttempts bounds the compare-and-swap retry loop (Incr, CAS) under contention.
const casAttempts = 5

// DefaultListLimit / MaxListLimit bound a single ListKeysPage response. The
// underlying store.List has no native cursor (it returns the whole namespace),
// so the window caps the RESPONSE size, not the read. Requests above the max are
// clamped down; a non-positive request uses the default.
const (
	DefaultListLimit = 200
	MaxListLimit     = 200
)

// KV is a tenant-scoped view over the underlying key-value store.
type KV struct {
	s        store.Store
	maxValue int           // value-size cap in bytes; 0 = unlimited
	maxTTL   time.Duration // ttl ceiling; 0 = unlimited
	now      func() time.Time
}

// wrapper is the stored envelope: the caller's JSON value plus an optional
// absolute expiry (unix seconds; 0/absent = persistent).
type wrapper struct {
	V   json.RawMessage `json:"v"`
	Exp int64           `json:"exp,omitempty"`
}

// New returns a KV over s. maxValueBytes/maxTTL of 0 mean unlimited.
func New(s store.Store, maxValueBytes int, maxTTL time.Duration) *KV {
	return &KV{s: s, maxValue: maxValueBytes, maxTTL: maxTTL, now: time.Now}
}

// segOK validates a single key segment: non-empty, bounded, and free of
// the '/' separator and control characters so the composed key is
// unambiguous and a caller cannot escape its namespace.
func segOK(s string) bool {
	if s == "" || len(s) > segMax {
		return false
	}
	for _, r := range s {
		if r == '/' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (k *KV) fullKey(tenant, ns, key string) (string, error) {
	if !segOK(tenant) {
		return "", fmt.Errorf("kv: invalid tenant scope")
	}
	if !segOK(ns) {
		return "", fmt.Errorf("kv: invalid namespace %q", ns)
	}
	if !segOK(key) {
		return "", fmt.Errorf("kv: invalid key %q", key)
	}
	return tenant + "/" + ns + "/" + key, nil
}

func (k *KV) clampTTL(ttl time.Duration) time.Duration {
	if ttl < 0 {
		return 0
	}
	if k.maxTTL > 0 && ttl > k.maxTTL {
		return k.maxTTL
	}
	return ttl
}

// expired reports whether w has a set expiry that has passed.
func (k *KV) expired(w wrapper) bool {
	return w.Exp != 0 && k.now().Unix() >= w.Exp
}

// Get returns the JSON value for (tenant, ns, key). found is false for a
// missing or lazily-expired key (no error in either case).
func (k *KV) Get(ctx context.Context, tenant, ns, key string) (value json.RawMessage, found bool, err error) {
	if k == nil || k.s == nil {
		return nil, false, errors.New("kv: store not configured")
	}
	fk, err := k.fullKey(tenant, ns, key)
	if err != nil {
		return nil, false, err
	}
	pair, gerr := k.s.Get(ctx, fk, nil)
	if gerr != nil {
		if errors.Is(gerr, store.ErrKeyNotFound) {
			return nil, false, nil
		}
		return nil, false, gerr
	}
	var w wrapper
	if uerr := json.Unmarshal(pair.Value, &w); uerr != nil {
		return nil, false, fmt.Errorf("kv: decode %q: %w", key, uerr)
	}
	if k.expired(w) {
		_ = k.s.Delete(ctx, fk) // best-effort lazy GC; ignore races
		return nil, false, nil
	}
	return w.V, true, nil
}

// Set writes value at (tenant, ns, key). A ttl <= 0 stores a persistent
// key (no expiry); a positive ttl is clamped to maxTTL.
func (k *KV) Set(ctx context.Context, tenant, ns, key string, value json.RawMessage, ttl time.Duration) error {
	if k == nil || k.s == nil {
		return errors.New("kv: store not configured")
	}
	fk, err := k.fullKey(tenant, ns, key)
	if err != nil {
		return err
	}
	if !json.Valid(value) {
		return errors.New("kv: value is not valid JSON")
	}
	if k.maxValue > 0 && len(value) > k.maxValue {
		return fmt.Errorf("kv: value %d bytes exceeds cap %d", len(value), k.maxValue)
	}
	blob, wo := k.encode(value, ttl)
	return k.s.Put(ctx, fk, blob, wo)
}

// Delete removes (tenant, ns, key). A missing key is not an error.
func (k *KV) Delete(ctx context.Context, tenant, ns, key string) error {
	if k == nil || k.s == nil {
		return errors.New("kv: store not configured")
	}
	fk, err := k.fullKey(tenant, ns, key)
	if err != nil {
		return err
	}
	if derr := k.s.Delete(ctx, fk); derr != nil && !errors.Is(derr, store.ErrKeyNotFound) {
		return derr
	}
	return nil
}

// listKeysAll returns all live (non-expired) user keys under (tenant, ns), order
// unspecified — the shared read behind ListKeys and ListKeysPage. The composed
// <tenant>/<ns>/ prefix is stripped so callers get bare user keys.
func (k *KV) listKeysAll(ctx context.Context, tenant, ns string) ([]string, error) {
	if k == nil || k.s == nil {
		return nil, errors.New("kv: store not configured")
	}
	if !segOK(tenant) {
		return nil, fmt.Errorf("kv: invalid tenant scope")
	}
	if !segOK(ns) {
		return nil, fmt.Errorf("kv: invalid namespace %q", ns)
	}
	prefix := tenant + "/" + ns + "/"
	pairs, err := k.s.List(ctx, prefix, nil)
	if err != nil {
		if errors.Is(err, store.ErrKeyNotFound) {
			return nil, nil // namespace has no keys
		}
		return nil, err
	}
	var keys []string
	for _, p := range pairs {
		// Backends differ on whether List returns full or directory-relative
		// keys; TrimPrefix yields the bare user key either way. A residual '/'
		// would mean a deeper path (segOK forbids '/' in user keys) — skip it.
		key := strings.TrimPrefix(p.Key, prefix)
		if key == "" || strings.ContainsRune(key, '/') {
			continue
		}
		var w wrapper
		if json.Unmarshal(p.Value, &w) == nil && k.expired(w) {
			continue
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// ListKeys returns the user keys currently stored under (tenant, ns) — the
// namespace view the declarative store-seed reconciler needs to find which
// managed keys a re-applied pack dropped. Lazily-expired keys are filtered
// (parity with Get); order is unspecified; an empty namespace yields nil.
func (k *KV) ListKeys(ctx context.Context, tenant, ns string) ([]string, error) {
	return k.listKeysAll(ctx, tenant, ns)
}

// ListKeysPage returns a stable, windowed page of the user keys under
// (tenant, ns), sorted ascending. It returns up to `limit` keys (clamped to
// MaxListLimit; a non-positive limit uses DefaultListLimit) that sort strictly
// AFTER the `after` cursor (empty = from the start), plus `next`: the cursor to
// pass on the following call, or "" when the namespace is exhausted.
//
// The backing store.List has no native cursor (redis SCANs to exhaustion,
// boltdb walks the bucket), so this fetches all keys then sorts + slices — the
// window bounds the RESPONSE, not the underlying read. Fine for the modest
// namespaces this serves; a truly large namespace pays a full in-memory sort per
// page. Deterministic order makes the `after` cursor a stable resume point even
// as keys are added/removed between pages.
func (k *KV) ListKeysPage(ctx context.Context, tenant, ns, after string, limit int) (keys []string, next string, err error) {
	all, err := k.listKeysAll(ctx, tenant, ns)
	if err != nil {
		return nil, "", err
	}
	if limit <= 0 || limit > MaxListLimit {
		limit = DefaultListLimit
	}
	sort.Strings(all)
	// First index strictly greater than the cursor (0 when after == "", since
	// every non-empty key sorts after "").
	i := sort.Search(len(all), func(j int) bool { return all[j] > after })
	end := i + limit
	if end > len(all) {
		end = len(all)
	}
	page := append([]string(nil), all[i:end]...)
	if end < len(all) {
		next = all[end-1] // more remain; resume strictly after the last returned key
	}
	return page, next, nil
}

// Incr atomically adds delta to an integer value (creating it at delta if
// absent or expired) using the store's CAS primitive, and returns the new
// value. A positive ttl (clamped) is (re)applied on each increment.
func (k *KV) Incr(ctx context.Context, tenant, ns, key string, delta int64, ttl time.Duration) (int64, error) {
	if k == nil || k.s == nil {
		return 0, errors.New("kv: store not configured")
	}
	fk, err := k.fullKey(tenant, ns, key)
	if err != nil {
		return 0, err
	}
	for attempt := 0; attempt < casAttempts; attempt++ {
		var cur int64
		var casPrev *store.KVPair

		pair, gerr := k.s.Get(ctx, fk, nil)
		switch {
		case gerr == nil:
			casPrev = pair // CAS against the existing pair (replaces expired too)
			var w wrapper
			if json.Unmarshal(pair.Value, &w) != nil {
				return 0, fmt.Errorf("kv: incr on corrupt value at %q", key)
			}
			if !k.expired(w) {
				if json.Unmarshal(w.V, &cur) != nil {
					return 0, fmt.Errorf("kv: incr on non-integer value at %q", key)
				}
			} // expired → cur stays 0 (reset), CAS-replacing the stale pair
		case errors.Is(gerr, store.ErrKeyNotFound):
			casPrev = nil // create
		default:
			return 0, gerr
		}

		next := cur + delta
		blob, wo := k.encode(json.RawMessage(strconv.FormatInt(next, 10)), ttl)
		ok, _, perr := k.s.AtomicPut(ctx, fk, blob, casPrev, wo)
		if perr != nil {
			if errors.Is(perr, store.ErrKeyExists) || errors.Is(perr, store.ErrKeyModified) {
				continue // lost the race; re-read and retry
			}
			return 0, perr
		}
		if ok {
			return next, nil
		}
	}
	return 0, fmt.Errorf("kv: incr contention on %q", key)
}

// CAS is check-and-set: write newVal at (tenant, ns, key) only if the current
// value passes the check, using the store's atomic compare so it stays
// race-safe under concurrency. With expectAbsent=true it writes only if the
// key is absent (a create-if-missing / lock primitive); otherwise it writes
// only if the current value equals `expected`. Returns whether it swapped and
// the value now in the store (newVal if swapped, else the existing value —
// nil if the key was absent).
func (k *KV) CAS(ctx context.Context, tenant, ns, key string, expectAbsent bool, expected, newVal json.RawMessage, ttl time.Duration) (swapped bool, current json.RawMessage, err error) {
	if k == nil || k.s == nil {
		return false, nil, errors.New("kv: store not configured")
	}
	fk, ferr := k.fullKey(tenant, ns, key)
	if ferr != nil {
		return false, nil, ferr
	}
	if !json.Valid(newVal) {
		return false, nil, errors.New("kv: value is not valid JSON")
	}
	if k.maxValue > 0 && len(newVal) > k.maxValue {
		return false, nil, fmt.Errorf("kv: value %d bytes exceeds cap %d", len(newVal), k.maxValue)
	}
	if !expectAbsent && !json.Valid(expected) {
		return false, nil, errors.New("kv: expected is not valid JSON")
	}

	for attempt := 0; attempt < casAttempts; attempt++ {
		var curVal json.RawMessage
		var prev *store.KVPair
		live := false // key exists AND is not expired

		pair, gerr := k.s.Get(ctx, fk, nil)
		switch {
		case gerr == nil:
			prev = pair // CAS against the existing pair (replaces an expired one too)
			var w wrapper
			if json.Unmarshal(pair.Value, &w) == nil && !k.expired(w) {
				live, curVal = true, w.V
			}
		case errors.Is(gerr, store.ErrKeyNotFound):
			prev = nil
		default:
			return false, nil, gerr
		}

		pass := !live // expectAbsent: pass iff the key isn't live
		if !expectAbsent {
			pass = live && jsonEqual(curVal, expected)
		}
		if !pass {
			return false, curVal, nil // check failed — report the actual current
		}

		blob, wo := k.encode(newVal, ttl)
		ok, _, perr := k.s.AtomicPut(ctx, fk, blob, prev, wo)
		if perr != nil {
			if errors.Is(perr, store.ErrKeyExists) || errors.Is(perr, store.ErrKeyModified) {
				continue // raced between read and write; re-read and re-check
			}
			return false, nil, perr
		}
		if ok {
			return true, newVal, nil
		}
	}
	return false, nil, fmt.Errorf("kv: cas contention on %q", key)
}

// jsonEqual reports whether two JSON values are semantically equal (ignoring
// whitespace and object key order). Falls back to a raw byte compare if either
// side doesn't parse.
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return bytes.Equal(a, b)
	}
	return reflect.DeepEqual(av, bv)
}

// encode wraps value with an optional expiry and returns the stored bytes
// plus the WriteOptions (native TTL) — nil for a persistent key.
func (k *KV) encode(value json.RawMessage, ttl time.Duration) ([]byte, *store.WriteOptions) {
	ttl = k.clampTTL(ttl)
	w := wrapper{V: value}
	var wo *store.WriteOptions
	if ttl > 0 {
		w.Exp = k.now().Add(ttl).Unix()
		wo = &store.WriteOptions{TTL: ttl}
	}
	blob, _ := json.Marshal(w) // wrapper marshals cleanly (V is valid JSON)
	return blob, wo
}

// ParseTTLSeconds converts an integer-seconds WITH param to a Duration.
// Zero or negative → 0 (persistent).
func ParseTTLSeconds(secs int64) time.Duration {
	if secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}
