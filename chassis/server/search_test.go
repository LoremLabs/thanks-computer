package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/search/blevestore"
)

// sctx builds the request context the txco://search handlers read: a trusted
// tenant scope + the WITH-clause meta (same shape ExecCore plumbs in prod).
func sctx(meta string) context.Context {
	return operation.WithMeta(processor.WithTenant(context.Background(), "acme"), meta)
}

func newSearchStore(t *testing.T) search.Store {
	t.Helper()
	ss, err := blevestore.New(filepath.Join(t.TempDir(), "search"), 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	return ss
}

func TestSearchOpsEndToEnd(t *testing.T) {
	ss := newSearchStore(t)
	in := []byte("{}")

	pl, _ := searchCollection(sctx(`{"collection":"pony-paris"}`), ss, in)
	if got := gjson.Get(pl.Raw, "_search.collection.analyzer_version").String(); got != blevestore.AnalyzerVersion {
		t.Fatalf("collection = %s", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "_search.collection.scoring_model").String(); got != "bm25" {
		t.Fatalf("scoring model = %q", got)
	}

	// The items array a stack builds for vector/upsert, handed over as it is:
	// the unknown `vector` key is ignored.
	pl, _ = searchUpsert(sctx(`{"collection":"pony-paris","items":[
	  {"id":"paris:dr_1:0","vector":[0.1,0.2],"text":"Luggage can be left in the hall cupboard before check-in.","name":"faq.md","title":"FAQ","metadata":{"slug":"paris","doc_key":"dr_1","idx":0,"audience":"anyone"}},
	  {"id":"paris:dr_1:1","text":"The pool opens at nine and closes at six.","name":"faq.md","title":"FAQ","metadata":{"slug":"paris","doc_key":"dr_1","idx":1,"audience":"anyone"}},
	  {"id":"paris:dr_2:0","text":"Ticket TXC-4821 tracks the broken pool pump.","name":"tickets.md","entities":["Marc Dupont"],"metadata":{"slug":"paris","doc_key":"dr_2","idx":0,"audience":"team"}}
	]}`), ss, in)
	if got := gjson.Get(pl.Raw, "_search.upserted").Int(); got != 3 {
		t.Fatalf("upserted = %s", pl.Raw)
	}
	// A single record from top-level keys.
	pl, _ = searchUpsert(sctx(`{"collection":"pony-paris","id":"paris:dr_3:0","text":"Parking is behind the building.","metadata":{"slug":"paris","doc_key":"dr_3","idx":0,"audience":"anyone"}}`), ss, in)
	if got := gjson.Get(pl.Raw, "_search.upserted").Int(); got != 1 {
		t.Fatalf("single upsert = %s", pl.Raw)
	}

	// The answer path's query: a whole message, the production filter, `into`.
	pl, _ = searchQuery(sctx(`{"collection":"pony-paris","query":"Hi! What is TXC-4821 about, and when does the pool open?","limit":6,
	  "filter":{"slug":"paris","audience":"anyone"},"into":"_lex.matches"}`), ss, in)
	hits := gjson.Get(pl.Raw, "_lex.matches")
	if got := matchIDs(hits); len(got) == 0 || got[0] != "paris:dr_1:1" {
		t.Fatalf("filtered query = %s", pl.Raw)
	}
	for _, h := range hits.Array() {
		if h.Get("metadata.audience").String() != "anyone" {
			t.Errorf("the filter admitted %s", h.Raw)
		}
		if h.Get("score").Exists() || !h.Get("rank").Exists() || h.Get("text").String() == "" {
			t.Errorf("hit shape = %s", h.Raw)
		}
	}
	// Without the audience filter the identifier finds its ticket first.
	pl, _ = searchQuery(sctx(`{"collection":"pony-paris","query":"what is TXC-4821 about?"}`), ss, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_search.hits")); len(got) == 0 || got[0] != "paris:dr_2:0" {
		t.Fatalf("identifier query = %s", pl.Raw)
	}
	// No hits is an empty array, not an error and not a missing key.
	pl, _ = searchQuery(sctx(`{"collection":"pony-paris","query":"zebra"}`), ss, in)
	if r := gjson.Get(pl.Raw, "_search.hits"); !r.IsArray() || len(r.Array()) != 0 || gjson.Get(pl.Raw, "search.error").Exists() {
		t.Fatalf("empty query = %s", pl.Raw)
	}

	// update: a re-label by merge, and a rename by fields.
	pl, _ = searchUpdate(sctx(`{"collection":"pony-paris","filter":{"slug":"paris","doc_key":"dr_2"},"merge":{"audience":"anyone"},"fields":{"name":"maintenance.md"},"into":"_relabel"}`), ss, in)
	if got := gjson.Get(pl.Raw, "_relabel").Int(); got != 1 {
		t.Fatalf("update = %s", pl.Raw)
	}
	pl, _ = searchQuery(sctx(`{"collection":"pony-paris","query":"maintenance.md","filter":{"audience":"anyone"}}`), ss, in)
	if got := matchIDs(gjson.Get(pl.Raw, "_search.hits")); len(got) == 0 || got[0] != "paris:dr_2:0" {
		t.Fatalf("after update = %s", pl.Raw)
	}

	// delete: the stale tail by filter, then by id.
	pl, _ = searchDelete(sctx(`{"collection":"pony-paris","filter":{"slug":"paris","doc_key":"dr_1","idx":{"gte":1}}}`), ss, in)
	if got := gjson.Get(pl.Raw, "_search.deleted").Int(); got != 1 {
		t.Fatalf("delete by filter = %s", pl.Raw)
	}
	pl, _ = searchDelete(sctx(`{"collection":"pony-paris","ids":["paris:dr_3:0","never-existed"],"into":"_gone"}`), ss, in)
	if got := gjson.Get(pl.Raw, "_gone").Int(); got != 1 {
		t.Fatalf("delete by ids = %s", pl.Raw)
	}
	pl, _ = searchCollection(sctx(`{"collection":"pony-paris"}`), ss, in)
	if got := gjson.Get(pl.Raw, "_search.collection.records").Int(); got != 2 {
		t.Fatalf("records = %s", pl.Raw)
	}
}

func TestSearchErrorsSurfaceOnEnvelope(t *testing.T) {
	ss := newSearchStore(t)
	in := []byte("{}")
	searchCollection(sctx(`{"collection":"c"}`), ss, in)

	type handler = func(ctx context.Context, ss search.Store, in []byte) (string, error)
	wrap := func(f func(context.Context, search.Store, []byte) (event.Payload, error)) handler {
		return func(ctx context.Context, ss search.Store, in []byte) (string, error) {
			pl, err := f(ctx, ss, in)
			return pl.Raw, err
		}
	}
	for _, tc := range []struct {
		name string
		h    handler
		meta string
		code string
	}{
		{"no collection", wrap(searchQuery), `{"query":"x"}`, "txco_search_invalid_arg"},
		{"no query", wrap(searchQuery), `{"collection":"c"}`, "txco_search_invalid_arg"},
		{"unknown collection", wrap(searchQuery), `{"collection":"nope","query":"x"}`, "txco_search_collection_not_found"},
		{"unknown filter op", wrap(searchQuery), `{"collection":"c","query":"x","filter":{"a":{"like":"b"}}}`, "txco_search_invalid_arg"},
		{"no items", wrap(searchUpsert), `{"collection":"c"}`, "txco_search_invalid_arg"},
		{"item without id", wrap(searchUpsert), `{"collection":"c","items":[{"text":"x"}]}`, "txco_search_invalid_arg"},
		{"delete names nothing", wrap(searchDelete), `{"collection":"c"}`, "txco_search_invalid_arg"},
		{"delete names both", wrap(searchDelete), `{"collection":"c","id":"a","filter":{"k":"v"}}`, "txco_search_invalid_arg"},
		{"update without filter", wrap(searchUpdate), `{"collection":"c","merge":{"k":"v"}}`, "txco_search_invalid_arg"},
		{"update changes nothing", wrap(searchUpdate), `{"collection":"c","filter":{"k":"v"}}`, "txco_search_invalid_arg"},
		{"update sets text", wrap(searchUpdate), `{"collection":"c","filter":{"k":"v"},"fields":{"text":"new"}}`, "txco_search_invalid_arg"},
		{"analyzer mismatch", wrap(searchCollection), `{"collection":"c","analyzer_version":"other"}`, "txco_search_analyzer_mismatch"},
	} {
		raw, err := tc.h(sctx(tc.meta), ss, in)
		if err != nil {
			t.Errorf("%s: handler returned an error (%v); a store failure is data on the envelope", tc.name, err)
		}
		if got := gjson.Get(raw, "search.error.code").String(); got != tc.code {
			t.Errorf("%s: code = %q, want %q (%s)", tc.name, got, tc.code, raw)
		}
		if gjson.Get(raw, "vector.error").Exists() {
			t.Errorf("%s: a search op reported a vector error: %s", tc.name, raw)
		}
	}

	// Too large.
	big := make([]byte, search.MaxTextBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	pl, _ := searchUpsert(sctx(`{"collection":"c","id":"a","text":"`+string(big)+`"}`), ss, in)
	if got := gjson.Get(pl.Raw, "search.error.code").String(); got != "txco_search_too_large" {
		t.Errorf("too large: %s", got)
	}

	// No tenant in scope.
	pl, _ = searchQuery(operation.WithMeta(context.Background(), `{"collection":"c","query":"x"}`), ss, in)
	if got := gjson.Get(pl.Raw, "search.error.code").String(); got != search.CodeNoTenant {
		t.Errorf("no tenant: %s", pl.Raw)
	}
}

// With no store the ops still answer, with a code a stack can branch on.
func TestSearchDisabled(t *testing.T) {
	in := []byte("{}")
	for name, h := range map[string]func(context.Context, search.Store, []byte) (event.Payload, error){
		"collection": searchCollection, "upsert": searchUpsert, "query": searchQuery, "delete": searchDelete, "update": searchUpdate,
	} {
		pl, err := h(sctx(`{"collection":"c","query":"x","id":"a","text":"t"}`), nil, in)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := gjson.Get(pl.Raw, "search.error.code").String(); got != search.CodeDisabled {
			t.Errorf("%s: code = %q, want %q", name, got, search.CodeDisabled)
		}
	}
}

// A reserved `into` falls back to the op's default, as for every txco:// op.
func TestSearchIntoCannotForgeControlFields(t *testing.T) {
	ss := newSearchStore(t)
	in := []byte("{}")
	searchCollection(sctx(`{"collection":"c"}`), ss, in)
	searchUpsert(sctx(`{"collection":"c","id":"a","text":"hello world"}`), ss, in)
	pl, _ := searchQuery(sctx(`{"collection":"c","query":"hello","into":"_txc.tenant"}`), ss, in)
	if gjson.Get(pl.Raw, "_txc").Exists() || len(gjson.Get(pl.Raw, "_search.hits").Array()) != 1 {
		t.Fatalf("reserved into = %s", pl.Raw)
	}
}

// A whole email is a fine query. One past the byte limit is cut on a rune
// boundary and still answered, never refused.
func TestSearchQueryClipsAnOverlongQuery(t *testing.T) {
	ss := newSearchStore(t)
	in := []byte("{}")
	searchCollection(sctx(`{"collection":"c"}`), ss, in)
	searchUpsert(sctx(`{"collection":"c","id":"a","text":"the boiler manual"}`), ss, in)

	long := "where is the boiler manual? " + strings.Repeat("é", search.MaxQueryBytes) // 2 bytes each: the cut lands mid-rune
	meta, _ := json.Marshal(map[string]any{"collection": "c", "query": long})
	pl, _ := searchQuery(sctx(string(meta)), ss, in)
	if gjson.Get(pl.Raw, "search.error").Exists() {
		t.Fatalf("an over-long query was refused: %s", pl.Raw)
	}
	if got := matchIDs(gjson.Get(pl.Raw, "_search.hits")); len(got) != 1 || got[0] != "a" {
		t.Fatalf("hits = %s", pl.Raw)
	}
	if c := clipQuery(long); len(c) > search.MaxQueryBytes || !utf8.ValidString(c) {
		t.Fatalf("clip: %d bytes, valid=%v", len(c), utf8.ValidString(c))
	}
	if c := clipQuery("short"); c != "short" {
		t.Fatalf("clip changed a short query: %q", c)
	}
}
