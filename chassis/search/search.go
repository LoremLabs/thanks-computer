// Package search is the chassis-owned lexical search store: a durable,
// tenant-scoped place to index text records and find them again by the words
// they contain, exposed to txcl as txco://search/*.
//
// **Separation of concerns.** This package does lexical retrieval and nothing
// else. It stores no embeddings and never talks to a model. Semantic retrieval
// is chassis/vector's job; fusing the two lanes belongs to the stack, above
// both primitives.
//
// **The collection is the ranking unit.** Term statistics are local to one
// (tenant, collection), so one collection's documents never move another's
// ranking. A query addresses exactly one collection.
//
// **One filter grammar.** Filter is the vector store's Filter, by alias, so a
// stack narrows both retrieval lanes with the same object and the two cannot
// drift.
package search

import (
	"context"
	"fmt"
	"sort"

	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// Filter narrows a query, a delete or an update by metadata. It is the vector
// store's grammar (eq, in, not_in, gte, lte, gt, lt; conjunction), including
// its two rules: the special field "id" addresses the record id, and an absent
// metadata field passes not_in.
type Filter = vector.Filter

// Condition is one Filter clause.
type Condition = vector.Condition

// Item is one searchable record. ID is unique within a collection; an upsert
// of an existing ID replaces the record. Text, Title, Heading, Name and
// Entities are searched, and a backend may weight them differently. Metadata
// is filtered on, never searched.
type Item struct {
	ID       string         `json:"id"`
	Text     string         `json:"text,omitempty"`
	Title    string         `json:"title,omitempty"`
	Heading  string         `json:"heading,omitempty"`
	Name     string         `json:"name,omitempty"`
	Entities []string       `json:"entities,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Hit is one query result. Rank is 1 for the best hit. No score is exposed:
// the contract is "best first", and a raw engine score is not portable across
// analyzer changes, backends or shards. Text is returned because a lexical hit
// may be absent from the vector lane's results, and the stack needs its body.
type Hit struct {
	ID       string         `json:"id"`
	Rank     int            `json:"rank"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Collection is one lexical corpus: the ranking unit, the physical index unit,
// and the unit a query addresses. AnalyzerVersion and ScoringModel are pinned
// when the collection is created. Changing either means re-indexing it.
type Collection struct {
	Name            string `json:"name"`
	AnalyzerVersion string `json:"analyzer_version"`
	ScoringModel    string `json:"scoring_model"`
	// Records is filled by DescribeCollection and ListCollections.
	Records uint64 `json:"records"`
}

// Selector names the records a Delete removes: IDs, or a Filter with at least
// one condition. Exactly one of the two is set.
type Selector struct {
	IDs    []string `json:"ids,omitempty"`
	Filter Filter   `json:"filter,omitempty"`
}

// Change is what an Update does to each record its filter admits. Merge is a
// shallow metadata merge, RFC 7396 style: a key replaces the record's key, and
// a nil value removes it. Fields replaces short searched fields. Text is never
// changed by an update: new text is a new upsert.
type Change struct {
	Merge  map[string]any `json:"merge,omitempty"`
	Fields *FieldSet      `json:"fields,omitempty"`
}

// FieldSet replaces the short searched fields it names. A nil field is left
// alone; a pointer to "" clears it.
type FieldSet struct {
	Name    *string `json:"name,omitempty"`
	Title   *string `json:"title,omitempty"`
	Heading *string `json:"heading,omitempty"`
}

// Empty reports whether the change would alter nothing.
func (c Change) Empty() bool {
	return len(c.Merge) == 0 && (c.Fields == nil || (c.Fields.Name == nil && c.Fields.Title == nil && c.Fields.Heading == nil))
}

// CountPending is returned in place of a count by a backend that accepted a
// Delete or an Update but has not applied it yet. The bundled backend applies
// synchronously and never returns it.
const CountPending = -1

// MutationOp names a write.
type MutationOp string

const (
	OpEnsure MutationOp = "ensure"
	OpUpsert MutationOp = "upsert"
	OpDelete MutationOp = "delete"
	OpUpdate MutationOp = "update"
	OpDrop   MutationOp = "drop"
)

// Mutation is one write to one collection, in a form that does not depend on
// how it arrived. Every write a Store accepts reduces to one, so a backend has
// a single apply path, and a backend that ships writes elsewhere ships exactly
// what the far side applies.
type Mutation struct {
	Op         MutationOp `json:"op"`
	Collection Collection `json:"collection,omitempty"` // ensure
	Items      []Item     `json:"items,omitempty"`      // upsert
	Select     Selector   `json:"select,omitempty"`     // delete
	Filter     Filter     `json:"filter,omitempty"`     // update
	Change     Change     `json:"change,omitempty"`     // update
}

// Store is the backend-agnostic lexical search interface. Implementations must
// be safe for concurrent use. tenant scopes every call (collections are
// namespaced by (tenant, name)); the op layer pins it from TenantScope, never
// from the envelope.
type Store interface {
	// EnsureCollection creates the collection if absent and is a no-op if it
	// exists. c.AnalyzerVersion and c.ScoringModel may be left empty to take
	// the backend's; a value that differs from an existing collection's pin,
	// or that the backend cannot provide, returns AnalyzerMismatchError.
	EnsureCollection(ctx context.Context, tenant string, c Collection) error

	// DescribeCollection returns the pinned collection and its record count.
	// found is false (nil error) when it doesn't exist.
	DescribeCollection(ctx context.Context, tenant, name string) (c Collection, found bool, err error)

	// Upsert inserts or replaces items by ID, all or nothing. Returns the
	// number of items written.
	Upsert(ctx context.Context, tenant, collection string, items []Item) (int, error)

	// Query returns up to limit hits for q among the records filter admits,
	// best first. q is plain language: a quoted string is a phrase, an
	// identifier is matched whole, and the rest is OR-ed. No engine syntax is
	// recognised. An empty result is not an error.
	Query(ctx context.Context, tenant, collection, q string, limit int, filter Filter) ([]Hit, error)

	// Delete removes the records sel names. Returns the number removed, or
	// CountPending.
	Delete(ctx context.Context, tenant, collection string, sel Selector) (int, error)

	// Update applies ch to every record filter admits, leaving text alone.
	// filter must carry at least one condition: an unconditional rewrite is
	// refused with InvalidArgError. The filter is evaluated when the update is
	// applied. Returns the number of records matched, or CountPending.
	Update(ctx context.Context, tenant, collection string, filter Filter, ch Change) (int, error)

	// ListCollections returns the tenant's collections, sorted by name. Not a
	// hot path.
	ListCollections(ctx context.Context, tenant string) ([]Collection, error)

	// DropCollection removes a collection and all its records, returning the
	// number of records removed. A missing collection is not an error
	// (returns 0).
	DropCollection(ctx context.Context, tenant, name string) (int, error)

	// Shared reports whether this backend is shared across nodes, versus
	// per-node. The bundled backend is per-node (returns false).
	Shared() bool

	// Close flushes and releases the backend. A search index must be closed.
	Close() error
}

// Config carries backend-selecting options resolved from chassis config. A
// backend reads only the fields it needs. Adding a field here doesn't affect
// existing backends (same posture as vector.Config).
type Config struct {
	// Path is the bundled backend's root directory (--search-path).
	Path string
	// MaxOpenIndexes bounds how many collection indexes the bundled backend
	// holds open at once (--search-max-open-indexes). Zero takes its default.
	MaxOpenIndexes int
}

// Constructor builds a Store from resolved config.
type Constructor func(Config) (Store, error)

// StoreNone is the --search-store value that turns lexical search off. Open
// returns a nil Store for it, and the ops answer txco_search_disabled.
const StoreNone = "none"

// registry maps backend name → constructor. The bundled "bleve" backend
// registers itself (chassis/search/blevestore init()); additional backends
// register the same way.
var registry = map[string]Constructor{}

// Register adds a backend constructor. Called from a backend package's init().
func Register(name string, c Constructor) {
	registry[name] = c
}

// Open constructs the named backend. StoreNone returns (nil, nil). An unknown
// name is a startup error listing what is available (so a misconfigured
// --search-store fails loudly).
func Open(name string, cfg Config) (Store, error) {
	if name == StoreNone {
		return nil, nil
	}
	c, ok := registry[name]
	if !ok {
		avail := make([]string, 0, len(registry)+1)
		for k := range registry {
			avail = append(avail, k)
		}
		avail = append(avail, StoreNone)
		sort.Strings(avail)
		return nil, fmt.Errorf("search: unknown store %q (available: %v)", name, avail)
	}
	return c(cfg)
}
