package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/embed"
	_ "github.com/loremlabs/thanks-computer/chassis/embed/ollama" // registers the "ollama" backend
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/vector"
	"github.com/loremlabs/thanks-computer/chassis/vector/sqlitevec"
)

// vctx builds the request context the txco://vector handlers read: a trusted
// tenant scope + the WITH-clause meta (same shape ExecCore plumbs in prod).
func vctx(meta string) context.Context {
	return operation.WithMeta(processor.WithTenant(context.Background(), "acme"), meta)
}

func matchIDs(arr gjson.Result) []string {
	var out []string
	for _, m := range arr.Array() {
		out = append(out, m.Get("id").String())
	}
	return out
}

func sameStrSet(got, want []string) bool {
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

func TestVectorOpsEndToEnd(t *testing.T) {
	vs, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer vs.Close()
	in := []byte("{}")

	// create collection
	pl, _ := vectorCollection(vctx(`{"collection":"books","embedding_model":"nomic-embed-text","dimensions":3,"metric":"cosine"}`), vs, in)
	if got := gjson.Get(pl.Raw, "_vector.collection.dimensions").Int(); got != 3 {
		t.Fatalf("collection dims=%d, want 3 (%s)", got, pl.Raw)
	}

	// upsert batch
	pl, _ = vectorUpsert(vctx(`{"collection":"books","items":[
		{"id":"a","vector":[1,0,0],"metadata":{"genre":"adventure","age":9},"text":"alpha"},
		{"id":"b","vector":[0,1,0],"metadata":{"genre":"cozy","age":50}},
		{"id":"c","vector":[0.9,0.1,0],"metadata":{"genre":"adventure","age":12}}]}`), vs, in)
	if got := gjson.Get(pl.Raw, "_vector.upserted").Int(); got != 3 {
		t.Fatalf("upserted=%d, want 3 (%s)", got, pl.Raw)
	}

	// search → custom `into` path (the multiple-ops-don't-collide requirement)
	pl, _ = vectorSearch(vctx(`{"collection":"books","vector":[1,0,0],"limit":10,"into":".myresults"}`), vs, in)
	res := gjson.Get(pl.Raw, "myresults")
	if !res.IsArray() || len(res.Array()) != 3 {
		t.Fatalf("custom into: want 3 matches at myresults (%s)", pl.Raw)
	}
	if matchIDs(res)[0] != "a" {
		t.Fatalf("ranking: first=%s want a", matchIDs(res)[0])
	}
	if gjson.Get(pl.Raw, "_vector.matches").Exists() {
		t.Fatal("custom into must not also write default _vector.matches")
	}

	// search → default into + eq filter
	pl, _ = vectorSearch(vctx(`{"collection":"books","vector":[1,0,0],"filter":{"genre":"adventure"}}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.matches")); !sameStrSet(got, []string{"a", "c"}) {
		t.Fatalf("eq filter: %v want [a c]", got)
	}

	// range filter
	pl, _ = vectorSearch(vctx(`{"collection":"books","vector":[1,0,0],"filter":{"age":{"gte":12}}}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.matches")); !sameStrSet(got, []string{"b", "c"}) {
		t.Fatalf("gte filter: %v want [b c]", got)
	}

	// exclude-by-id (already-read pattern)
	pl, _ = vectorSearch(vctx(`{"collection":"books","vector":[1,0,0],"filter":{"id":{"not_in":["a"]}}}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.matches")); !sameStrSet(got, []string{"b", "c"}) {
		t.Fatalf("id not_in: %v want [b c]", got)
	}

	// delete
	pl, _ = vectorDelete(vctx(`{"collection":"books","ids":["a"]}`), vs, in)
	if got := gjson.Get(pl.Raw, "_vector.deleted").Int(); got != 1 {
		t.Fatalf("deleted=%d, want 1 (%s)", got, pl.Raw)
	}
}

func TestVectorErrorsSurfaceOnEnvelope(t *testing.T) {
	vs, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer vs.Close()
	in := []byte("{}")

	// missing collection arg → invalid_arg on the envelope, not a hard error
	pl, e := vectorSearch(vctx(`{"vector":[1,0,0]}`), vs, in)
	if e != nil {
		t.Fatalf("want nil err (envelope-surfaced), got %v", e)
	}
	if got := gjson.Get(pl.Raw, "vector.error.code").String(); got != "txco_vector_invalid_arg" {
		t.Fatalf("error code=%q, want txco_vector_invalid_arg (%s)", got, pl.Raw)
	}

	// search a collection that doesn't exist → collection_not_found
	pl, _ = vectorSearch(vctx(`{"collection":"ghost","vector":[1,0,0]}`), vs, in)
	if got := gjson.Get(pl.Raw, "vector.error.code").String(); got != "txco_vector_collection_not_found" {
		t.Fatalf("error code=%q, want txco_vector_collection_not_found (%s)", got, pl.Raw)
	}
}

// TestEmbedUpsertSearchLive is the MVP vertical end-to-end: embed real text
// with local nomic (768-dim), store the vectors, and confirm a semantic query
// ranks the on-topic document first. Skipped when Ollama isn't reachable.
func TestEmbedUpsertSearchLive(t *testing.T) {
	ctx := context.Background()
	b, err := embed.Open("ollama", embed.Config{HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("embed open: %v", err)
	}
	docs, err := b.Embed(ctx, embed.Request{Texts: []string{
		"search_document: A snowy wilderness survival story about a wolf-dog in the Yukon.",
		"search_document: A cozy seaside romance about a village baker.",
	}}, nil)
	if err != nil {
		if _, net := err.(*embed.ProviderNetError); net {
			t.Skipf("ollama not reachable: %v", err)
		}
		t.Fatalf("embed docs: %v", err)
	}

	vs, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer vs.Close()

	if err := vs.EnsureCollection(ctx, "acme", vector.Collection{
		Name: "books", EmbeddingModel: "nomic-embed-text", Dimensions: docs.Dimensions, Metric: vector.MetricCosine,
	}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := vs.Upsert(ctx, "acme", "books", []vector.Item{
		{ID: "wolf", Vector: docs.Vectors[0], Text: "wolf"},
		{ID: "romance", Vector: docs.Vectors[1], Text: "romance"},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	q, err := b.Embed(ctx, embed.Request{Texts: []string{"search_query: adventure with wolves in the snow"}}, nil)
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	matches, err := vs.Search(ctx, "acme", "books", q.Vectors[0], 2, vector.Filter{})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(matches) != 2 || matches[0].ID != "wolf" {
		t.Fatalf("semantic ranking wrong: want wolf first, got %v", matches)
	}
	t.Logf("live vertical OK: dim=%d, top=%s (score %.3f)", docs.Dimensions, matches[0].ID, matches[0].Score)
}
