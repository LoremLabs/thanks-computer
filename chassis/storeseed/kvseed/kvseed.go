// Package kvseed is the KV store-seed Materializer: it reconciles
// KV/<namespace>.jsonl packs (chassis/storeseed) into the tenant KV store.
//
// A pack is NDJSON, one item per line:
//
//	{"key":"…","value":<any JSON>,"ttl":<seconds, optional>}
//
// Each pack OWNS its namespace (managed scope): reconcile sets every item and
// deletes any key in the namespace absent from the pack (desired-state sync).
//
// **Managed-scope hazard (read this).** The KV namespace for a runtime
// txco://kv/* op defaults to the routed STACK name. A seeded namespace is
// delete-missing'd against its pack, so it must be a namespace NO runtime op
// writes — never the stack's own name and never a runtime namespace (driplit's
// peek/library/session). Keep seed config in a dedicated namespace (e.g.
// KV/config.jsonl) so reconcile never wipes runtime state.
//
// TTL on a seed means the key self-expires between applies (re-set on each
// re-apply); persistent config omits ttl.
package kvseed

import (
	"context"
	"encoding/json"
	"fmt"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// Item is one line of a KV pack.
type Item struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
	TTL   int64           `json:"ttl,omitempty"` // seconds; 0/absent = persistent
}

// Materializer reconciles KV packs into a *kv.KV.
type Materializer struct {
	kv     *kvstore.KV
	shared bool
}

// New builds the KV Materializer. shared declares whether the KV backend is
// fleet-shared (redis) — reconciled once on the origin — or per-node (boltdb) —
// reconciled on every node. The wiring layer (server.go) knows the backend.
func New(kv *kvstore.KV, shared bool) *Materializer {
	return &Materializer{kv: kv, shared: shared}
}

func (m *Materializer) Kind() string { return storeseed.KindKV }
func (m *Materializer) Shared() bool { return m.shared }

func (m *Materializer) Reconcile(ctx context.Context, scope storeseed.Scope, packs []storeseed.RawPack) error {
	for _, p := range packs {
		if err := m.reconcileOne(ctx, scope, p); err != nil {
			return fmt.Errorf("namespace %q: %w", p.Name, err)
		}
	}
	return nil
}

func (m *Materializer) reconcileOne(ctx context.Context, scope storeseed.Scope, p storeseed.RawPack) error {
	items, err := parsePack(p)
	if err != nil {
		return err
	}

	keep := make(map[string]struct{}, len(items))
	for _, it := range items {
		if err := m.kv.Set(ctx, scope.Tenant, p.Name, it.Key, it.Value, kvstore.ParseTTLSeconds(it.TTL)); err != nil {
			return fmt.Errorf("set %q: %w", it.Key, err)
		}
		keep[it.Key] = struct{}{}
	}

	// Delete-missing: the pack is the desired state, so any namespace key not in
	// the pack is a managed key the new version dropped.
	current, err := m.kv.ListKeys(ctx, scope.Tenant, p.Name)
	if err != nil {
		return fmt.Errorf("list keys: %w", err)
	}
	for _, key := range current {
		if _, ok := keep[key]; ok {
			continue
		}
		if err := m.kv.Delete(ctx, scope.Tenant, p.Name, key); err != nil {
			return fmt.Errorf("delete stale %q: %w", key, err)
		}
	}
	return nil
}

// parsePack decodes a pack's NDJSON into Items. An empty/missing key, invalid
// JSON value, or duplicate key is a hard error — a malformed pack must fail
// loudly rather than seed a corrupt namespace.
func parsePack(p storeseed.RawPack) ([]Item, error) {
	var items []Item
	seen := map[string]struct{}{}
	for i, line := range p.Lines() {
		var it Item
		if err := json.Unmarshal(line, &it); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if it.Key == "" {
			return nil, fmt.Errorf("line %d: missing key", i+1)
		}
		if _, dup := seen[it.Key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", i+1, it.Key)
		}
		seen[it.Key] = struct{}{}
		if len(it.Value) == 0 || !json.Valid(it.Value) {
			return nil, fmt.Errorf("line %d (key %q): value is not valid JSON", i+1, it.Key)
		}
		items = append(items, it)
	}
	return items, nil
}
