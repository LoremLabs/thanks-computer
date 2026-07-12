// Package redisstore is the chassis's redis backend for the op-writable KV
// (txco://kv/*), registered with valkeyrie under the same "redis" name the
// fleet's TXCO_KVSTORE env selects.
//
// It is a vendored, slimmed derivative of github.com/kvtools/redis v1.2.0
// (Apache License 2.0 — see LICENSE.kvtools-redis in this directory), taken
// in-tree so the chassis controls the semantics that matter against a managed
// Redis (Upstash bills per command and every command is a network round
// trip). Changes from upstream, per Apache-2.0 §4(b):
//
//   - SCAN COUNT raised 10 → 1000 (scanCount): a full List previously cost
//     ~total_keys/10 sequential round trips regardless of the prefix asked
//     for, since SCAN walks the whole keyspace and MATCH only filters.
//   - MGET chunked at mgetChunk keys per call (upstream sent ONE unbounded
//     MGET for the entire listing).
//   - CAS/CAD lua compares the RAW STORED VALUE, not the KVPair "LastIndex"
//     field. Upstream's registry path stored raw values (RawCodec), which
//     never persists LastIndex — its lua compare was nil==nil, i.e. ALWAYS
//     true when the key existed, so AtomicPut was not actually atomic and
//     concurrent kv Incr/CAS could lose updates. Value-compare restores the
//     contract chassis/kv relies on (and drops the cjson dependency).
//   - Dropped what the chassis never uses: Sentinel/failover, locks,
//     Watch/WatchTree (stubbed with store.ErrCallNotSupported), the codec
//     indirection (values are stored raw — the chassis wrapper's own JSON
//     envelope — readable as-is in a redis console), and the boot-time
//     `CONFIG SET notify-keyspace-events` (only Watch consumed it; Upstash
//     rejects CONFIG).
//   - setNX surfaces the underlying redis error instead of folding every
//     failure into ErrKeyExists.
package redisstore

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kvtools/valkeyrie"
	"github.com/kvtools/valkeyrie/store"
	redis "github.com/redis/go-redis/v9"
)

// StoreName is the valkeyrie registry name — kept as "redis" so the fleet's
// TXCO_KVSTORE=redis selects this store unchanged.
const StoreName = "redis"

const noExpiration = time.Duration(0)

// scanCount is the COUNT hint per SCAN call. Each SCAN call is one billed
// command and one network round trip on a managed Redis, and COUNT bounds the
// keyspace slots the server walks per call — so a full-namespace List costs
// ~total_keys/scanCount round trips. 1000 keeps per-call server work well
// inside redis's non-blocking guidance while cutting round trips ~100x vs the
// upstream default of 10. A var (not const) so tests can force multi-cursor
// paging with a handful of keys.
var scanCount = int64(1000)

// mgetChunk caps the keys fetched per MGET so a large namespace can't produce
// one unbounded command/response. A var for the same test reason as scanCount.
var mgetChunk = 500

// ErrMultipleEndpointsUnsupported is returned when more than one endpoint is
// given; the chassis KV always connects to a single redis endpoint.
var ErrMultipleEndpointsUnsupported = errors.New("redis: does not support multiple endpoints")

// registers the store with valkeyrie. This package must be the only "redis"
// registrant in the binary (Register panics on a duplicate name) — the
// upstream github.com/kvtools/redis must not be imported alongside it.
func init() {
	valkeyrie.Register(StoreName, newStore)
}

// Config is the store configuration — exactly the fields the chassis sets
// (see app.redisConfigFromAddr): TLS for rediss://, userinfo, and the
// numeric database from a redis URL path.
type Config struct {
	TLS      *tls.Config
	Username string
	Password string
	DB       int
}

func newStore(ctx context.Context, endpoints []string, options valkeyrie.Config) (store.Store, error) {
	cfg, ok := options.(*Config)
	if !ok && options != nil {
		return nil, &store.InvalidConfigurationError{Store: StoreName, Config: options}
	}
	return New(ctx, endpoints, cfg)
}

// Store implements the store.Store interface over a single redis endpoint.
type Store struct {
	client *redis.Client
	script *redis.Script
}

// New creates the redis-backed store. ctx is unused (the client dials
// lazily); it is kept for constructor-signature parity with the registry.
func New(_ context.Context, endpoints []string, options *Config) (*Store, error) {
	if len(endpoints) > 1 {
		return nil, ErrMultipleEndpointsUnsupported
	}
	opt := &redis.Options{
		DialTimeout:  5 * time.Second,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	if len(endpoints) == 1 {
		opt.Addr = endpoints[0]
	}
	if options != nil {
		opt.TLSConfig = options.TLS
		opt.Username = options.Username
		opt.Password = options.Password
		opt.DB = options.DB
	}
	return &Store{
		client: redis.NewClient(opt),
		script: redis.NewScript(casScript),
	}, nil
}

// Put a value at the specified key. opts.TTL sets a native redis expiry.
func (r *Store) Put(ctx context.Context, key string, value []byte, opts *store.WriteOptions) error {
	ttl := noExpiration
	if opts != nil && opts.TTL != 0 {
		ttl = opts.TTL
	}
	return r.client.Set(ctx, normalize(key), value, ttl).Err()
}

// Get a value given its key.
func (r *Store) Get(ctx context.Context, key string, _ *store.ReadOptions) (*store.KVPair, error) {
	nKey := normalize(key)
	reply, err := r.client.Get(ctx, nKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, store.ErrKeyNotFound
		}
		return nil, err
	}
	return &store.KVPair{Key: nKey, Value: reply}, nil
}

