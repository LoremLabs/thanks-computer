// Package vector is the chassis-owned vector store: a durable, tenant-scoped
// place to upsert embeddings and find nearest neighbours, exposed to txcl as
// txco://vector/{upsert,search,delete,collection}.
//
// **Separation of concerns.** This package stores and searches vectors and
// nothing else — it never talks to an embedding provider. Producing the
// vectors is ai://embed's job (chassis/embed). The split mirrors the design:
// embedding belongs to AI; vector storage/retrieval belongs to infrastructure.
//
// **Durable and bounded, not in-RAM.** A platform store must not hold tenants'
// embeddings in process memory — a restart would reload everything and a large
// upload would OOM the node. Vectors live on disk in a dedicated store (the
// bundled backend is SQLite + sqlite-vec; a pgvector backend slots in behind
// this same interface for HA/scale). A search is a local index read, the store
// doing its job — like txco://kv hitting BoltDB.
//
// **v1 is exact, not approximate.** The bundled backend brute-force-scans with
// sqlite-vec's distance functions and pushes metadata filters into the SQL
// WHERE before ranking. No ANN, no recall tuning. Fine into the ~100k-vector
// range; the crossover to ANN/pgvector is HA or scale, not catalog size.
package vector

import "context"

// Metric is the distance metric a collection is compared under. v1 supports
// cosine only; the field exists so a collection pins it and a future backend
// can honour l2/ip without a schema change.
type Metric string

const (
	MetricCosine Metric = "cosine"
)

// Collection pins a vector space. Vectors are only comparable within one
// (embedding model, dimensions, metric); the store rejects upserts that don't
// match, so a model swap is a new collection + re-embed, never silent space
// mixing.
type Collection struct {
	Name           string `json:"name"`
	EmbeddingModel string `json:"embedding_model"`
	Dimensions     int    `json:"dimensions"`
	Metric         Metric `json:"metric"`
}

// Item is one vector to store. ID is unique within (tenant, collection);
// re-upserting an ID replaces it. Metadata is the filterable sidecar; Text is
// optional and returned on search so callers avoid a second lookup.
type Item struct {
	ID       string         `json:"id"`
	Vector   []float32      `json:"vector"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Text     string         `json:"text,omitempty"`
}

// Match is one search hit. Distance is the raw metric distance (cosine
// distance ∈ [0,2]); Score is the normalised similarity 1−distance (cosine
// similarity ∈ [-1,1], higher = closer) so rerankers get a clean signal.
type Match struct {
	ID       string         `json:"id"`
	Distance float64        `json:"distance"`
	Score    float64        `json:"score"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Text     string         `json:"text,omitempty"`
}

// Op is a filter comparison operator.
type Op string

const (
	OpEq    Op = "eq"
	OpIn    Op = "in"
	OpNotIn Op = "not_in"
	OpGte   Op = "gte"
	OpLte   Op = "lte"
	OpGt    Op = "gt"
	OpLt    Op = "lt"
)

// Condition is one metadata predicate. Value is a scalar for eq/gte/lte/gt/lt
// and a slice for in/not_in.
type Condition struct {
	Field string
	Op    Op
	Value any
}

// Filter is a conjunction (AND) of conditions applied to item metadata, pushed
// into the store's WHERE clause before nearest-neighbour ranking — so a tight
// filter both narrows results and avoids the "ANN returns 10, filter leaves 2"
// recall trap. An empty Filter matches everything.
type Filter struct {
	Conditions []Condition
}

// Store is the backend-agnostic vector store interface. Implementations must
// be safe for concurrent use. tenant scopes every call (collections are
// namespaced by (tenant, name)); the op layer pins it from TenantScope, never
// from the envelope.
type Store interface {
	// EnsureCollection creates the collection if absent (pinning model, dims,
	// metric) and is a no-op if it already exists with a compatible pin;
	// a conflicting pin (different dims/model/metric) returns CollectionConflictError.
	EnsureCollection(ctx context.Context, tenant string, c Collection) error

	// DescribeCollection returns the pinned collection. found is false (nil
	// error) when it doesn't exist.
	DescribeCollection(ctx context.Context, tenant, name string) (c Collection, found bool, err error)

	// Upsert inserts or replaces items by ID. Every vector's length must equal
	// the collection's pinned dimension (else DimensionMismatchError). Returns
	// the number of items written.
	Upsert(ctx context.Context, tenant, collection string, items []Item) (int, error)

	// Search returns up to limit nearest matches to query under the
	// collection's metric, restricted to items satisfying filter. query's
	// length must equal the pinned dimension.
	Search(ctx context.Context, tenant, collection string, query []float32, limit int, filter Filter) ([]Match, error)

	// Delete removes items by ID. Returns the number removed.
	Delete(ctx context.Context, tenant, collection string, ids []string) (int, error)

	// ListIDs returns the IDs of every item currently in the collection (order
	// unspecified). The store-seed reconciler uses it to compute which managed
	// items a re-applied pack dropped (a seeded collection is owned by its pack,
	// so reconcile is a desired-state sync: store − pack → delete). Returns
	// CollectionNotFoundError if the collection doesn't exist.
	ListIDs(ctx context.Context, tenant, collection string) ([]string, error)

	// ListCollections returns the tenant's collections (pins only, no items),
	// sorted by name — the inspect surface (`txco vector ls`). Not a hot path.
	ListCollections(ctx context.Context, tenant string) ([]Collection, error)

	// DropCollection removes a collection and all its items, returning the
	// number of items removed. A missing collection is not an error (returns 0).
	// This is the explicit whole-collection teardown the store-seed reconciler
	// deliberately does NOT perform automatically (removing a pack file stops
	// managing a collection but doesn't destroy it).
	DropCollection(ctx context.Context, tenant, name string) (int, error)

	// Close releases the underlying handle.
	Close() error
}
