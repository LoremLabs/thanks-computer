package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
)

// DirectoryNamespace is the chassis-reserved KV namespace that records which
// node owns each session: key = session id, scoped per tenant like every KV
// key (<tenant>/_txc.websocket/<sid>), so a lookup with the wrong tenant
// simply misses — no cross-tenant existence oracle, structurally. Author-
// facing writers (txco://kv/*, KV/ seed packs) refuse `_txc.*` namespaces,
// the same reservation the blob index relies on.
const DirectoryNamespace = "_txc.websocket"

// Directory maps (tenant, session id) → the node whose inbox reaches the
// session's socket. Entries are leases: written on open, refreshed by the
// owning node while the session lives, deleted on close, and expired by the
// store if the node dies — so a stale entry can only ever point at a node
// that no longer answers, which the Relay reports as not found.
type Directory interface {
	Register(ctx context.Context, tenant, sid, node, stack string) error
	Refresh(ctx context.Context, tenant, sid, node, stack string) error
	Unregister(ctx context.Context, tenant, sid string) error
	// Lookup answers the owning node's address; found=false is a miss.
	Lookup(ctx context.Context, tenant, sid string) (node string, found bool, err error)
}

// directoryEntry is the stored value.
type directoryEntry struct {
	Node  string `json:"node"`
	Stack string `json:"stack,omitempty"`
	At    string `json:"at"`
}

// kvDirectory keeps the map in the chassis KV store. It only makes sense on
// a SHARED store (--kvstore=redis); the boot wiring refuses a node-local one.
type kvDirectory struct {
	kv  *kvstore.KV
	ttl time.Duration
	now func() time.Time
}

// NewKVDirectory builds a Directory over a KV handle. Hand it a handle built
// with no TTL clamp (kvstore.New(store, 0, 0)): --kv-max-ttl guards authors'
// keys and must not shorten a session lease.
func NewKVDirectory(kv *kvstore.KV, ttl time.Duration) Directory {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &kvDirectory{kv: kv, ttl: ttl, now: time.Now}
}

func (d *kvDirectory) put(ctx context.Context, tenant, sid, node, stack string) error {
	if d.kv == nil {
		return errors.New("websocket directory: no kv store")
	}
	raw, err := json.Marshal(directoryEntry{Node: node, Stack: stack, At: d.now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	return d.kv.Set(ctx, tenant, DirectoryNamespace, sid, raw, d.ttl)
}

func (d *kvDirectory) Register(ctx context.Context, tenant, sid, node, stack string) error {
	return d.put(ctx, tenant, sid, node, stack)
}

func (d *kvDirectory) Refresh(ctx context.Context, tenant, sid, node, stack string) error {
	return d.put(ctx, tenant, sid, node, stack)
}

func (d *kvDirectory) Unregister(ctx context.Context, tenant, sid string) error {
	if d.kv == nil {
		return errors.New("websocket directory: no kv store")
	}
	return d.kv.Delete(ctx, tenant, DirectoryNamespace, sid)
}

func (d *kvDirectory) Lookup(ctx context.Context, tenant, sid string) (string, bool, error) {
	if d.kv == nil {
		return "", false, errors.New("websocket directory: no kv store")
	}
	raw, found, err := d.kv.Get(ctx, tenant, DirectoryNamespace, sid)
	if err != nil || !found {
		return "", false, err
	}
	var e directoryEntry
	if err := json.Unmarshal(raw, &e); err != nil || e.Node == "" {
		return "", false, nil // an unreadable entry is a miss, never an oracle
	}
	return e.Node, true, nil
}
