package server

import (
	"context"
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/search"
)

// search.go holds the handler bodies for the lexical search ops
// (txco://search/{collection,upsert,query,delete,update}). Storage is the
// configured search backend (Bleve on local disk, bundled). This layer adds
// the txcl surface: WITH params in, JSON in/out of the envelope. It mirrors
// vector.go on purpose, so an author who knows one retrieval lane knows both.
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx) (the
// request-pinned tenant, NOT the mutable _txc.tenant). Like vector
// collections, search collections are tenant-level: one stack indexes, another
// queries.
//
// `filter` is the vector store's grammar, parsed by the same function, so one
// filter object narrows both lanes.
//
// Errors surface as a top-level `search.error` on the envelope (code +
// message, never values) so authors handle them uniformly with
// `WHEN @search.error EXEC ...`. Unlike the vector ops, these are registered
// even when no store is configured, and then answer txco_search_disabled: a
// stack that fuses two lanes must be able to branch on "no lexical lane here"
// instead of failing on an unknown op.

func searchErr(code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, "search.error.code", code)
	raw, _ = sjson.Set(raw, "search.error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func searchErrFrom(err error) event.Payload {
	code := search.CodeStore
	if c, ok := err.(search.CodedError); ok {
		code = c.Code()
	}
	return searchErr(code, err.Error())
}

// searchScope resolves what every handler needs first: a store, a tenant, the
// WITH clause and the collection name. A non-nil payload is the error to
// return.
func searchScope(ctx context.Context, ss search.Store) (tenant string, meta []byte, collection string, fail *event.Payload) {
	bad := func(code, msg string) (string, []byte, string, *event.Payload) {
		p := searchErr(code, msg)
		return "", nil, "", &p
	}
	if ss == nil {
		return bad(search.CodeDisabled, "lexical search is not enabled on this chassis (--search-store)")
	}
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return bad(search.CodeNoTenant, "no tenant in request scope")
	}
	meta = []byte(operation.MetaFromContext(ctx))
	collection = gjson.GetBytes(meta, "collection").String()
	if collection == "" {
		return bad("txco_search_invalid_arg", "missing `collection`")
	}
	return tenant, meta, collection, nil
}

// searchFilter parses `filter` with the vector lane's parser and re-codes its
// one error, so a search op never reports a txco_vector_* code.
func searchFilter(meta []byte) (search.Filter, error) {
	f, err := parseFilter(meta)
	if err != nil {
		return f, &search.InvalidArgError{Reason: err.Error()}
	}
	return f, nil
}

