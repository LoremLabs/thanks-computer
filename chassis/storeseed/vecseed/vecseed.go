// Package vecseed is the vector store-seed Materializer: it reconciles
// VECTORS/<collection>.jsonl packs (chassis/storeseed) into a vector.Store.
//
// A pack is NDJSON, one item per line:
//
//	{"id":"…","vector":[…pre-computed floats…],"metadata":{…},"text":"…","model":"…"}
//
// Vectors are PRE-COMPUTED at build time (apply must stay offline + deterministic
// and N data-plane nodes must not each re-embed). The collection's dimension is
// derived from the vectors; its embedding model is the optional per-item "model"
// (first non-empty wins) — it documents what the query path must embed with so
// query vectors stay comparable. Each pack OWNS its collection: reconcile upserts
// every item and deletes any store item absent from the pack (desired-state sync).
package vecseed

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// Item is one line of a VECTORS pack. It mirrors vector.Item plus an optional
// per-pack embedding-model declaration used to pin the collection.
type Item struct {
	ID       string         `json:"id"`
	Vector   []float32      `json:"vector"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Text     string         `json:"text,omitempty"`
	Model    string         `json:"model,omitempty"`
}

// Materializer reconciles VECTORS packs into a vector.Store.
type Materializer struct {
	store  vector.Store
	shared bool
}

// New builds the vector Materializer. shared declares whether store is a
// fleet-shared backend (pgvector) — reconciled once on the origin — or per-node
// (sqlite-vec) — reconciled on every node. The wiring layer (server.go) knows
// which backend it opened and passes the answer.
func New(store vector.Store, shared bool) *Materializer {
	return &Materializer{store: store, shared: shared}
}

func (m *Materializer) Kind() string { return storeseed.KindVector }
func (m *Materializer) Shared() bool { return m.shared }

func (m *Materializer) Reconcile(ctx context.Context, scope storeseed.Scope, packs []storeseed.RawPack) error {
	for _, p := range packs {
		if err := m.reconcileOne(ctx, scope, p); err != nil {
			return fmt.Errorf("collection %q: %w", p.Name, err)
		}
	}
	return nil
}

func (m *Materializer) reconcileOne(ctx context.Context, scope storeseed.Scope, p storeseed.RawPack) error {
	items, dims, model, err := parsePack(p)
	if err != nil {
		return err
	}

	// Empty pack: the collection (if any) should hold nothing. We can't derive
	// dimensions to (re)create it, so only empty an EXISTING collection; a never-
	// created one stays absent.
	if len(items) == 0 {
		return m.emptyExisting(ctx, scope.Tenant, p.Name)
	}

	if err := m.store.EnsureCollection(ctx, scope.Tenant, vector.Collection{
		Name:           p.Name,
		EmbeddingModel: model,
		Dimensions:     dims,
		Metric:         vector.MetricCosine,
	}); err != nil {
		return fmt.Errorf("ensure collection: %w", err)
	}

	if _, err := m.store.Upsert(ctx, scope.Tenant, p.Name, items); err != nil {
		return fmt.Errorf("upsert %d items: %w", len(items), err)
	}

	// Delete-missing: the pack is the desired state, so any store item not in
	// the pack is a managed item the new version dropped.
	keep := make(map[string]struct{}, len(items))
	for _, it := range items {
		keep[it.ID] = struct{}{}
	}
	current, err := m.store.ListIDs(ctx, scope.Tenant, p.Name)
	if err != nil {
		return fmt.Errorf("list ids: %w", err)
	}
	var stale []string
	for _, id := range current {
		if _, ok := keep[id]; !ok {
			stale = append(stale, id)
		}
	}
	if len(stale) > 0 {
		if _, err := m.store.Delete(ctx, scope.Tenant, p.Name, stale); err != nil {
			return fmt.Errorf("delete %d stale items: %w", len(stale), err)
		}
	}
	return nil
}

// emptyExisting deletes every item from an existing collection, leaving the
// (empty) collection in place. A not-yet-created collection is a no-op.
func (m *Materializer) emptyExisting(ctx context.Context, tenant, name string) error {
	_, found, err := m.store.DescribeCollection(ctx, tenant, name)
	if err != nil {
		return fmt.Errorf("describe collection: %w", err)
	}
	if !found {
		return nil
	}
	ids, err := m.store.ListIDs(ctx, tenant, name)
	if err != nil {
		return fmt.Errorf("list ids: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := m.store.Delete(ctx, tenant, name, ids); err != nil {
		return fmt.Errorf("empty collection: %w", err)
	}
	return nil
}

// parsePack decodes a pack's NDJSON into vector.Items, deriving the collection
// dimension from the vectors (all must match) and the embedding model from the
// first non-empty per-item "model". A zero-length or missing vector, or a
// duplicate/empty id, is a hard error — a malformed pack must fail loudly rather
// than seed a corrupt collection.
func parsePack(p storeseed.RawPack) (items []vector.Item, dims int, model string, err error) {
	seen := map[string]struct{}{}
	for i, line := range p.Lines() {
		var it Item
		if uerr := json.Unmarshal(line, &it); uerr != nil {
			return nil, 0, "", fmt.Errorf("line %d: %w", i+1, uerr)
		}
		if it.ID == "" {
			return nil, 0, "", fmt.Errorf("line %d: missing id", i+1)
		}
		if _, dup := seen[it.ID]; dup {
			return nil, 0, "", fmt.Errorf("line %d: duplicate id %q", i+1, it.ID)
		}
		seen[it.ID] = struct{}{}
		if len(it.Vector) == 0 {
			return nil, 0, "", fmt.Errorf("line %d (id %q): empty vector", i+1, it.ID)
		}
		if dims == 0 {
			dims = len(it.Vector)
		} else if len(it.Vector) != dims {
			return nil, 0, "", fmt.Errorf("line %d (id %q): vector length %d != collection dimension %d",
				i+1, it.ID, len(it.Vector), dims)
		}
		if model == "" && it.Model != "" {
			model = it.Model
		}
		items = append(items, vector.Item{ID: it.ID, Vector: it.Vector, Metadata: it.Metadata, Text: it.Text})
	}
	return items, dims, model, nil
}
