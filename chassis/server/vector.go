package server

import (
	"context"
	"encoding/json"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// vector.go holds the handler bodies for the durable vector store ops
// (txco://vector/{collection,upsert,search,delete}). Storage is the configured
// vector backend (sqlite-vec bundled; pgvector behind the same interface for
// HA). This layer adds the txcl surface: WITH params in, JSON in/out of the
// envelope.
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx) (the
// request-pinned tenant, NOT the mutable _txc.tenant). Unlike txco://kv —
// whose keys are per-stack — vector COLLECTIONS are tenant-level on purpose:
// one stack imports/upserts (the catalog builder) while another searches (the
// start@ flow), so they must share the collection.
//
// Errors surface as a top-level `vector.error` on the envelope (code +
// message, never values) so authors handle them uniformly with
// `WHEN @vector.error EXEC ...`, mirroring ai://chat / ai://embed.

func vecTenant(ctx context.Context) (string, bool) {
	return processor.TenantScope(ctx), processor.TenantScope(ctx) != ""
}

func vecErr(code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, "vector.error.code", code)
	raw, _ = sjson.Set(raw, "vector.error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

func vecErrFrom(err error) event.Payload {
	code := "txco_vector_unknown"
	if c, ok := err.(vector.CodedError); ok {
		code = c.Code()
	}
	return vecErr(code, err.Error())
}

// vectorCollection ensures a collection exists, pinning embedding model,
// dimensions, and metric. Idempotent; a conflicting pin errors.
func vectorCollection(ctx context.Context, vs vector.Store, in []byte) (event.Payload, error) {
	tenant, ok := vecTenant(ctx)
	if !ok {
		return vecErr("txco_vector_no_tenant", "no tenant in request scope"), nil
	}
	meta := []byte(operation.MetaFromContext(ctx))
	name := gjson.GetBytes(meta, "collection").String()
	if name == "" {
		return vecErr("txco_vector_invalid_arg", "missing `collection`"), nil
	}
	c := vector.Collection{
		Name:           name,
		EmbeddingModel: gjson.GetBytes(meta, "embedding_model").String(),
		Dimensions:     int(gjson.GetBytes(meta, "dimensions").Int()),
		Metric:         vector.Metric(gjson.GetBytes(meta, "metric").String()),
	}
	if err := vs.EnsureCollection(ctx, tenant, c); err != nil {
		return vecErrFrom(err), nil
	}
	got, _, err := vs.DescribeCollection(ctx, tenant, name)
	if err != nil {
		return vecErrFrom(err), nil
	}
	out, _ := json.Marshal(got)
	resp, _ := sjson.SetRaw(`{}`, "_vector.collection", string(out))
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// vectorUpsert writes one or more items. Accepts a batch `items` array, or a
// single item from `id`/`vector`/`metadata`/`text`.
func vectorUpsert(ctx context.Context, vs vector.Store, in []byte) (event.Payload, error) {
	tenant, ok := vecTenant(ctx)
	if !ok {
		return vecErr("txco_vector_no_tenant", "no tenant in request scope"), nil
	}
	meta := []byte(operation.MetaFromContext(ctx))
	name := gjson.GetBytes(meta, "collection").String()
	if name == "" {
		return vecErr("txco_vector_invalid_arg", "missing `collection`"), nil
	}
	items, err := parseItems(meta)
	if err != nil {
		return vecErrFrom(err), nil
	}
	if len(items) == 0 {
		return vecErr("txco_vector_invalid_arg", "no items to upsert (supply `items` or `id`+`vector`)"), nil
	}
	n, uerr := vs.Upsert(ctx, tenant, name, items)
	if uerr != nil {
		return vecErrFrom(uerr), nil
	}
	resp, _ := sjson.Set(`{}`, "_vector.upserted", n)
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// vectorSearch returns the nearest matches, restricted by `filter`, placed at
// the author-chosen `into` path (default `_vector.matches`) so multiple search
// ops in one pipeline don't collide.
func vectorSearch(ctx context.Context, vs vector.Store, in []byte) (event.Payload, error) {
	tenant, ok := vecTenant(ctx)
	if !ok {
		return vecErr("txco_vector_no_tenant", "no tenant in request scope"), nil
	}
	meta := []byte(operation.MetaFromContext(ctx))
	name := gjson.GetBytes(meta, "collection").String()
	if name == "" {
		return vecErr("txco_vector_invalid_arg", "missing `collection`"), nil
	}
	query, qok := parseFloat32(gjson.GetBytes(meta, "vector"))
	if !qok || len(query) == 0 {
		return vecErr("txco_vector_invalid_arg", "missing `vector` (the query embedding array)"), nil
	}
	limit := int(gjson.GetBytes(meta, "limit").Int())
	filter, ferr := parseFilter(meta)
	if ferr != nil {
		return vecErrFrom(ferr), nil
	}

	matches, serr := vs.Search(ctx, tenant, name, query, limit, filter)
	if serr != nil {
		return vecErrFrom(serr), nil
	}

	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_vector.matches"
	}
	if matches == nil {
		matches = []vector.Match{}
	}
	mj, _ := json.Marshal(matches)
	resp, _ := sjson.SetRaw(`{}`, into, string(mj))
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// vectorDelete removes items by id (`ids` array, or a single `id`).
func vectorDelete(ctx context.Context, vs vector.Store, in []byte) (event.Payload, error) {
	tenant, ok := vecTenant(ctx)
	if !ok {
		return vecErr("txco_vector_no_tenant", "no tenant in request scope"), nil
	}
	meta := []byte(operation.MetaFromContext(ctx))
	name := gjson.GetBytes(meta, "collection").String()
	if name == "" {
		return vecErr("txco_vector_invalid_arg", "missing `collection`"), nil
	}
	var ids []string
	if r := gjson.GetBytes(meta, "ids"); r.IsArray() {
		for _, v := range r.Array() {
			ids = append(ids, v.String())
		}
	} else if r := gjson.GetBytes(meta, "id"); r.Exists() {
		ids = []string{r.String()}
	}
	if len(ids) == 0 {
		return vecErr("txco_vector_invalid_arg", "no ids to delete (supply `ids` or `id`)"), nil
	}
	n, derr := vs.Delete(ctx, tenant, name, ids)
	if derr != nil {
		return vecErrFrom(derr), nil
	}
	resp, _ := sjson.Set(`{}`, "_vector.deleted", n)
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// parseItems reads the upsert items: the batch `items` array, else a single
// item from top-level `id`/`vector`/`metadata`/`text`.
func parseItems(meta []byte) ([]vector.Item, error) {
	if arr := gjson.GetBytes(meta, "items"); arr.Exists() {
		if !arr.IsArray() {
			return nil, &vector.InvalidArgError{Reason: "`items` must be an array"}
		}
		var items []vector.Item
		for _, it := range arr.Array() {
			vec, ok := parseFloat32(it.Get("vector"))
			if !ok {
				return nil, &vector.InvalidArgError{Reason: "each item needs a `vector` array"}
			}
			items = append(items, vector.Item{
				ID:       it.Get("id").String(),
				Vector:   vec,
				Metadata: parseMetadata(it.Get("metadata")),
				Text:     it.Get("text").String(),
			})
		}
		return items, nil
	}
	if vr := gjson.GetBytes(meta, "vector"); vr.Exists() {
		vec, ok := parseFloat32(vr)
		if !ok {
			return nil, &vector.InvalidArgError{Reason: "`vector` must be an array"}
		}
		return []vector.Item{{
			ID:       gjson.GetBytes(meta, "id").String(),
			Vector:   vec,
			Metadata: parseMetadata(gjson.GetBytes(meta, "metadata")),
			Text:     gjson.GetBytes(meta, "text").String(),
		}}, nil
	}
	return nil, nil
}

func parseFloat32(r gjson.Result) ([]float32, bool) {
	if !r.IsArray() {
		return nil, false
	}
	arr := r.Array()
	out := make([]float32, len(arr))
	for i, v := range arr {
		out[i] = float32(v.Float())
	}
	return out, true
}

func parseMetadata(r gjson.Result) map[string]any {
	if !r.Exists() || !r.IsObject() {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Raw), &m); err != nil {
		return nil
	}
	return m
}

// parseFilter turns the WITH `filter` object into a vector.Filter:
//
//	{ "genre": "adventure",          // scalar      → eq
//	  "tags":  ["a","b"],            // array       → in
//	  "age":   {"gte": 9, "lte": 18}, // op-object  → range (AND)
//	  "id":    {"not_in": [...]} }    // op-object  → not_in
func parseFilter(meta []byte) (vector.Filter, error) {
	fr := gjson.GetBytes(meta, "filter")
	if !fr.Exists() {
		return vector.Filter{}, nil
	}
	if !fr.IsObject() {
		return vector.Filter{}, &vector.InvalidArgError{Reason: "`filter` must be an object"}
	}
	var f vector.Filter
	var perr error
	fr.ForEach(func(key, val gjson.Result) bool {
		field := key.String()
		switch {
		case val.IsObject():
			val.ForEach(func(opKey, opVal gjson.Result) bool {
				op, ok := mapFilterOp(opKey.String())
				if !ok {
					perr = &vector.InvalidArgError{Reason: "unknown filter op " + opKey.String()}
					return false
				}
				f.Conditions = append(f.Conditions, vector.Condition{Field: field, Op: op, Value: opVal.Value()})
				return true
			})
		case val.IsArray():
			f.Conditions = append(f.Conditions, vector.Condition{Field: field, Op: vector.OpIn, Value: val.Value()})
		default:
			f.Conditions = append(f.Conditions, vector.Condition{Field: field, Op: vector.OpEq, Value: val.Value()})
		}
		return perr == nil
	})
	return f, perr
}

func mapFilterOp(s string) (vector.Op, bool) {
	switch s {
	case "eq":
		return vector.OpEq, true
	case "in":
		return vector.OpIn, true
	case "not_in":
		return vector.OpNotIn, true
	case "gte":
		return vector.OpGte, true
	case "lte":
		return vector.OpLte, true
	case "gt":
		return vector.OpGt, true
	case "lt":
		return vector.OpLt, true
	}
	return "", false
}
