package blevestore

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

func newTestCollection(t *testing.T) *Collection {
	t.Helper()
	c, err := OpenCollection(filepath.Join(t.TempDir(), "idx"))
	if err != nil {
		t.Fatalf("OpenCollection: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func hitIDs(hits []search.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func TestAnalyzerTokens(t *testing.T) {
	c := newTestCollection(t)
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"TXC-4821", []string{"txc", "4821"}},
		{"RFC-5321", []string{"rfc", "5321"}},
		{"matt@example.com", []string{"matt", "example", "com"}},
		{"onepony-parser", []string{"onepony", "parser"}},
		{"foo.bar.baz", []string{"foo", "bar", "baz"}},
		{"invoice_92812", []string{"invoice", "92812"}},
		{"2026-09-21", []string{"2026", "09", "21"}},
		{"github.com/foo/bar", []string{"github", "com", "foo", "bar"}},
		{"Crème Brûlée", []string{"creme", "brulee"}},
		{"Running runs", []string{"running", "runs"}}, // no stemming
		{"the of and", []string{"the", "of", "and"}},  // no stop words
	} {
		if got := tokens(c.an, tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tokens(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParseQuery(t *testing.T) {
	c := newTestCollection(t)
	pq := parseQuery(c.an, `where is "termination for convenience" in TXC-4821, don't guess`)
	wantPhrases := [][]string{{"termination", "for", "convenience"}, {"txc", "4821"}}
	if !reflect.DeepEqual(pq.phrases, wantPhrases) {
		t.Errorf("phrases = %v, want %v", pq.phrases, wantPhrases)
	}
	wantTerms := []string{"where", "is", "termination", "for", "convenience", "in", "txc", "4821", "don", "t", "guess"}
	if !reflect.DeepEqual(pq.terms, wantTerms) {
		t.Errorf("terms = %v, want %v", pq.terms, wantTerms)
	}
}

// Every identifier in the design's conformance list must pull its own record
// to the top, past records that merely share one of its parts.
func TestIdentifiersRankFirst(t *testing.T) {
	c := newTestCollection(t)
	ids := []string{
		"TXC-4821", "RFC-5321", "matt@example.com", "onepony-parser",
		"foo.bar.baz", "invoice_92812", "2026-09-21", "github.com/foo/bar",
	}
	var items []search.Item
	for i, id := range ids {
		items = append(items, search.Item{
			ID:       id,
			Text:     "The record for " + id + " was filed by the operations desk after review.",
			Metadata: map[string]any{"n": i},
		})
	}
	// Distractors share parts of the identifiers, in other orders and places.
	items = append(items,
		search.Item{ID: "d1", Text: "TXC was founded in 4821 words of prose. RFC 9999 and 5321 lines of code."},
		search.Item{ID: "d2", Text: "Write to bar@foo.example or see baz.bar.foo; invoice 11111 and parser onepony."},
		search.Item{ID: "d3", Text: "On 21-09-2026 the com github bar foo listing was updated by matt."},
	)
	if err := c.Upsert(items, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for _, id := range ids {
		hits, err := c.Query(context.Background(), "what do we know about "+id+"?", 5, search.Filter{})
		if err != nil {
			t.Fatalf("Query(%q): %v", id, err)
		}
		if len(hits) == 0 || hits[0].ID != id {
			t.Errorf("Query(%q): top hit = %v, want %q first", id, hitIDs(hits), id)
		}
	}
}

func TestPhraseAndOrSemantics(t *testing.T) {
	c := newTestCollection(t)
	if err := c.Upsert([]search.Item{
		{ID: "clause", Text: "Either party may exercise termination for convenience on thirty days notice."},
		{ID: "scatter", Text: "For your convenience, the termination date is printed on the form. Convenience matters."},
		{ID: "other", Text: "Luggage can be left in the hall cupboard before check-in."},
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	hits, err := c.Query(context.Background(), `"termination for convenience"`, 5, search.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := hitIDs(hits); len(got) != 2 || got[0] != "clause" {
		t.Errorf("phrase query = %v, want [clause scatter]", got)
	}

	// OR semantics: a long natural-language question still finds the record
	// that shares only a few of its words.
	hits, err = c.Query(context.Background(), "hello, could you tell me where I might leave my luggage before we arrive?", 5, search.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := hitIDs(hits); len(got) == 0 || got[0] != "other" {
		t.Errorf("OR query = %v, want other first", got)
	}

	// Engine syntax is not recognised.
	hits, err = c.Query(context.Background(), `+luggage -cupboard text:hall*`, 5, search.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := hitIDs(hits); len(got) != 1 || got[0] != "other" {
		t.Errorf("syntax query = %v, want [other]", got)
	}

	if hits, _ := c.Query(context.Background(), `?!… --`, 5, search.Filter{}); len(hits) != 0 {
		t.Errorf("empty query returned %v", hitIDs(hits))
	}
}

func TestFieldsAndHitShape(t *testing.T) {
	c := newTestCollection(t)
	if err := c.Upsert([]search.Item{
		{ID: "a", Text: "General notes.", Name: "apollo-launch-plan.pdf", Metadata: map[string]any{"doc_key": "dr_1", "page": float64(7)}},
		{ID: "b", Text: "The apollo launch plan is mentioned once here in passing, among many other words about nothing much."},
		{ID: "c", Text: "Unrelated.", Entities: []string{"Marc Dupont", "Mozilla"}},
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	hits, err := c.Query(context.Background(), "apollo-launch-plan.pdf", 5, search.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "a" {
		t.Fatalf("filename query = %v, want a first", hitIDs(hits))
	}
	if hits[0].Rank != 1 || hits[0].Text != "General notes." || hits[0].Metadata["doc_key"] != "dr_1" || hits[0].Metadata["page"] != float64(7) {
		t.Errorf("hit shape = %+v", hits[0])
	}
	hits, _ = c.Query(context.Background(), "what did Marc Dupont say", 5, search.Filter{})
	if len(hits) == 0 || hits[0].ID != "c" {
		t.Errorf("entity query = %v, want c first", hitIDs(hits))
	}

	// Replace by id.
	if err := c.Upsert([]search.Item{{ID: "c", Text: "Now about gardening."}}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if hits, _ := c.Query(context.Background(), "Marc Dupont", 5, search.Filter{}); len(hits) != 0 {
		t.Errorf("replaced record still matches: %v", hitIDs(hits))
	}
	if n, _ := c.Count(); n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
}

func cond(field string, op vector.Op, v any) search.Filter {
	return search.Filter{Conditions: []search.Condition{{Field: field, Op: op, Value: v}}}
}

func TestFilters(t *testing.T) {
	c := newTestCollection(t)
	if err := c.Upsert([]search.Item{
		{ID: "p1", Text: "pool opening hours", Metadata: map[string]any{"slug": "paris", "audience": "anyone", "idx": float64(0), "date": "2026-09-21"}},
		{ID: "p2", Text: "pool opening hours", Metadata: map[string]any{"slug": "paris", "audience": "team", "idx": float64(1)}},
		{ID: "p3", Text: "pool opening hours", Metadata: map[string]any{"slug": "rome", "audience": "anyone", "idx": float64(2), "draft": true}},
		{ID: "p4", Text: "pool opening hours", Metadata: map[string]any{"slug": "paris", "idx": float64(3)}}, // no audience
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	and := func(fs ...search.Filter) search.Filter {
		var out search.Filter
		for _, f := range fs {
			out.Conditions = append(out.Conditions, f.Conditions...)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		f    search.Filter
		want []string
	}{
		{"none", search.Filter{}, []string{"p1", "p2", "p3", "p4"}},
		{"eq", cond("slug", vector.OpEq, "paris"), []string{"p1", "p2", "p4"}},
		{"eq+eq", and(cond("slug", vector.OpEq, "paris"), cond("audience", vector.OpEq, "anyone")), []string{"p1"}},
		{"eq is exact", cond("slug", vector.OpEq, "Paris"), nil},
		{"in", cond("audience", vector.OpIn, []any{"anyone", "team"}), []string{"p1", "p2", "p3"}},
		{"in empty", cond("audience", vector.OpIn, []any{}), nil},
		{"not_in passes absent", cond("audience", vector.OpNotIn, []any{"team"}), []string{"p1", "p3", "p4"}},
		{"not_in empty", cond("audience", vector.OpNotIn, []any{}), []string{"p1", "p2", "p3", "p4"}},
		{"gte", cond("idx", vector.OpGte, float64(2)), []string{"p3", "p4"}},
		{"lt", cond("idx", vector.OpLt, float64(1)), []string{"p1"}},
		{"number is not string", cond("idx", vector.OpEq, "1"), nil},
		{"bool", cond("draft", vector.OpEq, true), []string{"p3"}},
		{"date string stays a string", cond("date", vector.OpEq, "2026-09-21"), []string{"p1"}},
		{"id eq", cond("id", vector.OpEq, "p2"), []string{"p2"}},
		{"id not_in", cond("id", vector.OpNotIn, []any{"p1", "p2"}), []string{"p3", "p4"}},
	} {
		hits, err := c.Query(context.Background(), "pool hours", 10, tc.f)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := hitIDs(hits); !reflect.DeepEqual(got, tc.want) && !(len(got) == 0 && len(tc.want) == 0) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := c.Query(context.Background(), "pool", 10, cond("slug", vector.Op("like"), "x")); err == nil {
		t.Errorf("unsupported op: want an error")
	}
}

// A filter must not move the ranking: the same records come back in the same
// order whether or not a filter that admits them all is present.
func TestFilterDoesNotScore(t *testing.T) {
	c := newTestCollection(t)
	if err := c.Upsert([]search.Item{
		{ID: "a", Text: "pool pool pool hours", Metadata: map[string]any{"slug": "paris"}},
		{ID: "b", Text: "the pool has hours posted by the door near reception", Metadata: map[string]any{"slug": "paris"}},
		{ID: "c", Text: "hours", Metadata: map[string]any{"slug": "paris"}},
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	plain, _ := c.Query(context.Background(), "pool hours", 10, search.Filter{})
	filtered, _ := c.Query(context.Background(), "pool hours", 10, cond("slug", vector.OpEq, "paris"))
	if !reflect.DeepEqual(hitIDs(plain), hitIDs(filtered)) {
		t.Errorf("filter changed order: %v vs %v", hitIDs(plain), hitIDs(filtered))
	}
}

func TestReopenRefusesOtherAnalyzer(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	c, err := OpenCollection(dir)
	if err != nil {
		t.Fatalf("OpenCollection: %v", err)
	}
	if err := c.Upsert([]search.Item{{ID: "a", Text: "durable words"}}, map[string][]byte{string(keyAnalyzer): []byte("txco_v0")}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_ = c.Close()
	if _, err := OpenCollection(dir); err == nil {
		t.Fatalf("reopen under another analyzer version: want an error")
	}
}

// document.go relies on this: values share fieldAll as separate array
// elements, and a phrase must not match across two of them.
func TestPhraseDoesNotSpanValues(t *testing.T) {
	c := newTestCollection(t)
	if err := c.Upsert([]search.Item{
		{ID: "spans", Text: "hours are posted by the door", Title: "pool opening"},
		{ID: "intact", Text: "the pool opening hours are posted by the door"},
	}, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	pq := query.NewMatchPhraseQuery("opening hours")
	pq.Analyzer = AnalyzerVersion
	pq.SetField(fieldAll)
	res, err := c.idx.Search(bleve.NewSearchRequest(pq))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var got []string
	for _, h := range res.Hits {
		got = append(got, h.ID)
	}
	if !reflect.DeepEqual(got, []string{"intact"}) {
		t.Errorf("phrase across values matched: %v, want [intact]", got)
	}
}

// A long question full of common words must not bury the one rare word that
// matters. Scored per field, "we" and "use" look rare among headings and
// outrank it; matched in one combined field, they stay common. The corpus has
// to be big enough for "common" to mean something: in five records every word
// is rare.
func TestRareTermBeatsCommonWordsInShortFields(t *testing.T) {
	c := newTestCollection(t)
	items := []search.Item{{ID: "rare", Heading: "Isolation", Text: "Each machine is a Firecracker microVM with its own kernel, started on demand and stopped when idle, and billed only while it runs."}}
	headings := []string{"What we use", "Do we use it anywhere", "Where we use this", "How we use them"}
	for i := 0; i < 60; i++ {
		items = append(items, search.Item{
			ID:      fmt.Sprintf("common%02d", i),
			Heading: headings[i%len(headings)],
			Text:    fmt.Sprintf("Note %d. We do use this anywhere we can, and where we do not use it we say so. General notes about how the team works.", i),
		})
	}
	if err := c.Upsert(items, nil); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	hits, err := c.Query(context.Background(), "Do we use Firecracker anywhere?", 5, search.Filter{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(hits) == 0 || hits[0].ID != "rare" {
		t.Errorf("got %v, want rare first", hitIDs(hits))
	}
}
