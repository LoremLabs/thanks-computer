// Package searchtest holds the backend-agnostic conformance suite for
// search.Store implementations. The bundled Bleve backend runs it; a backend
// that reaches the same engine some other way reuses it unchanged, which is
// what keeps "search on a laptop" and "search hosted" behaviourally identical
// behind the interface.
package searchtest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// Options adapts the suite to a backend.
type Options struct {
	// Settle, when set, is called after every write and must return once that
	// write is visible to a Query. A backend that applies writes
	// asynchronously supplies it; a synchronous backend leaves it nil.
	Settle func(t *testing.T, s search.Store, tenant, collection string)
}

// RunConformance exercises the full search.Store contract against a fresh
// store produced by newStore. newStore is called once per RunConformance.
func RunConformance(t *testing.T, newStore func(t *testing.T) search.Store, opts ...Options) {
	t.Helper()
	ctx := context.Background()
	const tenant = "acme"
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}

	s := newStore(t)
	t.Cleanup(func() { _ = s.Close() })

	settle := func(t *testing.T, tn, coll string) {
		t.Helper()
		if opt.Settle != nil {
			opt.Settle(t, s, tn, coll)
		}
	}
	ensure := func(t *testing.T, tn, coll string) {
		t.Helper()
		if err := s.EnsureCollection(ctx, tn, search.Collection{Name: coll}); err != nil {
			t.Fatalf("EnsureCollection(%s): %v", coll, err)
		}
		settle(t, tn, coll)
	}
	upsert := func(t *testing.T, tn, coll string, items ...search.Item) {
		t.Helper()
		n, err := s.Upsert(ctx, tn, coll, items)
		if err != nil {
			t.Fatalf("Upsert(%s): %v", coll, err)
		}
		if n != len(items) {
			t.Fatalf("Upsert(%s) = %d, want %d", coll, n, len(items))
		}
		settle(t, tn, coll)
	}
	query := func(t *testing.T, tn, coll, q string, limit int, f search.Filter) []search.Hit {
		t.Helper()
		hits, err := s.Query(ctx, tn, coll, q, limit, f)
		if err != nil {
			t.Fatalf("Query(%s, %q): %v", coll, q, err)
		}
		return hits
	}

	t.Run("EnsureAndDescribe", func(t *testing.T) {
		const coll = "describe"
		if _, found, err := s.DescribeCollection(ctx, tenant, coll); err != nil || found {
			t.Fatalf("describe before ensure: found=%v err=%v", found, err)
		}
		ensure(t, tenant, coll)
		got, found, err := s.DescribeCollection(ctx, tenant, coll)
		if err != nil || !found {
			t.Fatalf("describe: found=%v err=%v", found, err)
		}
		if got.Name != coll || got.AnalyzerVersion == "" || got.ScoringModel == "" || got.Records != 0 {
			t.Fatalf("describe = %+v", got)
		}
		ensure(t, tenant, coll) // idempotent
		// Re-ensuring under the pins it already has is fine too.
		if err := s.EnsureCollection(ctx, tenant, got); err != nil {
			t.Fatalf("re-ensure with its own pins: %v", err)
		}
		upsert(t, tenant, coll, search.Item{ID: "a", Text: "one record"})
		if got, _, _ := s.DescribeCollection(ctx, tenant, coll); got.Records != 1 {
			t.Fatalf("Records = %d, want 1", got.Records)
		}
	})

	t.Run("AnalyzerMismatch", func(t *testing.T) {
		const coll = "pins"
		ensure(t, tenant, coll)
		for _, c := range []search.Collection{
			{Name: coll, AnalyzerVersion: "some_other_analyzer"},
			{Name: coll, ScoringModel: "some_other_model"},
			{Name: "pins-new", AnalyzerVersion: "some_other_analyzer"},
		} {
			var want *search.AnalyzerMismatchError
			if err := s.EnsureCollection(ctx, tenant, c); !errors.As(err, &want) {
				t.Errorf("EnsureCollection(%+v): want AnalyzerMismatchError, got %T %v", c, err, err)
			}
		}
	})

	t.Run("MissingCollection", func(t *testing.T) {
		var want *search.CollectionNotFoundError
		if _, err := s.Upsert(ctx, tenant, "nope", []search.Item{{ID: "x", Text: "y"}}); !errors.As(err, &want) {
			t.Errorf("Upsert: want CollectionNotFoundError, got %T %v", err, err)
		}
		if _, err := s.Query(ctx, tenant, "nope", "y", 5, search.Filter{}); !errors.As(err, &want) {
			t.Errorf("Query: want CollectionNotFoundError, got %T %v", err, err)
		}
		if _, err := s.Delete(ctx, tenant, "nope", search.Selector{IDs: []string{"x"}}); !errors.As(err, &want) {
			t.Errorf("Delete: want CollectionNotFoundError, got %T %v", err, err)
		}
		if _, err := s.Update(ctx, tenant, "nope", eq("a", "b"), search.Change{Merge: map[string]any{"k": "v"}}); !errors.As(err, &want) {
			t.Errorf("Update: want CollectionNotFoundError, got %T %v", err, err)
		}
	})

	t.Run("UpsertReplaceAndHitShape", func(t *testing.T) {
		const coll = "shape"
		ensure(t, tenant, coll)
		upsert(t, tenant, coll,
			search.Item{ID: "a", Text: "Luggage can be left in the hall cupboard before check-in.", Title: "FAQ",
				Metadata: map[string]any{"doc_key": "dr_1", "page": float64(7), "tags": []any{"x", "y"}}},
			search.Item{ID: "b", Text: "The pool opens at nine."},
		)
		hits := query(t, tenant, coll, "where can I leave my luggage", 5, search.Filter{})
		if len(hits) != 1 || hits[0].ID != "a" || hits[0].Rank != 1 {
			t.Fatalf("hits = %+v", hits)
		}
		if hits[0].Text != "Luggage can be left in the hall cupboard before check-in." {
			t.Errorf("hit text = %q", hits[0].Text)
		}
		if hits[0].Metadata["doc_key"] != "dr_1" || hits[0].Metadata["page"] != float64(7) {
			t.Errorf("hit metadata = %v", hits[0].Metadata)
		}
		// Same id replaces: the old words stop matching, the count stays.
		upsert(t, tenant, coll, search.Item{ID: "a", Text: "Bicycles are stored in the courtyard."})
		if hits := query(t, tenant, coll, "luggage cupboard", 5, search.Filter{}); len(hits) != 0 {
			t.Errorf("replaced record still matches: %v", ids(hits))
		}
		if hits := query(t, tenant, coll, "bicycles", 5, search.Filter{}); len(hits) != 1 || hits[0].ID != "a" {
			t.Errorf("replacement not found: %v", ids(hits))
		}
		if c, _, _ := s.DescribeCollection(ctx, tenant, coll); c.Records != 2 {
			t.Errorf("Records = %d, want 2", c.Records)
		}
	})

	t.Run("QuerySemantics", func(t *testing.T) {
		const coll = "semantics"
		ensure(t, tenant, coll)
		upsert(t, tenant, coll,
			search.Item{ID: "clause", Text: "Either party may exercise termination for convenience on thirty days notice."},
			search.Item{ID: "scatter", Text: "For your convenience, the termination date is printed on the form. Convenience matters."},
			search.Item{ID: "luggage", Text: "Luggage can be left in the hall cupboard before check-in."},
			search.Item{ID: "accent", Text: "The crème brûlée is made fresh every morning."},
		)
		// A quoted string is a phrase: the intact one ranks first, and OR
		// semantics still admit the record with the same words scattered.
		if got := ids(query(t, tenant, coll, `"termination for convenience"`, 5, search.Filter{})); len(got) != 2 || got[0] != "clause" {
			t.Errorf("phrase = %v, want [clause scatter]", got)
		}
		// Terms are OR-ed, so a whole message works as a query.
		if got := ids(query(t, tenant, coll, "hello, could you tell me where I might leave my luggage before we arrive?", 5, search.Filter{})); len(got) == 0 || got[0] != "luggage" {
			t.Errorf("OR = %v, want luggage first", got)
		}
		// Case and diacritics fold.
		if got := ids(query(t, tenant, coll, "CREME BRULEE", 5, search.Filter{})); len(got) != 1 || got[0] != "accent" {
			t.Errorf("folding = %v, want [accent]", got)
		}
		// No engine syntax is recognised.
		if got := ids(query(t, tenant, coll, `+luggage -cupboard text:hall*`, 5, search.Filter{})); len(got) != 1 || got[0] != "luggage" {
			t.Errorf("syntax = %v, want [luggage]", got)
		}
		// No stemming: "exercises" is not "exercise".
		if got := ids(query(t, tenant, coll, "exercises", 5, search.Filter{})); len(got) != 0 {
			t.Errorf("stemming = %v, want none", got)
		}
		// Nothing to search for is an empty result, not an error.
		for _, q := range []string{"", "?!… --", "zebra"} {
			if got := query(t, tenant, coll, q, 5, search.Filter{}); len(got) != 0 {
				t.Errorf("Query(%q) = %v, want none", q, ids(got))
			}
		}
	})

	t.Run("Identifiers", func(t *testing.T) {
		const coll = "identifiers"
		ensure(t, tenant, coll)
		idents := []string{
			"TXC-4821", "RFC-5321", "matt@example.com", "onepony-parser",
			"foo.bar.baz", "invoice_92812", "2026-09-21", "github.com/foo/bar",
		}
		var items []search.Item
		for _, id := range idents {
			items = append(items, search.Item{ID: id, Text: "The record for " + id + " was filed by the operations desk after review."})
		}
		// Distractors share the identifiers' parts, in other orders and places.
		items = append(items,
			search.Item{ID: "d1", Text: "TXC was founded in 4821 words of prose. RFC 9999 and 5321 lines of code."},
			search.Item{ID: "d2", Text: "Write to bar@foo.example or see baz.bar.foo; invoice 11111 and parser onepony."},
			search.Item{ID: "d3", Text: "On 21-09-2026 the com github bar foo listing was updated by matt."},
		)
		upsert(t, tenant, coll, items...)
		for _, id := range idents {
			if got := ids(query(t, tenant, coll, "what do we know about "+id+"?", 5, search.Filter{})); len(got) == 0 || got[0] != id {
				t.Errorf("Query(%q) = %v, want it first", id, got)
			}
		}
	})

	t.Run("Fields", func(t *testing.T) {
		const coll = "fields"
		ensure(t, tenant, coll)
		upsert(t, tenant, coll,
			search.Item{ID: "file", Text: "General notes.", Name: "apollo-launch-plan.pdf"},
			search.Item{ID: "mention", Text: "The apollo launch plan is mentioned once here in passing, among many other words about nothing much."},
			search.Item{ID: "entity", Text: "Unrelated.", Entities: []string{"Marc Dupont", "Mozilla"}},
			search.Item{ID: "titled", Text: "Body text.", Title: "Quarterly Revenue Forecast", Heading: "Assumptions"},
		)
		for q, want := range map[string]string{
			"apollo-launch-plan.pdf":     "file",
			"what did Marc Dupont say":   "entity",
			"quarterly revenue forecast": "titled",
			"assumptions":                "titled",
		} {
			if got := ids(query(t, tenant, coll, q, 5, search.Filter{})); len(got) == 0 || got[0] != want {
				t.Errorf("Query(%q) = %v, want %s first", q, got, want)
			}
		}
	})

	t.Run("Filters", func(t *testing.T) {
		const coll = "filters"
		ensure(t, tenant, coll)
		upsert(t, tenant, coll,
			search.Item{ID: "p1", Text: "pool opening hours", Metadata: map[string]any{"slug": "paris", "audience": "anyone", "idx": float64(0), "date": "2026-09-21"}},
			search.Item{ID: "p2", Text: "pool opening hours", Metadata: map[string]any{"slug": "paris", "audience": "team", "idx": float64(1)}},
			search.Item{ID: "p3", Text: "pool opening hours", Metadata: map[string]any{"slug": "rome", "audience": "anyone", "idx": float64(2), "draft": true}},
			search.Item{ID: "p4", Text: "pool opening hours", Metadata: map[string]any{"slug": "paris", "idx": float64(3)}}, // no audience
		)
		for _, tc := range []struct {
			name string
			f    search.Filter
			want []string
		}{
			{"none", search.Filter{}, []string{"p1", "p2", "p3", "p4"}},
			{"eq", eq("slug", "paris"), []string{"p1", "p2", "p4"}},
			{"eq and eq", and(eq("slug", "paris"), eq("audience", "anyone")), []string{"p1"}},
			{"eq is exact", eq("slug", "Paris"), nil},
			{"in", cond("audience", vector.OpIn, []any{"anyone", "team"}), []string{"p1", "p2", "p3"}},
			{"in nothing", cond("audience", vector.OpIn, []any{}), nil},
			{"not_in passes an absent field", cond("audience", vector.OpNotIn, []any{"team"}), []string{"p1", "p3", "p4"}},
			{"not_in nothing", cond("audience", vector.OpNotIn, []any{}), []string{"p1", "p2", "p3", "p4"}},
			{"gte", cond("idx", vector.OpGte, float64(2)), []string{"p3", "p4"}},
			{"gt", cond("idx", vector.OpGt, float64(2)), []string{"p4"}},
			{"lte", cond("idx", vector.OpLte, float64(1)), []string{"p1", "p2"}},
			{"lt", cond("idx", vector.OpLt, float64(1)), []string{"p1"}},
			{"a number is not a string", eq("idx", "1"), nil},
			{"bool", eq("draft", true), []string{"p3"}},
			{"a date-shaped string stays a string", eq("date", "2026-09-21"), []string{"p1"}},
			{"id eq", eq("id", "p2"), []string{"p2"}},
			{"id in", cond("id", vector.OpIn, []any{"p2", "p4", "zz"}), []string{"p2", "p4"}},
			{"id not_in", cond("id", vector.OpNotIn, []any{"p1", "p2"}), []string{"p3", "p4"}},
		} {
			got := ids(query(t, tenant, coll, "pool hours", 10, tc.f))
			sort.Strings(got)
			if !sameIDs(got, tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
		var bad *search.InvalidArgError
		if _, err := s.Query(ctx, tenant, coll, "pool", 10, cond("slug", vector.Op("like"), "x")); !errors.As(err, &bad) {
			t.Errorf("unknown op: want InvalidArgError, got %T %v", err, err)
		}
	})

	// A filter admits or rejects. It must never move the order.
	t.Run("FilterDoesNotScore", func(t *testing.T) {
		const coll = "filterscore"
		ensure(t, tenant, coll)
		upsert(t, tenant, coll,
			search.Item{ID: "a", Text: "pool pool pool hours", Metadata: map[string]any{"slug": "paris"}},
			search.Item{ID: "b", Text: "the pool has hours posted by the door near reception", Metadata: map[string]any{"slug": "paris"}},
			search.Item{ID: "c", Text: "hours", Metadata: map[string]any{"slug": "paris"}},
		)
		plain := ids(query(t, tenant, coll, "pool hours", 10, search.Filter{}))
		filtered := ids(query(t, tenant, coll, "pool hours", 10, eq("slug", "paris")))
		if !reflect.DeepEqual(plain, filtered) {
			t.Errorf("a filter changed the order: %v vs %v", plain, filtered)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		const coll = "delete"
		ensure(t, tenant, coll)
		var items []search.Item
		for i := 0; i < 6; i++ {
			items = append(items, search.Item{ID: fmt.Sprintf("d1:%d", i), Text: "cleaning rota", Metadata: map[string]any{"doc_key": "d1", "idx": float64(i)}})
		}
		items = append(items, search.Item{ID: "d2:0", Text: "cleaning rota", Metadata: map[string]any{"doc_key": "d2", "idx": float64(0)}})
		upsert(t, tenant, coll, items...)

		// By ids: the count is what existed, not what was asked for.
		n, err := s.Delete(ctx, tenant, coll, search.Selector{IDs: []string{"d1:5", "never-existed"}})
		if err != nil || n != 1 {
			t.Fatalf("Delete by ids = %d, %v; want 1", n, err)
		}
		settle(t, tenant, coll)
		// The stale tail of a document that shrank: one filter, no id list.
		n, err = s.Delete(ctx, tenant, coll, search.Selector{Filter: and(eq("doc_key", "d1"), cond("idx", vector.OpGte, float64(3)))})
		if err != nil || n != 2 {
			t.Fatalf("Delete the tail = %d, %v; want 2", n, err)
		}
		settle(t, tenant, coll)
		if got := ids(query(t, tenant, coll, "cleaning", 10, search.Filter{})); !sameIDs(sorted(got), []string{"d1:0", "d1:1", "d1:2", "d2:0"}) {
			t.Errorf("after deletes: %v", got)
		}
		// A whole document, without knowing how many chunks it has.
		n, err = s.Delete(ctx, tenant, coll, search.Selector{Filter: eq("doc_key", "d1")})
		if err != nil || n != 3 {
			t.Fatalf("Delete a document = %d, %v; want 3", n, err)
		}
		settle(t, tenant, coll)
		if got := ids(query(t, tenant, coll, "cleaning", 10, search.Filter{})); !sameIDs(got, []string{"d2:0"}) {
			t.Errorf("after the document delete: %v", got)
		}
		// Deleting again is not an error.
		if n, err := s.Delete(ctx, tenant, coll, search.Selector{Filter: eq("doc_key", "d1")}); err != nil || n != 0 {
			t.Errorf("second delete = %d, %v; want 0", n, err)
		}
		var bad *search.InvalidArgError
		for _, sel := range []search.Selector{{}, {IDs: []string{"a"}, Filter: eq("doc_key", "d2")}} {
			if _, err := s.Delete(ctx, tenant, coll, sel); !errors.As(err, &bad) {
				t.Errorf("Delete(%+v): want InvalidArgError, got %T %v", sel, err, err)
			}
		}
	})

	t.Run("Update", func(t *testing.T) {
		const coll = "update"
		ensure(t, tenant, coll)
		upsert(t, tenant, coll,
			search.Item{ID: "h:0", Text: "staff handbook, holidays", Name: "rota-2025.pdf", Metadata: map[string]any{"doc_key": "h", "audience": "team", "legacy": "internal"}},
			search.Item{ID: "h:1", Text: "staff handbook, expenses", Name: "rota-2025.pdf", Metadata: map[string]any{"doc_key": "h", "audience": "team", "legacy": "internal"}},
			search.Item{ID: "m:0", Text: "the menu", Name: "menu.md", Metadata: map[string]any{"doc_key": "m", "audience": "anyone"}},
		)
		// A merge re-labels, removes a key on nil, and leaves text alone.
		n, err := s.Update(ctx, tenant, coll, eq("doc_key", "h"), search.Change{Merge: map[string]any{"audience": "anyone", "legacy": nil}})
		if err != nil || n != 2 {
			t.Fatalf("Update = %d, %v; want 2", n, err)
		}
		settle(t, tenant, coll)
		hits := query(t, tenant, coll, "handbook", 10, eq("audience", "anyone"))
		if !sameIDs(sorted(ids(hits)), []string{"h:0", "h:1"}) {
			t.Fatalf("after the merge: %v", ids(hits))
		}
		for _, h := range hits {
			if _, still := h.Metadata["legacy"]; still || h.Metadata["doc_key"] != "h" || !strings.HasPrefix(h.Text, "staff handbook") {
				t.Errorf("merged record = %+v", h)
			}
		}
		if got := query(t, tenant, coll, "handbook", 10, eq("audience", "team")); len(got) != 0 {
			t.Errorf("the old label still matches: %v", ids(got))
		}
		// Fields renames the file without a re-upsert: the new name finds it and
		// the old name no longer does.
		renamed := "employee-guide.docx" // shares no word with the old name
		if n, err := s.Update(ctx, tenant, coll, eq("doc_key", "h"), search.Change{Fields: &search.FieldSet{Name: &renamed}}); err != nil || n != 2 {
			t.Fatalf("Update fields = %d, %v; want 2", n, err)
		}
		settle(t, tenant, coll)
		if got := ids(query(t, tenant, coll, "employee-guide.docx", 10, search.Filter{})); !sameIDs(sorted(got), []string{"h:0", "h:1"}) {
			t.Errorf("the new name: %v", got)
		}
		if got := ids(query(t, tenant, coll, "rota-2025.pdf", 10, search.Filter{})); len(got) != 0 {
			t.Errorf("the old name still matches: %v", got)
		}
		// No match is a count of zero.
		if n, err := s.Update(ctx, tenant, coll, eq("doc_key", "zz"), search.Change{Merge: map[string]any{"a": "b"}}); err != nil || n != 0 {
			t.Errorf("Update of nothing = %d, %v; want 0", n, err)
		}
		// An update must be conditional, and must change something.
		var bad *search.InvalidArgError
		if _, err := s.Update(ctx, tenant, coll, search.Filter{}, search.Change{Merge: map[string]any{"a": "b"}}); !errors.As(err, &bad) {
			t.Errorf("no filter: want InvalidArgError, got %T %v", err, err)
		}
		if _, err := s.Update(ctx, tenant, coll, eq("doc_key", "h"), search.Change{}); !errors.As(err, &bad) {
			t.Errorf("no change: want InvalidArgError, got %T %v", err, err)
		}
	})

	t.Run("TenantIsolation", func(t *testing.T) {
		const coll = "shared-name"
		ensure(t, "tenant-a", coll)
		ensure(t, "tenant-b", coll)
		upsert(t, "tenant-a", coll, search.Item{ID: "x", Text: "alpha secret"})
		upsert(t, "tenant-b", coll, search.Item{ID: "x", Text: "beta secret"})
		if got := query(t, "tenant-a", coll, "secret", 5, search.Filter{}); len(got) != 1 || !strings.HasPrefix(got[0].Text, "alpha") {
			t.Errorf("tenant-a sees %+v", got)
		}
		if got := query(t, "tenant-b", coll, "alpha", 5, search.Filter{}); len(got) != 0 {
			t.Errorf("tenant-b sees tenant-a's record: %+v", got)
		}
		if _, err := s.Delete(ctx, "tenant-b", coll, search.Selector{IDs: []string{"x"}}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		settle(t, "tenant-b", coll)
		if got := query(t, "tenant-a", coll, "secret", 5, search.Filter{}); len(got) != 1 {
			t.Errorf("tenant-b's delete reached tenant-a: %+v", got)
		}
	})

	// The collection is the ranking unit. What a second collection holds must
	// not move the first one's order, however lopsided its vocabulary.
	t.Run("CollectionLocalRanking", func(t *testing.T) {
		ensure(t, tenant, "rank-a")
		ensure(t, tenant, "rank-b")
		corpus := []search.Item{
			{ID: "1", Text: "apollo launch schedule and the apollo crew"},
			{ID: "2", Text: "launch checklist for the crew, with the apollo callsign"},
			{ID: "3", Text: "crew rota"},
			{ID: "4", Text: "schedule of the week"},
		}
		upsert(t, tenant, "rank-a", corpus...)
		before := ids(query(t, tenant, "rank-a", "apollo crew schedule", 10, search.Filter{}))
		var flood []search.Item
		for i := 0; i < 200; i++ {
			flood = append(flood, search.Item{ID: fmt.Sprintf("f%d", i), Text: "apollo apollo apollo schedule crew launch"})
		}
		upsert(t, tenant, "rank-b", flood...)
		after := ids(query(t, tenant, "rank-a", "apollo crew schedule", 10, search.Filter{}))
		if len(before) == 0 || !reflect.DeepEqual(before, after) {
			t.Errorf("another collection moved this one's ranking: %v then %v", before, after)
		}
	})

	t.Run("Limits", func(t *testing.T) {
		const coll = "limits"
		ensure(t, tenant, coll)
		var big *search.TooLargeError
		var bad *search.InvalidArgError
		many := make([]search.Item, search.MaxItemsPerUpsert+1)
		for i := range many {
			many[i] = search.Item{ID: fmt.Sprintf("i%d", i), Text: "x"}
		}
		manyKeys := map[string]any{}
		for i := 0; i <= search.MaxMetadataKeys; i++ {
			manyKeys[fmt.Sprintf("k%d", i)] = i
		}
		for name, items := range map[string][]search.Item{
			"items per upsert": many,
			"text":             {{ID: "a", Text: strings.Repeat("x", search.MaxTextBytes+1)}},
			"title":            {{ID: "a", Title: strings.Repeat("x", search.MaxShortFieldBytes+1)}},
			"entities":         {{ID: "a", Entities: make([]string, search.MaxEntities+1)}},
			"metadata keys":    {{ID: "a", Metadata: manyKeys}},
			"metadata bytes":   {{ID: "a", Metadata: map[string]any{"k": strings.Repeat("x", search.MaxMetadataBytes)}}},
		} {
			if _, err := s.Upsert(ctx, tenant, coll, items); !errors.As(err, &big) {
				t.Errorf("%s: want TooLargeError, got %T %v", name, err, err)
			}
		}
		for name, items := range map[string][]search.Item{
			"no items":     nil,
			"no id":        {{Text: "x"}},
			"repeated id":  {{ID: "a", Text: "x"}, {ID: "a", Text: "y"}},
			"NUL in an id": {{ID: "a\x00b", Text: "x"}},
		} {
			if _, err := s.Upsert(ctx, tenant, coll, items); !errors.As(err, &bad) {
				t.Errorf("%s: want InvalidArgError, got %T %v", name, err, err)
			}
		}
		if _, err := s.Query(ctx, tenant, coll, strings.Repeat("x ", search.MaxQueryBytes), 5, search.Filter{}); !errors.As(err, &big) {
			t.Errorf("query bytes: want TooLargeError, got %T %v", err, err)
		}
		if _, err := s.Delete(ctx, tenant, coll, search.Selector{IDs: make([]string, search.MaxIDsPerDelete+1)}); !errors.As(err, &big) {
			t.Errorf("ids per delete: want TooLargeError, got %T %v", err, err)
		}
		// A refused upsert wrote nothing: all or nothing.
		if c, _, _ := s.DescribeCollection(ctx, tenant, coll); c.Records != 0 {
			t.Errorf("a refused upsert left %d records", c.Records)
		}
		// The limit is clamped, never an error; zero takes the default.
		var fill []search.Item
		for i := 0; i < search.MaxLimit+20; i++ {
			fill = append(fill, search.Item{ID: fmt.Sprintf("r%03d", i), Text: "repeated words"})
		}
		upsert(t, tenant, coll, fill...)
		for limit, want := range map[int]int{0: search.DefaultLimit, -3: search.DefaultLimit, 7: 7, 100000: search.MaxLimit} {
			if got := query(t, tenant, coll, "repeated", limit, search.Filter{}); len(got) != want {
				t.Errorf("limit %d returned %d hits, want %d", limit, len(got), want)
			}
		}
		// Best first, ranked from 1 with no gaps.
		for i, h := range query(t, tenant, coll, "repeated", 12, search.Filter{}) {
			if h.Rank != i+1 {
				t.Fatalf("hit %d has rank %d", i, h.Rank)
			}
		}
	})

	t.Run("ListAndDropCollection", func(t *testing.T) {
		const tn = "lister"
		if got, err := s.ListCollections(ctx, tn); err != nil || len(got) != 0 {
			t.Fatalf("ListCollections on a new tenant = %v, %v", got, err)
		}
		ensure(t, tn, "zebra")
		ensure(t, tn, "apple")
		upsert(t, tn, "apple", search.Item{ID: "1", Text: "a"}, search.Item{ID: "2", Text: "b"})
		got, err := s.ListCollections(ctx, tn)
		if err != nil || len(got) != 2 || got[0].Name != "apple" || got[1].Name != "zebra" || got[0].Records != 2 {
			t.Fatalf("ListCollections = %+v, %v", got, err)
		}
		if n, err := s.DropCollection(ctx, tn, "apple"); err != nil || n != 2 {
			t.Fatalf("DropCollection = %d, %v; want 2", n, err)
		}
		settle(t, tn, "apple")
		if _, found, _ := s.DescribeCollection(ctx, tn, "apple"); found {
			t.Errorf("a dropped collection is still described")
		}
		if n, err := s.DropCollection(ctx, tn, "apple"); err != nil || n != 0 {
			t.Errorf("dropping a missing collection = %d, %v; want 0, nil", n, err)
		}
		// The name is free again, and comes back empty.
		ensure(t, tn, "apple")
		if got := query(t, tn, "apple", "a b", 5, search.Filter{}); len(got) != 0 {
			t.Errorf("a recreated collection kept records: %v", ids(got))
		}
	})

	// Names are data. Nothing a tenant or an author types may reach the
	// filesystem or collide with another name.
	t.Run("HostileNames", func(t *testing.T) {
		names := []string{"../../etc/passwd", "a/b", "a\\b", "with space", "ünïcödé", strings.Repeat("n", search.MaxNameBytes), ".", ".."}
		for i, name := range names {
			ensure(t, "ten/../ant", name)
			upsert(t, "ten/../ant", name, search.Item{ID: "only", Text: fmt.Sprintf("marker%d", i)})
		}
		for i, name := range names {
			if got := query(t, "ten/../ant", name, fmt.Sprintf("marker%d", i), 5, search.Filter{}); len(got) != 1 {
				t.Errorf("collection %q lost or shared its record: %v", name, ids(got))
			}
		}
		var bad *search.InvalidArgError
		if err := s.EnsureCollection(ctx, tenant, search.Collection{Name: ""}); !errors.As(err, &bad) {
			t.Errorf("empty name: want InvalidArgError, got %T %v", err, err)
		}
		var big *search.TooLargeError
		if err := s.EnsureCollection(ctx, tenant, search.Collection{Name: strings.Repeat("n", search.MaxNameBytes+1)}); !errors.As(err, &big) {
			t.Errorf("long name: want TooLargeError, got %T %v", err, err)
		}
	})
}

func cond(field string, op vector.Op, v any) search.Filter {
	return search.Filter{Conditions: []search.Condition{{Field: field, Op: op, Value: v}}}
}

func eq(field string, v any) search.Filter { return cond(field, vector.OpEq, v) }

func and(fs ...search.Filter) search.Filter {
	var out search.Filter
	for _, f := range fs {
		out.Conditions = append(out.Conditions, f.Conditions...)
	}
	return out
}

func ids(hits []search.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func sameIDs(got, want []string) bool {
	if len(got) == 0 && len(want) == 0 {
		return true
	}
	return reflect.DeepEqual(got, want)
}
