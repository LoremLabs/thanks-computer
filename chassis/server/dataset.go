package server

// dataset.go holds the handler body for `txco://dataset`: named-query
// lookups against the routed stack's DATASETS/ artifacts (chassis/dataset).
// Only queries the dataset's manifest declares can run — validated at
// activation, executed here read-only with parameters always bound.
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx) (the
// request-pinned tenant, NOT the mutable _txc.tenant); an explicit `stack`
// re-targets only WITHIN the caller's tenant (read-file precedent).
//
// Errors surface as a top-level `dataset.error` on the envelope (code +
// message, never data) so authors handle them uniformly with
// `WHEN @dataset.error EXEC ...`, mirroring txco://vector / ai://chat.
//
// WITH parameters (op.Meta):
//
//	dataset = "books"          (required: DATASETS/<name>)
//	query   = "lookup_title"   (required: a name from DATASETS/<name>.yaml)
//	args    = [.q]             (optional: positional binds, in SQL ? order)
//	limit   = 5                (optional: tightens the row cap; never widens)
//	stack   = "other-stack"    (optional: another of YOUR tenant's stacks)
//	into    = "_dataset"       (optional: destination subtree)
//
// Output (default `into` "_dataset" — a `_`-prefixed key the default web
// projection drops, so rows never leak to a caller unless redirected):
//
//	{"_dataset":{"dataset":"books","query":"lookup_title",
//	  "rows":[{"title":"…","author":"…"}], "count":1, "truncated":false}}

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/dataset"
	"github.com/loremlabs/thanks-computer/chassis/dbcache"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// datasetMaxResponseBytes bounds the marshalled rows payload so one fat row
// set can't balloon the envelope; hitting it marks the result truncated.
// Row-count limits are the primary cap — this is the byte backstop.
const datasetMaxResponseBytes = 1 << 20 // 1 MiB

// manifestCache memoises parsed manifests by their content hash — manifests
// are immutable per hash, so this never invalidates, and a stack re-apply
// simply introduces a new hash. Bounded by the number of distinct manifest
// versions a node serves (small).
var manifestCache sync.Map // hash string → *dataset.Manifest

func dsErr(code, msg string) event.Payload {
	raw, _ := sjson.Set(`{}`, "dataset.error.code", code)
	raw, _ = sjson.Set(raw, "dataset.error.message", msg)
	return event.Payload{Raw: raw, Type: event.JSON}
}

// datasetQuery executes one declared query and writes the rows at `into`.
func datasetQuery(ctx context.Context, dbc *dbcache.DbCache, dsc *dataset.Cache, fcas filecas.Store, in []byte, maxRows int) (event.Payload, error) {
	tenant := processor.TenantScope(ctx)
	if tenant == "" {
		return dsErr("txco_dataset_no_tenant", "no tenant in request scope"), nil
	}
	meta := []byte(operation.MetaFromContext(ctx))
	name := gjson.GetBytes(meta, "dataset").String()
	if name == "" {
		return dsErr("txco_dataset_invalid_arg", "missing `dataset`"), nil
	}
	qname := gjson.GetBytes(meta, "query").String()
	if qname == "" {
		return dsErr("txco_dataset_invalid_arg", "missing `query`"), nil
	}
	stack := gjson.GetBytes(meta, "stack").String()
	if stack == "" {
		stack = gjson.GetBytes(in, "_txc.stack").String()
		if stack == "" {
			stack = gjson.GetBytes(in, "_txc.route.stack").String()
		}
	}
	if stack == "" {
		return dsErr("txco_dataset_invalid_arg", "no stack in scope (set `stack` or route the request)"), nil
	}

	artifactHash, manifest, errPayload := resolveDataset(ctx, dbc, fcas, tenant, stack, name)
	if errPayload != nil {
		return *errPayload, nil
	}

	q, ok := manifest.Queries[qname]
	if !ok {
		return dsErr("txco_dataset_unknown_query",
			fmt.Sprintf("dataset %q has no query %q (have: %s)", name, qname, strings.Join(manifest.QueryNames(), ", "))), nil
	}

	// Row cap: node config is the ceiling; the manifest's max_rows and the
	// rule's `limit` only ever tighten it.
	cap := maxRows
	if q.MaxRows > 0 && q.MaxRows < cap {
		cap = q.MaxRows
	}
	if l := int(gjson.GetBytes(meta, "limit").Int()); l > 0 && l < cap {
		cap = l
	}

	args, aerr := datasetArgs(meta)
	if aerr != nil {
		return dsErr("txco_dataset_invalid_arg", aerr.Error()), nil
	}

	db, err := dsc.Handle(ctx, artifactHash)
	if err != nil {
		if errors.Is(err, filecas.ErrNotFound) {
			return dsErr("txco_dataset_missing_artifact",
				fmt.Sprintf("artifact for dataset %q (%s) not in the CAS", name, artifactHash)), nil
		}
		return dsErr("txco_dataset_store", fmt.Sprintf("open dataset %q: %v", name, err)), nil
	}

	rows, err := db.QueryContext(ctx, q.SQL, args...)
	if err != nil {
		// Wrong bind-count, ctx timeout, and (belt) authorizer denials all
		// land here with sqlite's message.
		return dsErr("txco_dataset_query", fmt.Sprintf("query %q: %v", qname, err)), nil
	}
	defer rows.Close()

	rowsJSON, count, truncated, rerr := marshalRows(rows, cap)
	if rerr != nil {
		return dsErr("txco_dataset_query", fmt.Sprintf("query %q: %v", qname, rerr)), nil
	}

	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_dataset"
	}
	resp, _ := sjson.Set(`{}`, into+".dataset", name)
	resp, _ = sjson.Set(resp, into+".query", qname)
	resp, _ = sjson.SetRaw(resp, into+".rows", rowsJSON)
	resp, _ = sjson.Set(resp, into+".count", count)
	resp, _ = sjson.Set(resp, into+".truncated", truncated)
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// resolveDataset maps (tenant slug, stack, dataset name) → the ACTIVE
// version's artifact hash + parsed manifest, reading metadata from the
// dbcache snapshot (in-memory mirror — no disk on the hot path) and
// manifest bytes from the row (inline, single-node) or the CAS
// (fingerprint-only fleet rows; small + LRU-cached).
func resolveDataset(ctx context.Context, dbc *dbcache.DbCache, fcas filecas.Store, tenant, stack, name string) (string, *dataset.Manifest, *event.Payload) {
	fail := func(p event.Payload) (string, *dataset.Manifest, *event.Payload) { return "", nil, &p }

	artifactPath := dataset.Dir + "/" + name + dataset.ArtifactExt
	manifestPath := dataset.Dir + "/" + name + dataset.ManifestExt
	rows, err := dbc.Snapshot().QueryContext(ctx, `
		SELECT sf.path, sf.content, sf.content_hash
		  FROM stack_files sf
		  JOIN stacks  s ON s.active_version = sf.version_id
		  JOIN tenants t ON t.tenant_id = s.tenant_id
		 WHERE t.slug = ? AND t.revoked_at IS NULL AND s.name = ?
		   AND sf.path IN (?, ?)`,
		tenant, stack, artifactPath, manifestPath)
	if err != nil {
		return fail(dsErr("txco_dataset_store", fmt.Sprintf("resolve dataset %q: %v", name, err)))
	}
	defer rows.Close()

	var artifactHash, manifestInline, manifestHash string
	found := 0
	for rows.Next() {
		var p, content, hash string
		if err := rows.Scan(&p, &content, &hash); err != nil {
			return fail(dsErr("txco_dataset_store", fmt.Sprintf("resolve dataset %q: %v", name, err)))
		}
		found++
		switch p {
		case artifactPath:
			artifactHash = hash
		case manifestPath:
			manifestInline, manifestHash = content, hash
		}
	}
	if found == 0 {
		return fail(dsErr("txco_dataset_not_found",
			fmt.Sprintf("dataset %q not in stack %q's active version", name, stack)))
	}
	if artifactHash == "" || (manifestInline == "" && manifestHash == "") {
		// Half a pair should be impossible past the activation gate.
		return fail(dsErr("txco_dataset_not_found",
			fmt.Sprintf("dataset %q is incomplete in stack %q (missing %s)", name, stack,
				map[bool]string{true: artifactPath, false: manifestPath}[artifactHash == ""])))
	}

	// Manifest: prefer the inline body; fleet fingerprint rows resolve from
	// the CAS. Cache the parse by content hash (immutable).
	if manifestHash == "" {
		sum := sha256.Sum256([]byte(manifestInline))
		manifestHash = hex.EncodeToString(sum[:])
	}
	if m, ok := manifestCache.Load(manifestHash); ok {
		return artifactHash, m.(*dataset.Manifest), nil
	}
	body := []byte(manifestInline)
	if len(body) == 0 {
		if fcas == nil {
			return fail(dsErr("txco_dataset_store", "no filecas store; cannot resolve manifest"))
		}
		if body, err = fcas.Get(ctx, manifestHash); err != nil {
			return fail(dsErr("txco_dataset_store", fmt.Sprintf("resolve manifest for %q: %v", name, err)))
		}
	}
	m, perr := dataset.ParseManifest(body)
	if perr != nil {
		// Activation validated it; reaching here means bytes drifted.
		return fail(dsErr("txco_dataset_store", fmt.Sprintf("manifest for %q: %v", name, perr)))
	}
	manifestCache.Store(manifestHash, m)
	return artifactHash, m, nil
}

// datasetArgs converts the WITH `args` array to positional bind values.
// Scalars only — a nested object/array almost certainly means a resolve
// mistake in the rule, so fail loudly rather than bind its JSON text.
func datasetArgs(meta []byte) ([]any, error) {
	v := gjson.GetBytes(meta, "args")
	if !v.Exists() {
		return nil, nil
	}
	if !v.IsArray() {
		return nil, errors.New("`args` must be an array of scalar bind values")
	}
	arr := v.Array()
	out := make([]any, 0, len(arr))
	for i, a := range arr {
		switch a.Type {
		case gjson.String:
			out = append(out, a.String())
		case gjson.Number:
			out = append(out, a.Float())
		case gjson.True, gjson.False:
			out = append(out, a.Bool())
		case gjson.Null:
			out = append(out, nil)
		default:
			return nil, fmt.Errorf("args[%d] is not a scalar", i)
		}
	}
	return out, nil
}

// marshalRows renders up to cap rows as a JSON array of {column: value}
// objects, reporting whether more rows (or bytes) remained.
func marshalRows(rows *sql.Rows, cap int) (rowsJSON string, count int, truncated bool, err error) {
	cols, err := rows.Columns()
	if err != nil {
		return "", 0, false, err
	}
	var b strings.Builder
	b.WriteByte('[')
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if count >= cap {
			truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", 0, false, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			if bv, ok := vals[i].([]byte); ok {
				row[c] = string(bv)
				continue
			}
			row[c] = vals[i]
		}
		enc, jerr := json.Marshal(row)
		if jerr != nil {
			return "", 0, false, jerr
		}
		if b.Len()+len(enc)+1 > datasetMaxResponseBytes {
			truncated = true
			break
		}
		if count > 0 {
			b.WriteByte(',')
		}
		b.Write(enc)
		count++
	}
	if err := rows.Err(); err != nil {
		return "", 0, false, err
	}
	b.WriteByte(']')
	return b.String(), count, truncated, nil
}