// Delete the value at the specified key.
func (r *Store) Delete(ctx context.Context, key string) error {
	return r.client.Del(ctx, normalize(key)).Err()
}

// Exists verifies whether a key exists in the store.
func (r *Store) Exists(ctx context.Context, key string, _ *store.ReadOptions) (bool, error) {
	count, err := r.client.Exists(ctx, normalize(key)).Result()
	return count != 0, err
}

// List the content of a given prefix. Returns store.ErrKeyNotFound when
// nothing matches (the chassis wrapper maps that to an empty namespace).
func (r *Store) List(ctx context.Context, directory string, _ *store.ReadOptions) ([]*store.KVPair, error) {
	prefix := normalize(directory)
	allKeys, err := r.keys(ctx, scanPattern(prefix))
	if err != nil {
		return nil, err
	}
	return r.mget(ctx, prefix, allKeys)
}

// DeleteTree deletes all keys under a given prefix. Not atomic: the SCAN and
// the DEL are separate commands (upstream behavior, unchanged).
func (r *Store) DeleteTree(ctx context.Context, directory string) error {
	allKeys, err := r.keys(ctx, scanPattern(normalize(directory)))
	if err != nil {
		return err
	}
	return r.client.Del(ctx, allKeys...).Err()
}

// keys SCANs the keyspace to exhaustion for the given MATCH pattern.
func (r *Store) keys(ctx context.Context, pattern string) ([]string, error) {
	var (
		all    []string
		cursor uint64
	)
	for {
		batch, next, err := r.client.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if next == 0 {
			break
		}
		cursor = next
	}
	if len(all) == 0 {
		return nil, store.ErrKeyNotFound
	}
	return all, nil
}

// mget fetches the values for keys in mgetChunk-sized batches. Keys that
// vanished since the SCAN (deleted or natively expired) come back nil and are
// skipped, as is a key exactly equal to the listed prefix itself.
func (r *Store) mget(ctx context.Context, prefix string, keys []string) ([]*store.KVPair, error) {
	var pairs []*store.KVPair
	for start := 0; start < len(keys); start += mgetChunk {
		chunk := keys[start:min(start+mgetChunk, len(keys))]
		replies, err := r.client.MGet(ctx, chunk...).Result()
		if err != nil {
			return nil, err
		}
		for i, reply := range replies {
			sreply, ok := reply.(string)
			if !ok || sreply == "" {
				continue
			}
			if chunk[i] == prefix {
				continue
			}
			pairs = append(pairs, &store.KVPair{Key: chunk[i], Value: []byte(sreply)})
		}
	}
	return pairs, nil
}

// AtomicPut is an atomic compare-and-swap on a single value. previous == nil
// means create (fails with ErrKeyExists if the key is already there);
// otherwise the swap happens only if the stored bytes still equal
// previous.Value (ErrKeyModified when they don't, ErrKeyNotFound when the
// key vanished).
func (r *Store) AtomicPut(ctx context.Context, key string, value []byte, previous *store.KVPair, opts *store.WriteOptions) (bool, *store.KVPair, error) {
	ttl := noExpiration
	if opts != nil && opts.TTL != 0 {
		ttl = opts.TTL
	}
	nKey := normalize(key)
	newKV := &store.KVPair{Key: nKey, Value: value}

	if previous == nil {
		if err := r.setNX(ctx, nKey, value, ttl); err != nil {
			return false, nil, err
		}
		return true, newKV, nil
	}

	if err := r.runScript(ctx, cmdCAS, nKey, string(previous.Value), string(value), formatSec(ttl)); err != nil {
		return false, nil, err
	}
	return true, newKV, nil
}

func (r *Store) setNX(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	ok, err := r.client.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return err
	}
	if !ok {
		return store.ErrKeyExists
	}
	return nil
}

// AtomicDelete deletes the key only if the stored bytes still equal
// previous.Value.
func (r *Store) AtomicDelete(ctx context.Context, key string, previous *store.KVPair) (bool, error) {
	if previous == nil {
		return false, store.ErrPreviousNotSpecified
	}
	if err := r.runScript(ctx, cmdCAD, normalize(key), string(previous.Value)); err != nil {
		return false, err
	}
	return true, nil
}

// Close the store connection.
func (r *Store) Close() error {
	return r.client.Close()
}

// Watch is not supported (the chassis KV never watches keys).
func (r *Store) Watch(_ context.Context, _ string, _ *store.ReadOptions) (<-chan *store.KVPair, error) {
	return nil, store.ErrCallNotSupported
}

// WatchTree is not supported (the chassis KV never watches keys).
func (r *Store) WatchTree(_ context.Context, _ string, _ *store.ReadOptions) (<-chan []*store.KVPair, error) {
	return nil, store.ErrCallNotSupported
}

// NewLock is not supported (the chassis KV never takes distributed locks).
func (r *Store) NewLock(_ context.Context, _ string, _ *store.LockOptions) (store.Locker, error) {
	return nil, store.ErrCallNotSupported
}

func (r *Store) runScript(ctx context.Context, args ...interface{}) error {
	err := r.script.Run(ctx, r.client, nil, args...).Err()
	if err != nil && strings.Contains(err.Error(), "redis: key is not found") {
		return store.ErrKeyNotFound
	}
	if err != nil && strings.Contains(err.Error(), "redis: value has been changed") {
		return store.ErrKeyModified
	}
	return err
}

func scanPattern(prefix string) string {
	return prefix + "*"
}

func normalize(key string) string {
	return strings.TrimPrefix(key, "/")
}

func formatSec(dur time.Duration) string {
	return fmt.Sprintf("%d", int(dur/time.Second))
}
