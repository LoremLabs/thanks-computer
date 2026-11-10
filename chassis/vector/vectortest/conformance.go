// Package vectortest holds the backend-agnostic conformance suite for
// vector.Store implementations. The sqlite-vec backend runs it today; the
// pgvector backend (Phase 4) reuses it unchanged, which is what keeps the two
// backends behaviourally identical behind the interface.
package vectortest

import (
	"context"
	"fmt"
	"strings"
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

		// ListIDs sees all three before the delete (the reconciler's
		// desired-state input).
		if all, err := s.ListIDs(ctx, tenant, coll); err != nil || !sameSet(all, []string{"a", "b", "c"}) {
			t.Fatalf("ListIDs before delete: %v err=%v want [a b c]", all, err)
		}

		// delete
		if n, err := s.Delete(ctx, tenant, coll, []string{"a", "b"}); err != nil || n != 2 {
			t.Fatalf("delete: n=%d err=%v", n, err)
		}
		got, _ = s.Search(ctx, tenant, coll, []float32{1, 0, 0, 0}, 10, vector.Filter{})
		if !sameSet(ids(got), []string{"c"}) {
			t.Fatalf("after delete: %v want [c]", ids(got))
		}
		// ListIDs reflects the delete.
		if all, err := s.ListIDs(ctx, tenant, coll); err != nil || !sameSet(all, []string{"c"}) {
			t.Fatalf("ListIDs after delete: %v err=%v want [c]", all, err)
		}
	})

	t.Run("UpdateMetadata", func(t *testing.T) {
		const uc = "labels"
		if err := s.EnsureCollection(ctx, tenant, vector.Collection{Name: uc, Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		if _, err := s.Upsert(ctx, tenant, uc, []vector.Item{
			{ID: "p", Vector: []float32{1, 0, 0, 0}, Text: "keep-p", Metadata: map[string]any{"classification": "guest", "doc": "d1"}},
			{ID: "q", Vector: []float32{0, 1, 0, 0}, Text: "keep-q", Metadata: map[string]any{"classification": "guest", "doc": "d2"}},
			{ID: "r", Vector: []float32{0, 0, 1, 0}, Text: "keep-r", Metadata: map[string]any{"classification": "internal", "doc": "d3"}},
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// Re-label the guest items: add audience, keep doc, drop classification.
		n, err := s.UpdateMetadata(ctx, tenant, uc,
			vector.Filter{Conditions: []vector.Condition{{Field: "classification", Op: vector.OpEq, Value: "guest"}}},
			map[string]any{"audience": "anyone", "classification": nil})
		if err != nil || n != 2 {
			t.Fatalf("update: n=%d err=%v want 2", n, err)
		}
		got, _ := s.Search(ctx, tenant, uc, []float32{1, 0, 0, 0}, 10,
			vector.Filter{Conditions: []vector.Condition{{Field: "audience", Op: vector.OpEq, Value: "anyone"}}})
		if !sameSet(ids(got), []string{"p", "q"}) {
			t.Fatalf("after update: %v want [p q]", ids(got))
		}
		for _, m := range got {
			if m.Metadata["doc"] == nil || m.Metadata["classification"] != nil || m.Text == "" {
				t.Fatalf("update must merge (keep doc, drop classification, keep text): %+v", m)
			}
		}
		// The untouched item still carries its old label and no audience.
		got, _ = s.Search(ctx, tenant, uc, []float32{0, 0, 1, 0}, 1, vector.Filter{})
		if len(got) != 1 || got[0].ID != "r" || got[0].Metadata["classification"] != "internal" || got[0].Metadata["audience"] != nil {
			t.Fatalf("untouched item changed: %+v", got)
		}
		// Vectors are untouched: ranking is unchanged.
		got, _ = s.Search(ctx, tenant, uc, []float32{0, 1, 0, 0}, 1, vector.Filter{})
		if len(got) != 1 || got[0].ID != "q" {
			t.Fatalf("vector changed by update: %v", ids(got))
		}
		// Re-running the same re-label with a not_in on the new field must
		// match nothing: the retag lane's "done" signal.
		if n, err := s.UpdateMetadata(ctx, tenant, uc,
			vector.Filter{Conditions: []vector.Condition{
				{Field: "doc", Op: vector.OpIn, Value: []any{"d1", "d2"}},
				{Field: "audience", Op: vector.OpNotIn, Value: []any{"anyone"}},
			}},
			map[string]any{"audience": "anyone"}); err != nil || n != 0 {
			t.Fatalf("second pass: n=%d err=%v want 0 (not_in must exclude the rows already tagged)", n, err)
		}
		// id filter works for update too; a miss matches nothing.
		if n, err := s.UpdateMetadata(ctx, tenant, uc,
			vector.Filter{Conditions: []vector.Condition{{Field: "id", Op: vector.OpIn, Value: []any{"r", "nope"}}}},
			map[string]any{"audience": "team"}); err != nil || n != 1 {
			t.Fatalf("id update: n=%d err=%v want 1", n, err)
		}
		// An unconditional update is refused.
		if _, err := s.UpdateMetadata(ctx, tenant, uc, vector.Filter{}, map[string]any{"x": 1}); err == nil {
			t.Fatal("unconditional update: want error")
		} else if _, ok := err.(*vector.InvalidArgError); !ok {
			t.Fatalf("want InvalidArgError, got %T", err)
		}
		if _, err := s.UpdateMetadata(ctx, tenant, "nope",
			vector.Filter{Conditions: []vector.Condition{{Field: "id", Op: vector.OpEq, Value: "x"}}}, map[string]any{"x": 1}); err == nil {
			t.Fatal("update missing collection: want error")
		}
	})

	t.Run("Get", func(t *testing.T) {
		const gc = "gets"
		if err := s.EnsureCollection(ctx, tenant, vector.Collection{Name: gc, Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		var items []vector.Item
		for i := 0; i < 5; i++ {
			heading := "A"
			if i >= 3 {
				heading = "B"
			}
			items = append(items, vector.Item{
				ID: fmt.Sprintf("d:%d", i), Vector: []float32{1, float32(i), 0, 0}, Text: fmt.Sprintf("t%d", i),
				Metadata: map[string]any{"doc": "x", "idx": i, "heading": heading},
			})
		}
		items = append(items, vector.Item{ID: "e:0", Vector: []float32{0, 0, 1, 0}, Text: "other", Metadata: map[string]any{"doc": "y", "idx": 0}})
		if _, err := s.Upsert(ctx, tenant, gc, items); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		get := func(sel vector.Selector, after string, limit int) vector.GetResult {
			t.Helper()
			res, err := s.Get(ctx, tenant, gc, sel, after, limit)
			if err != nil {
				t.Fatalf("get %+v after=%q limit=%d: %v", sel, after, limit, err)
			}
			return res
		}
		where := func(conds ...vector.Condition) vector.Selector {
			return vector.Selector{Filter: vector.Filter{Conditions: conds}}
		}
		docX := vector.Condition{Field: "doc", Op: vector.OpEq, Value: "x"}

		// By ids: the order asked, a duplicate once, an absent id in Missing,
		// metadata and text whole.
		res := get(vector.Selector{IDs: []string{"d:3", "nope", "d:1", "d:3"}}, "", 0)
		if got := recIDs(res.Items); !equal(got, []string{"d:3", "d:1"}) || !equal(res.Missing, []string{"nope"}) || res.More {
			t.Fatalf("by ids: items=%v missing=%v more=%v, want [d:3 d:1] [nope] false", got, res.Missing, res.More)
		}
		if r := res.Items[0]; r.Text != "t3" || r.Metadata["doc"] != "x" || fmt.Sprint(r.Metadata["idx"]) != "3" {
			t.Fatalf("by ids: record not whole: %+v", r)
		}

		// By filter: ascending id, the limit, More, and paging with after.
		res = get(where(docX), "", 2)
		if got := recIDs(res.Items); !equal(got, []string{"d:0", "d:1"}) || !res.More || len(res.Missing) != 0 {
			t.Fatalf("filter page 1: %v more=%v, want [d:0 d:1] true", got, res.More)
		}
		res = get(where(docX), "d:1", 2)
		if got := recIDs(res.Items); !equal(got, []string{"d:2", "d:3"}) || !res.More {
			t.Fatalf("filter page 2: %v more=%v, want [d:2 d:3] true", got, res.More)
		}
		res = get(where(docX), "d:3", 2)
		if got := recIDs(res.Items); !equal(got, []string{"d:4"}) || res.More {
			t.Fatalf("filter page 3: %v more=%v, want [d:4] false", got, res.More)
		}
		// The default limit covers a small match whole.
		res = get(where(docX), "", 0)
		if got := recIDs(res.Items); len(got) != 5 || res.More {
			t.Fatalf("default limit: %v more=%v, want 5 items, false", got, res.More)
		}
		// Every operator the search grammar has, a number included.
		for _, tc := range []struct {
			name string
			sel  vector.Selector
			want []string
		}{
			{"in number", where(docX, vector.Condition{Field: "idx", Op: vector.OpIn, Value: []any{1, 2}}), []string{"d:1", "d:2"}},
			{"gte/lt", where(vector.Condition{Field: "idx", Op: vector.OpGte, Value: 3}, vector.Condition{Field: "idx", Op: vector.OpLt, Value: 5}), []string{"d:3", "d:4"}},
			{"not_in", where(vector.Condition{Field: "heading", Op: vector.OpNotIn, Value: []any{"A"}}), []string{"d:3", "d:4", "e:0"}},
			{"id field", where(vector.Condition{Field: "id", Op: vector.OpIn, Value: []any{"d:0", "e:0", "nope"}}), []string{"d:0", "e:0"}},
			{"a section", where(docX, vector.Condition{Field: "heading", Op: vector.OpEq, Value: "B"}), []string{"d:3", "d:4"}},
			{"no match", where(vector.Condition{Field: "doc", Op: vector.OpEq, Value: "z"}), []string{}},
		} {
			res := get(tc.sel, "", 0)
			if got := recIDs(res.Items); !equal(got, tc.want) || res.More {
				t.Fatalf("filter %s: %v more=%v, want %v false", tc.name, got, res.More, tc.want)
			}
		}

		// Refusals: the selector's rules, the same for every backend.
		for _, tc := range []struct {
			name  string
			sel   vector.Selector
			after string
			limit int
		}{
			{"neither", vector.Selector{}, "", 0},
			{"both", vector.Selector{IDs: []string{"d:0"}, Filter: vector.Filter{Conditions: []vector.Condition{docX}}}, "", 0},
			{"an empty id", vector.Selector{IDs: []string{"d:0", ""}}, "", 0},
			{"too many ids", vector.Selector{IDs: make501()}, "", 0},
			{"after with ids", vector.Selector{IDs: []string{"d:0"}}, "d:0", 0},
			{"limit with ids", vector.Selector{IDs: []string{"d:0"}}, "", 5},
			{"limit too large", where(docX), "", vector.MaxGetLimit + 1},
		} {
			if _, err := s.Get(ctx, tenant, gc, tc.sel, tc.after, tc.limit); err == nil {
				t.Fatalf("get %s: want an error", tc.name)
			} else if _, ok := err.(*vector.InvalidArgError); !ok {
				t.Fatalf("get %s: want InvalidArgError, got %T %v", tc.name, err, err)
			}
		}
		if _, err := s.Get(ctx, tenant, "nope", vector.Selector{IDs: []string{"d:0"}}, "", 0); err == nil {
			t.Fatal("get from a missing collection: want an error")
		} else if _, ok := err.(*vector.CollectionNotFoundError); !ok {
			t.Fatalf("want CollectionNotFoundError, got %T", err)
		}

		// Tenant isolation: the same collection name in another tenant holds
		// only its own items.
		const other = "globex"
		if err := s.EnsureCollection(ctx, other, vector.Collection{Name: gc, Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("ensure other: %v", err)
		}
		if _, err := s.Upsert(ctx, other, gc, []vector.Item{{ID: "g:0", Vector: []float32{1, 0, 0, 0}, Metadata: map[string]any{"doc": "x"}}}); err != nil {
			t.Fatalf("upsert other: %v", err)
		}
		res, err := s.Get(ctx, other, gc, vector.Selector{IDs: []string{"d:0", "g:0"}}, "", 0)
		if err != nil || !equal(recIDs(res.Items), []string{"g:0"}) || !equal(res.Missing, []string{"d:0"}) {
			t.Fatalf("other tenant by ids: %+v err=%v, want [g:0] missing [d:0]", res, err)
		}
		res, err = s.Get(ctx, other, gc, where(docX), "", 0)
		if err != nil || !equal(recIDs(res.Items), []string{"g:0"}) {
			t.Fatalf("other tenant by filter: %+v err=%v, want [g:0]", res, err)
		}
	})

	t.Run("GetTextBound", func(t *testing.T) {
		const bc = "bigtext"
		if err := s.EnsureCollection(ctx, tenant, vector.Collection{Name: bc, Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("ensure: %v", err)
		}
		big := strings.Repeat("x", 600<<10)                   // two of these pass the 1 MiB bound
		huge := strings.Repeat("y", vector.MaxGetTextBytes+1) // larger than the bound alone
		if _, err := s.Upsert(ctx, tenant, bc, []vector.Item{
			{ID: "b:0", Vector: []float32{1, 0, 0, 0}, Text: big, Metadata: map[string]any{"k": "big"}},
			{ID: "b:1", Vector: []float32{1, 0, 0, 0}, Text: big, Metadata: map[string]any{"k": "big"}},
			{ID: "b:2", Vector: []float32{1, 0, 0, 0}, Text: "small", Metadata: map[string]any{"k": "big"}},
			{ID: "h:0", Vector: []float32{1, 0, 0, 0}, Text: huge, Metadata: map[string]any{"k": "huge"}},
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// By filter: the second big item would pass the bound, so the answer
		// stops at the first, whole, and says More.
		res, err := s.Get(ctx, tenant, bc, vector.Selector{Filter: vector.Filter{Conditions: []vector.Condition{{Field: "k", Op: vector.OpEq, Value: "big"}}}}, "", 0)
		if err != nil || !equal(recIDs(res.Items), []string{"b:0"}) || !res.More || len(res.Items[0].Text) != len(big) {
			t.Fatalf("filter bound: ids=%v more=%v err=%v, want [b:0] true, text whole", recIDs(res.Items), res.More, err)
		}
		// By ids the same, and an id past the bound is neither returned nor
		// missing.
		res, err = s.Get(ctx, tenant, bc, vector.Selector{IDs: []string{"b:0", "b:1", "nope"}}, "", 0)
		if err != nil || !equal(recIDs(res.Items), []string{"b:0"}) || !res.More || !equal(res.Missing, []string{"nope"}) {
			t.Fatalf("ids bound: %v more=%v missing=%v err=%v, want [b:0] true [nope]", recIDs(res.Items), res.More, res.Missing, err)
		}
		// An item larger than the bound comes back alone, never cut.
		res, err = s.Get(ctx, tenant, bc, vector.Selector{IDs: []string{"h:0", "b:2"}}, "", 0)
		if err != nil || !equal(recIDs(res.Items), []string{"h:0"}) || !res.More || len(res.Items[0].Text) != len(huge) {
			t.Fatalf("huge item: %v more=%v err=%v, want [h:0] alone, whole", recIDs(res.Items), res.More, err)
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

	t.Run("ListAndDropCollection", func(t *testing.T) {
		const lt = "listtenant"
		if err := s.EnsureCollection(ctx, lt, vector.Collection{Name: "alpha", EmbeddingModel: "m", Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("ensure alpha: %v", err)
		}
		if err := s.EnsureCollection(ctx, lt, vector.Collection{Name: "beta", EmbeddingModel: "m", Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("ensure beta: %v", err)
		}
		if _, err := s.Upsert(ctx, lt, "alpha", []vector.Item{
			{ID: "x", Vector: []float32{1, 0, 0, 0}}, {ID: "y", Vector: []float32{0, 1, 0, 0}},
		}); err != nil {
			t.Fatalf("upsert alpha: %v", err)
		}

		cols, err := s.ListCollections(ctx, lt)
		if err != nil {
			t.Fatalf("list collections: %v", err)
		}
		var names []string
		for _, c := range cols {
			names = append(names, c.Name)
		}
		if !sameSet(names, []string{"alpha", "beta"}) {
			t.Fatalf("ListCollections: %v want [alpha beta]", names)
		}

		// Drop alpha (2 items); beta untouched.
		if n, err := s.DropCollection(ctx, lt, "alpha"); err != nil || n != 2 {
			t.Fatalf("drop alpha: n=%d err=%v want 2", n, err)
		}
		if _, found, _ := s.DescribeCollection(ctx, lt, "alpha"); found {
			t.Fatal("alpha still present after drop")
		}
		if _, found, _ := s.DescribeCollection(ctx, lt, "beta"); !found {
			t.Fatal("beta should survive alpha's drop")
		}
		// Dropping a missing collection is a no-op, not an error.
		if n, err := s.DropCollection(ctx, lt, "alpha"); err != nil || n != 0 {
			t.Fatalf("re-drop alpha: n=%d err=%v want 0/nil", n, err)
		}
		// A re-created collection of the same name starts empty.
		if err := s.EnsureCollection(ctx, lt, vector.Collection{Name: "alpha", Dimensions: dim, Metric: vector.MetricCosine}); err != nil {
			t.Fatalf("re-ensure alpha: %v", err)
		}
		if ids, _ := s.ListIDs(ctx, lt, "alpha"); len(ids) != 0 {
			t.Fatalf("re-created alpha not empty: %v", ids)
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

func recIDs(rs []vector.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// equal compares in order (a Get's order is part of its contract); nil and
// empty are the same.
func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func make501() []string {
	out := make([]string, vector.MaxGetIDs+1)
	for i := range out {
		out[i] = fmt.Sprintf("id-%d", i)
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
