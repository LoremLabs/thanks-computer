// Package vectortest holds the backend-agnostic conformance suite for
// vector.Store implementations. The sqlite-vec backend runs it today; the
// pgvector backend (Phase 4) reuses it unchanged, which is what keeps the two
// backends behaviourally identical behind the interface.
package vectortest

import (
	"context"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// RunConformance exercises the full vector.Store contract against a fresh
// store produced by newStore. newStore is called once per RunConformance.
func RunConformance(t *testing.T, newStore func(t *testing.T) vector.Store) {
	t.Helper()
	ctx := context.Background()
	const tenant = "acme"
	const coll = "books"
	const dim = 4

	s := newStore(t)
	t.Cleanup(func() { _ = s.Close() })

	mustEnsure := func() {
		if err := s.EnsureCollection(ctx, tenant, vector.Collection{
			Name: coll, EmbeddingModel: "test-model", Dimensions: dim, Metric: vector.MetricCosine,
		}); err != nil {
			t.Fatalf("EnsureCollection: %v", err)
		}
	}

	t.Run("EnsureAndDescribe", func(t *testing.T) {
		mustEnsure()
		got, found, err := s.DescribeCollection(ctx, tenant, coll)
		if err != nil || !found {
			t.Fatalf("describe: found=%v err=%v", found, err)
		}
		if got.Dimensions != dim || got.EmbeddingModel != "test-model" || got.Metric != vector.MetricCosine {
			t.Fatalf("describe pin wrong: %+v", got)
		}
		// idempotent re-ensure is fine
		mustEnsure()
	})

	t.Run("ConflictOnDifferentDim", func(t *testing.T) {
		err := s.EnsureCollection(ctx, tenant, vector.Collection{Name: coll, Dimensions: dim + 1, Metric: vector.MetricCosine})
		if _, ok := err.(*vector.CollectionConflictError); !ok {
			t.Fatalf("want CollectionConflictError, got %T %v", err, err)
		}
	})

	t.Run("MissingCollection", func(t *testing.T) {
		if _, err := s.Upsert(ctx, tenant, "nope", []vector.Item{{ID: "x", Vector: make([]float32, dim)}}); err == nil {
			t.Fatal("upsert to missing collection: want error")
		} else if _, ok := err.(*vector.CollectionNotFoundError); !ok {
			t.Fatalf("want CollectionNotFoundError, got %T", err)
		}
	})

	t.Run("UpsertSearchFilterDelete", func(t *testing.T) {
		mustEnsure()
		items := []vector.Item{
			{ID: "a", Vector: []float32{1, 0, 0, 0}, Text: "alpha",
				Metadata: map[string]any{"genre": "adventure", "age": 9, "pd": true}},
			{ID: "b", Vector: []float32{0, 1, 0, 0}, Text: "beta",
				Metadata: map[string]any{"genre": "cozy", "age": 50, "pd": false}},
			{ID: "c", Vector: []float32{0.9, 0.1, 0, 0}, Text: "gamma",
				Metadata: map[string]any{"genre": "adventure", "age": 12, "pd": true}},
		}
		if n, err := s.Upsert(ctx, tenant, coll, items); err != nil || n != 3 {
			t.Fatalf("upsert: n=%d err=%v", n, err)
		}

		// nearest to a's direction → a then c then b
		got, err := s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10, vector.Filter{})
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		if len(got) != 3 || got[0].ID != "a" || got[1].ID != "c" || got[2].ID != "b" {
			t.Fatalf("ranking wrong: %v", ids(got))
		}
		if got[0].Text != "alpha" || got[0].Metadata["genre"] != "adventure" {
			t.Fatalf("hit payload wrong: %+v", got[0])
		}
		if got[0].Score < got[2].Score {
			t.Fatalf("score should decrease with distance: %v", scores(got))
		}

		// eq filter
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10,
			vector.Filter{Conditions: []vector.Condition{{Field: "genre", Op: vector.OpEq, Value: "adventure"}}})
		if !sameSet(ids(got), []string{"a", "c"}) {
			t.Fatalf("eq filter: %v want [a c]", ids(got))
		}

		// in filter
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10,
			vector.Filter{Conditions: []vector.Condition{{Field: "genre", Op: vector.OpIn, Value: []any{"cozy"}}}})
		if !sameSet(ids(got), []string{"b"}) {
			t.Fatalf("in filter: %v want [b]", ids(got))
		}

		// not_in on a metadata field
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10,
			vector.Filter{Conditions: []vector.Condition{{Field: "genre", Op: vector.OpNotIn, Value: []any{"cozy"}}}})
		if !sameSet(ids(got), []string{"a", "c"}) {
			t.Fatalf("not_in filter: %v want [a c]", ids(got))
		}

		// not_in on the id column — the "exclude already-seen" pattern
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10,
			vector.Filter{Conditions: []vector.Condition{{Field: "id", Op: vector.OpNotIn, Value: []any{"a"}}}})
		if !sameSet(ids(got), []string{"b", "c"}) {
			t.Fatalf("id not_in filter: %v want [b c]", ids(got))
		}

		// numeric range
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10,
			vector.Filter{Conditions: []vector.Condition{{Field: "age", Op: vector.OpGte, Value: 12}}})
		if !sameSet(ids(got), []string{"b", "c"}) {
			t.Fatalf("gte filter: %v want [b c]", ids(got))
		}

		// idempotent upsert: replace c, narrow result count stays 3
		if _, err := s.Upsert(ctx, tenant, coll, []vector.Item{
			{ID: "c", Vector: []float32{0, 0, 1, 0}, Metadata: map[string]any{"genre": "scifi"}},
		}); err != nil {
			t.Fatalf("re-upsert: %v", err)
		}
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10, vector.Filter{})
		if len(got) != 3 {
			t.Fatalf("idempotent upsert changed count: %d", len(got))
		}

		// delete
		if n, err := s.Delete(ctx, tenant, coll, []string{"a", "b"}); err != nil || n != 2 {
			t.Fatalf("delete: n=%d err=%v", n, err)
		}
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10, vector.Filter{})
		if !sameSet(ids(got), []string{"c"}) {
			t.Fatalf("after delete: %v want [c]", ids(got))
		}
	})

	t.Run("DimensionMismatch", func(t *testing.T) {
		mustEnsure()
		if _, err := s.Upsert(ctx, tenant, coll, []vector.Item{{ID: "z", Vector: []float32{1, 2}}}); err == nil {
			t.Fatal("upsert wrong dim: want error")
		} else if _, ok := err.(*vector.DimensionMismatchError); !ok {
			t.Fatalf("want DimensionMismatchError, got %T", err)
		}
		if _, err := s.Search(ctx, tenant, coll, []float32{1, 2}, 10, vector.Filter{}); err == nil {
			t.Fatal("search wrong dim: want error")
		}
	})

	t.Run("TenantIsolation", func(t *testing.T) {
		mustEnsure() // acme/books
		const other = "globex"
		if err := s.EnsureCollection(ctx, other, vector.Collection{Name: coll, Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("ensure other tenant: %v", err)
		}
		if _, err := s.Upsert(ctx, other, coll, []vector.Item{{ID: "only-globex", Vector: []float32{1, 0, 0, 0}}}); err != nil {
			t.Fatalf("upsert other: %v", err)
		}
		// acme must not see globex's item
		got, _ := s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10, vector.Filter{})
		for _, m := range got {
			if m.ID == "only-globex" {
				t.Fatal("tenant isolation breached: acme saw globex's item")
			}
		}
		// globex sees only its own
		got, _ = s.Search(ctx, other, coll, []float32{1, 0, 0, 0}, 10, vector.Filter{})
		if !sameSet(ids(got), []string{"only-globex"}) {
			t.Fatalf("globex search: %v want [only-globex]", ids(got))
		}
	})
}

func ids(ms []vector.Match) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func scores(ms []vector.Match) []float64 {
	out := make([]float64, len(ms))
	for i, m := range ms {
		out[i] = m.Score
	}
	return out
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		if seen[w] == 0 {
			return false
		}
		seen[w]--
	}
	return true
}