// searchCollection ensures a collection exists and describes it: its pinned
// analyzer and scoring model, and its record count. Idempotent. An optional
// `analyzer_version` asserts the pin and errors on a mismatch.
func searchCollection(ctx context.Context, ss search.Store, in []byte) (event.Payload, error) {
	tenant, meta, name, fail := searchScope(ctx, ss)
	if fail != nil {
		return *fail, nil
	}
	c := search.Collection{
		Name:            name,
		AnalyzerVersion: gjson.GetBytes(meta, "analyzer_version").String(),
		ScoringModel:    gjson.GetBytes(meta, "scoring_model").String(),
	}
	if err := ss.EnsureCollection(ctx, tenant, c); err != nil {
		return searchErrFrom(err), nil
	}
	got, _, err := ss.DescribeCollection(ctx, tenant, name)
	if err != nil {
		return searchErrFrom(err), nil
	}
	out, _ := json.Marshal(got)
	resp, _ := sjson.SetRaw(`{}`, "_search.collection", string(out))
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// searchUpsert writes one or more records. Accepts a batch `items` array, or a
// single record from top-level `id`/`text`/`title`/`heading`/`name`/
// `entities`/`metadata`. Keys an item carries beyond those are ignored, so the
// array a stack builds for txco://vector/upsert can be handed to both ops.
func searchUpsert(ctx context.Context, ss search.Store, in []byte) (event.Payload, error) {
	tenant, meta, name, fail := searchScope(ctx, ss)
	if fail != nil {
		return *fail, nil
	}
	var items []search.Item
	if arr := gjson.GetBytes(meta, "items"); arr.Exists() {
		if !arr.IsArray() {
			return searchErr("txco_search_invalid_arg", "`items` must be an array"), nil
		}
		for _, it := range arr.Array() {
			items = append(items, parseSearchItem(it))
		}
	} else if gjson.GetBytes(meta, "id").Exists() {
		items = []search.Item{parseSearchItem(gjson.ParseBytes(meta))}
	}
	if len(items) == 0 {
		return searchErr("txco_search_invalid_arg", "no items to upsert (supply `items` or `id`+`text`)"), nil
	}
	n, err := ss.Upsert(ctx, tenant, name, items)
	if err != nil {
		return searchErrFrom(err), nil
	}
	resp, _ := sjson.Set(`{}`, "_search.upserted", n)
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

func parseSearchItem(r gjson.Result) search.Item {
	it := search.Item{
		ID:       r.Get("id").String(),
		Text:     r.Get("text").String(),
		Title:    r.Get("title").String(),
		Heading:  r.Get("heading").String(),
		Name:     r.Get("name").String(),
		Metadata: parseMetadata(r.Get("metadata")),
	}
	if er := r.Get("entities"); er.IsArray() {
		for _, e := range er.Array() {
			it.Entities = append(it.Entities, e.String())
		}
	}
	return it
}

// searchQuery returns the best hits for `query` among the records `filter`
// admits, placed at the author-chosen `into` path (default `_search.hits`) so
// several queries in one pipeline don't collide. `query` is plain language: no
// engine syntax is recognised. A hit is {id, rank, text, metadata}; there is no
// score, because best-first is the contract and an engine score is not.
func searchQuery(ctx context.Context, ss search.Store, in []byte) (event.Payload, error) {
	tenant, meta, name, fail := searchScope(ctx, ss)
	if fail != nil {
		return *fail, nil
	}
	qr := gjson.GetBytes(meta, "query")
	if !qr.Exists() {
		return searchErr("txco_search_invalid_arg", "missing `query` (the words to search for)"), nil
	}
	filter, err := searchFilter(meta)
	if err != nil {
		return searchErrFrom(err), nil
	}
	hits, err := ss.Query(ctx, tenant, name, qr.String(), int(gjson.GetBytes(meta, "limit").Int()), filter)
	if err != nil {
		return searchErrFrom(err), nil
	}
	if hits == nil {
		hits = []search.Hit{}
	}
	hj, _ := json.Marshal(hits)
	resp, _ := sjson.SetRaw(`{}`, intoPath(meta, "_search.hits"), string(hj))
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// searchDelete removes records by id (`ids` array, or a single `id`) or by
// `filter`, which must carry at least one condition. The filter form removes a
// whole document without knowing how many chunks it has. Reports the number
// removed at `into` (default `_search.deleted`).
func searchDelete(ctx context.Context, ss search.Store, in []byte) (event.Payload, error) {
	tenant, meta, name, fail := searchScope(ctx, ss)
	if fail != nil {
		return *fail, nil
	}
	var sel search.Selector
	if r := gjson.GetBytes(meta, "ids"); r.IsArray() {
		for _, v := range r.Array() {
			sel.IDs = append(sel.IDs, v.String())
		}
	} else if r := gjson.GetBytes(meta, "id"); r.Exists() {
		sel.IDs = []string{r.String()}
	}
	filter, err := searchFilter(meta)
	if err != nil {
		return searchErrFrom(err), nil
	}
	sel.Filter = filter
	n, err := ss.Delete(ctx, tenant, name, sel)
	if err != nil {
		return searchErrFrom(err), nil
	}
	return searchCount(intoPath(meta, "_search.deleted"), n), nil
}

// searchUpdate changes every record `filter` admits without touching its text:
// `merge` merges metadata keys (a null removes the key), and `fields` replaces
// the short searched fields `name`, `title` and `heading` — for a file that was
// renamed, not rewritten. (Not `set`: that is a txcl clause keyword, and cannot
// be a WITH key.) `filter` must carry at least one condition. Reports
// the number of records matched at `into` (default `_search.updated`).
func searchUpdate(ctx context.Context, ss search.Store, in []byte) (event.Payload, error) {
	tenant, meta, name, fail := searchScope(ctx, ss)
	if fail != nil {
		return *fail, nil
	}
	filter, err := searchFilter(meta)
	if err != nil {
		return searchErrFrom(err), nil
	}
	var ch search.Change
	if mr := gjson.GetBytes(meta, "merge"); mr.Exists() {
		if !mr.IsObject() {
			return searchErr("txco_search_invalid_arg", "`merge` must be an object"), nil
		}
		if err := json.Unmarshal([]byte(mr.Raw), &ch.Merge); err != nil {
			return searchErr("txco_search_invalid_arg", "`merge` must be an object"), nil
		}
	}
	if sr := gjson.GetBytes(meta, "fields"); sr.Exists() {
		if !sr.IsObject() {
			return searchErr("txco_search_invalid_arg", "`fields` must be an object"), nil
		}
		var bad string
		fs := &search.FieldSet{}
		sr.ForEach(func(k, v gjson.Result) bool {
			val := v.String()
			switch k.String() {
			case "name":
				fs.Name = &val
			case "title":
				fs.Title = &val
			case "heading":
				fs.Heading = &val
			default:
				bad = k.String()
			}
			return bad == ""
		})
		if bad != "" {
			return searchErr("txco_search_invalid_arg", "`fields` takes `name`, `title` and `heading`, not `"+bad+"` (new text is a new upsert)"), nil
		}
		ch.Fields = fs
	}
	n, err := ss.Update(ctx, tenant, name, filter, ch)
	if err != nil {
		return searchErrFrom(err), nil
	}
	return searchCount(intoPath(meta, "_search.updated"), n), nil
}

// searchCount renders a delete or update count. A backend that has accepted
// the write but not applied it yet reports {"pending": true} in place of a
// number; the bundled backend never does.
func searchCount(into string, n int) event.Payload {
	var resp string
	if n == search.CountPending {
		resp, _ = sjson.Set(`{}`, into+".pending", true)
	} else {
		resp, _ = sjson.Set(`{}`, into, n)
	}
	return event.Payload{Raw: resp, Type: event.JSON}
}
