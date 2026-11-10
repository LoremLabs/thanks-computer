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
	// upsert with the vectors paired by index (items without their own):
	// the same three ids, re-labelled, so nothing below changes shape.
	pl, _ = vectorUpsert(vctx(`{"collection":"books","items":[
		{"id":"a","metadata":{"genre":"adventure","age":9},"text":"alpha"},
		{"id":"b","metadata":{"genre":"cozy","age":50}},
		{"id":"c","vector":[0.9,0.1,0],"metadata":{"genre":"adventure","age":12}}],
		"vectors":[[1,0,0],[0,1,0],[0,0,1]]}`), vs, in)
	if got := gjson.Get(pl.Raw, "_vector.upserted").Int(); got != 3 {
		t.Fatalf("paired upsert=%d, want 3 (%s)", got, pl.Raw)
	}
	// c kept its own vector over the paired one: it still ranks with a.
	pl, _ = vectorSearch(vctx(`{"collection":"books","vector":[1,0,0],"limit":2}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.matches")); !sameStrSet(got, []string{"a", "c"}) {
		t.Fatalf("paired upsert ranking: %v want [a c]", got)
	}
	for _, meta := range []string{
		`{"collection":"books","items":[{"id":"a"},{"id":"b"}],"vectors":[[1,0,0]]}`,
		`{"collection":"books","items":[{"id":"a"}],"vectors":"nope"}`,
		`{"collection":"books","items":[{"id":"a"}]}`,
	} {
		pl, _ = vectorUpsert(vctx(meta), vs, in)
		if got := gjson.Get(pl.Raw, "vector.error.code").String(); got != "txco_vector_invalid_arg" {
			t.Fatalf("%s → %q, want txco_vector_invalid_arg", meta, got)
		}
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

	// update: re-label by filter, custom into; vectors untouched
	pl, _ = vectorUpdate(vctx(`{"collection":"books","filter":{"genre":"adventure"},"merge":{"audience":"anyone","genre":null},"into":"._retag.anyone"}`), vs, in)
	if got := gjson.Get(pl.Raw, "_retag.anyone").Int(); got != 2 || gjson.Get(pl.Raw, "_vector.updated").Exists() {
		t.Fatalf("update: want 2 at custom into only (%s)", pl.Raw)
	}
	pl, _ = vectorSearch(vctx(`{"collection":"books","vector":[1,0,0],"filter":{"audience":"anyone"}}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.matches")); !sameStrSet(got, []string{"a", "c"}) || got[0] != "a" {
		t.Fatalf("after update: %v want [a c], a first", got)
	}
	if gjson.Get(pl.Raw, "_vector.matches.0.metadata.genre").Exists() || gjson.Get(pl.Raw, "_vector.matches.0.metadata.age").Int() == 0 {
		t.Fatalf("update must drop genre and keep age (%s)", pl.Raw)
	}
	// update refusals: no filter, no set
	for _, meta := range []string{
		`{"collection":"books","merge":{"x":1}}`,
		`{"collection":"books","filter":{"genre":"cozy"}}`,
		`{"collection":"books","filter":{"genre":"cozy"},"merge":{}}`,
	} {
		pl, _ = vectorUpdate(vctx(meta), vs, in)
		if got := gjson.Get(pl.Raw, "vector.error.code").String(); got != "txco_vector_invalid_arg" {
			t.Fatalf("%s → %q, want txco_vector_invalid_arg", meta, got)
		}
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

// TestVectorGet drives txco://vector/get through its handler: items read
// back by ids and by filter with no query vector, the answer at `into`, the
// vector never in it, and every refusal on the envelope.
func TestVectorGet(t *testing.T) {
	vs, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer vs.Close()
	in := []byte("{}")
	vectorCollection(vctx(`{"collection":"chunks","dimensions":3}`), vs, in)
	pl, _ := vectorUpsert(vctx(`{"collection":"chunks","items":[
		{"id":"p:doc:0","vector":[1,0,0],"metadata":{"slug":"p","doc":"notes.md","idx":0,"audience":"anyone"},"text":"zero"},
		{"id":"p:doc:1","vector":[0,1,0],"metadata":{"slug":"p","doc":"notes.md","idx":1,"audience":"anyone"},"text":"one"},
		{"id":"p:doc:2","vector":[0,0,1],"metadata":{"slug":"p","doc":"notes.md","idx":2,"audience":"anyone"},"text":"two"},
		{"id":"p:doc:3","vector":[1,1,0],"metadata":{"slug":"p","doc":"notes.md","idx":3,"audience":"owner"},"text":"private"}]}`), vs, in)
	if got := gjson.Get(pl.Raw, "_vector.upserted").Int(); got != 4 {
		t.Fatalf("upserted=%d (%s)", got, pl.Raw)
	}

	// By ids, at a custom into: the order asked, missing listed, no vector.
	pl, e := vectorGet(vctx(`{"collection":"chunks","ids":["p:doc:2","p:doc:9","p:doc:0"],"into":"_near"}`), vs, in)
	if e != nil {
		t.Fatalf("get: %v", e)
	}
	if got := matchIDs(gjson.Get(pl.Raw, "_near.items")); len(got) != 2 || got[0] != "p:doc:2" || got[1] != "p:doc:0" {
		t.Fatalf("by ids: %v, want [p:doc:2 p:doc:0] (%s)", got, pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "_near.missing").Raw; got != `["p:doc:9"]` {
		t.Fatalf("missing=%s, want [\"p:doc:9\"]", got)
	}
	if gjson.Get(pl.Raw, "_near.more").Bool() || gjson.Get(pl.Raw, "_near.items.0.vector").Exists() || gjson.Get(pl.Raw, "_near.items.0.distance").Exists() {
		t.Fatalf("by ids: no more, no vector, no distance (%s)", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "_near.items.0.text").String(); got != "two" {
		t.Fatalf("text=%q, want two", got)
	}
	if got := gjson.Get(pl.Raw, "_near.items.0.metadata.idx").Int(); got != 2 {
		t.Fatalf("metadata.idx=%d, want 2", got)
	}

	// A single id, nothing missing: `missing` is still there, empty.
	pl, _ = vectorGet(vctx(`{"collection":"chunks","id":"p:doc:1"}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.got.items")); len(got) != 1 || got[0] != "p:doc:1" {
		t.Fatalf("one id: %v (%s)", got, pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "_vector.got.missing").Raw; got != "[]" {
		t.Fatalf("one id: missing=%q, want [] (%s)", got, pl.Raw)
	}

	// By filter, the way a passage read asks for its neighbours: the lane's
	// audience filter keeps the private chunk out; limit and after page.
	pl, _ = vectorGet(vctx(`{"collection":"chunks","filter":{"slug":"p","doc":"notes.md","idx":{"in":[1,2,3]},"audience":"anyone"}}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.got.items")); len(got) != 2 || got[0] != "p:doc:1" || got[1] != "p:doc:2" {
		t.Fatalf("neighbours: %v, want [p:doc:1 p:doc:2] (%s)", got, pl.Raw)
	}
	if gjson.Get(pl.Raw, "_vector.got.missing").Exists() {
		t.Fatalf("a filter get has no missing (%s)", pl.Raw)
	}
	pl, _ = vectorGet(vctx(`{"collection":"chunks","filter":{"doc":"notes.md"},"limit":2,"after":"p:doc:0"}`), vs, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_vector.got.items")); len(got) != 2 || got[0] != "p:doc:1" || got[1] != "p:doc:2" || !gjson.Get(pl.Raw, "_vector.got.more").Bool() {
		t.Fatalf("page: %v more=%v, want [p:doc:1 p:doc:2] true (%s)", got, gjson.Get(pl.Raw, "_vector.got.more").Bool(), pl.Raw)
	}
	// A filter that matches nothing answers an empty list, not null.
	pl, _ = vectorGet(vctx(`{"collection":"chunks","filter":{"doc":"other.md"}}`), vs, in)
	if got := gjson.Get(pl.Raw, "_vector.got.items").Raw; got != "[]" {
		t.Fatalf("no match: items=%q, want [] (%s)", got, pl.Raw)
	}

	// Refusals surface on the envelope, never as a Go error.
	for _, tc := range []struct{ name, meta, code string }{
		{"no collection", `{"ids":["a"]}`, "txco_vector_invalid_arg"},
		{"neither", `{"collection":"chunks"}`, "txco_vector_invalid_arg"},
		{"both", `{"collection":"chunks","ids":["a"],"filter":{"doc":"x"}}`, "txco_vector_invalid_arg"},
		{"an empty filter", `{"collection":"chunks","filter":{}}`, "txco_vector_invalid_arg"},
		{"empty ids", `{"collection":"chunks","ids":[]}`, "txco_vector_invalid_arg"},
		{"ids not an array", `{"collection":"chunks","ids":"a"}`, "txco_vector_invalid_arg"},
		{"limit zero", `{"collection":"chunks","filter":{"doc":"x"},"limit":0}`, "txco_vector_invalid_arg"},
		{"limit too large", `{"collection":"chunks","filter":{"doc":"x"},"limit":501}`, "txco_vector_invalid_arg"},
		{"limit with ids", `{"collection":"chunks","ids":["a"],"limit":5}`, "txco_vector_invalid_arg"},
		{"unknown op", `{"collection":"chunks","filter":{"idx":{"near":1}}}`, "txco_vector_invalid_arg"},
		{"missing collection", `{"collection":"ghost","ids":["a"]}`, "txco_vector_collection_not_found"},
	} {
		pl, e := vectorGet(vctx(tc.meta), vs, in)
		if e != nil {
			t.Fatalf("%s: want an envelope error, got %v", tc.name, e)
		}
		if got := gjson.Get(pl.Raw, "vector.error.code").String(); got != tc.code {
			t.Fatalf("%s: code=%q, want %s (%s)", tc.name, got, tc.code, pl.Raw)
		}
	}
	// No tenant in scope.
	pl, _ = vectorGet(operation.WithMeta(context.Background(), `{"collection":"chunks","ids":["a"]}`), vs, in)
	if got := gjson.Get(pl.Raw, "vector.error.code").String(); got != "txco_vector_no_tenant" {
		t.Fatalf("no tenant: code=%q (%s)", got, pl.Raw)
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
