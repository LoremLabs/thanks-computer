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

import "github.com/loremlabs/thanks-computer/chassis/vector"

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
