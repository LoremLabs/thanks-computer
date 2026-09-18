package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/jsonx"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// kv.go holds the handler bodies for the op-writable KV ops (txco://kv/get,
// kv/set, kv/delete, kv/incr, kv/cas) and their batch forms (kv/mget,
// kv/mset, kv/mdelete, kv/list). They are the only ops that persist data
// across requests (the envelope is per-request). Storage is the configured
// KV backend (boltdb or redis); this layer adds the txcl surface: WITH params
// in, JSON value into/out of the envelope tree.
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx)
// (the request-pinned tenant, NOT the mutable _txc.tenant), and the
// namespace defaults to the routed stack so one stack's keys don't collide
// with another's. Values land under a private `_kv` key by default (dropped
// from the web projection), mirroring read-file's `into` convention.

// appStackNamespace maps a routed stack name to its default KV namespace.
// A stack is one app that owns its hostname across inlets: web runs as
// `<stack>`, mail as `<stack>/_mail`, a WebSocket session as
// `<stack>/_websocket`. Those `_`-nested inlet sub-stacks share the app's
// KV by default — the name up to the first `_`-prefixed segment — rather
// than each getting a namespace of its own (which a `/` could not even
// spell: kv segments forbid it, so before this every kv op from a nested
// sub-stack failed with "invalid namespace"). An explicit WITH `namespace`
// still wins; a nested name with no `_` segment is left alone.
func appStackNamespace(stack string) string {
	for i := 0; i < len(stack); i++ {
		if stack[i] == '/' && i+1 < len(stack) && stack[i+1] == '_' {
			return stack[:i]
		}
	}
	return stack
}

// kvScope resolves the trusted tenant + the namespace (WITH `namespace`,
// else the routed stack's app namespace — see appStackNamespace — else
// "default").
func kvScope(ctx context.Context, in []byte) (tenant, ns string, err error) {
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", "", errors.New("kv: no tenant in request scope")
	}
	meta := []byte(operation.MetaFromContext(ctx))
	ns = gjson.GetBytes(meta, "namespace").String()
	if ns == "" {
		// routed stack: _txc.stack on the mail/LMTP path, _txc.route.stack on
		// the HTTP path (same dual read as readfile.go — reading only
		// _txc.route.stack made mail-routed KV ops fall through to "default").
		ns = gjson.GetBytes(in, "_txc.stack").String()
	}
	if ns == "" {
		ns = gjson.GetBytes(in, "_txc.route.stack").String()
	}
	if ns == "" {
		ns = "default"
	}
	if ns, err = kvNamespace(ns); err != nil {
		return "", "", err
	}
	return tenant, ns, nil
}

// kvNamespace maps a namespace as written (or the routed stack's name) to the
// one keys are stored under — see appStackNamespace — and refuses the
// chassis-reserved ones. kvScope applies it to the call's namespace; kv/mget
// applies it to each item's own.
func kvNamespace(ns string) (string, error) {
	ns = appStackNamespace(ns)
	// Chassis-owned namespaces (the txco://blob name index) are not plain KV:
	// refused here so an author can neither read an index as data nor write
	// into one. Same reservation idiom as `_txc.*` on the envelope.
	if kvstore.IsReservedNamespace(ns) {
		return "", fmt.Errorf("kv: namespace %q is reserved for the chassis", ns)
	}
	return ns, nil
}

func kvKey(ctx context.Context, op string) (string, error) {
	key := gjson.GetBytes([]byte(operation.MetaFromContext(ctx)), "key").String()
	if key == "" {
		return "", fmt.Errorf("%s: missing `key`", op)
	}
	return key, nil
}

// kvGet reads one key's JSON value into the envelope at `into` (default
// `_kv`). A miss writes the optional `fallback`, or nothing. (Named
// `fallback`, not `default`: `default` is a reserved txcl keyword, so it
// can't be a WITH param name.)
func kvGet(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	key, err := kvKey(ctx, "kv/get")
	if err != nil {
		return kvErr(err.Error()), err
	}
	tenant, ns, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}
	val, found, gerr := k.Get(ctx, tenant, ns, key)
	if gerr != nil {
		return kvErr(gerr.Error()), gerr
	}

	meta := []byte(operation.MetaFromContext(ctx))
	into := intoPath(meta, "_kv")
	resp := `{}`
	switch {
	case found:
		resp, _ = sjson.SetRaw(resp, into, string(val))
	default:
		if fb := gjson.GetBytes(meta, "fallback"); fb.Exists() {
			resp, _ = sjson.SetRaw(resp, into, fb.Raw)
		}
	}
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// kvSet writes a value (from envelope path `from`, or literal `value`) at
// `key`, with an optional `ttl` (seconds; omit for a persistent key).
func kvSet(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	key, err := kvKey(ctx, "kv/set")
	if err != nil {
		return kvErr(err.Error()), err
	}
	tenant, ns, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}

	meta := []byte(operation.MetaFromContext(ctx))
	raw, verr := kvNewValue(meta, in)
	if verr != nil {
		e := "kv/set: " + verr.Error()
		return kvErr(e), errors.New(e)
	}

	ttl := kvstore.ParseTTLSeconds(gjson.GetBytes(meta, "ttl").Int())
	if serr := k.Set(ctx, tenant, ns, key, json.RawMessage(raw), ttl); serr != nil {
		return kvErr(serr.Error()), serr
	}
	return event.Payload{Raw: `{}`, Type: event.JSON}, nil
}

// kvNewValue resolves the value to write from WITH `from` (an envelope path)
// or `value` (a literal). Exactly one is required. Shared by kv/set + kv/cas.
func kvNewValue(meta, in []byte) (string, error) {
	switch {
	case gjson.GetBytes(meta, "from").Exists():
		from := normReadFilePath(gjson.GetBytes(meta, "from").String())
		src := gjson.GetBytes(in, from)
		if !src.Exists() {
			return "", fmt.Errorf("source path %q is absent", from)
		}
		return src.Raw, nil
	case gjson.GetBytes(meta, "value").Exists():
		return gjson.GetBytes(meta, "value").Raw, nil
	default:
		return "", errors.New("need `from` or `value`")
	}
}

// kvCAS is check-and-set: write the new value (`value` literal or `from` path)
// at `key` only if the current value equals `expected` — or, when `expected`
// is omitted, only if the key is absent (set-if-missing / lock primitive).
// Writes {swapped, current} at `into` (default `_kv`): `swapped` reports
// whether it wrote; `current` is the value now in the store (the new value on
// success, the existing value when the check failed) — for retry-on-conflict.
func kvCAS(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	key, err := kvKey(ctx, "kv/cas")
	if err != nil {
		return kvErr(err.Error()), err
	}
	tenant, ns, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}

	meta := []byte(operation.MetaFromContext(ctx))
	raw, verr := kvNewValue(meta, in)
	if verr != nil {
		e := "kv/cas: " + verr.Error()
		return kvErr(e), errors.New(e)
	}

	exp := gjson.GetBytes(meta, "expected")
	expectAbsent := !exp.Exists()
	var expected json.RawMessage
	if !expectAbsent {
		expected = json.RawMessage(exp.Raw)
	}

	ttl := kvstore.ParseTTLSeconds(gjson.GetBytes(meta, "ttl").Int())
	swapped, current, cerr := k.CAS(ctx, tenant, ns, key, expectAbsent, expected, json.RawMessage(raw), ttl)
	if cerr != nil {
		return kvErr(cerr.Error()), cerr
	}

	into := intoPath(meta, "_kv")
	resp := `{}`
	resp, _ = sjson.Set(resp, into+".swapped", swapped)
	if len(current) > 0 {
		resp, _ = sjson.SetRaw(resp, into+".current", string(current))
	}
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// kvDelete removes a key (a missing key is a success).
func kvDelete(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	key, err := kvKey(ctx, "kv/delete")
	if err != nil {
		return kvErr(err.Error()), err
	}
	tenant, ns, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}
	if derr := k.Delete(ctx, tenant, ns, key); derr != nil {
		return kvErr(derr.Error()), derr
	}
	return event.Payload{Raw: `{}`, Type: event.JSON}, nil
}

// kvIncr atomically adds `by` (default 1) to an integer key and writes the
// new value into the envelope at `into` (default `_kv`), with optional `ttl`.
func kvIncr(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	key, err := kvKey(ctx, "kv/incr")
	if err != nil {
		return kvErr(err.Error()), err
	}
	tenant, ns, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}

	meta := []byte(operation.MetaFromContext(ctx))
	by := int64(1)
	if b := gjson.GetBytes(meta, "by"); b.Exists() {
		by = b.Int()
	}
	ttl := kvstore.ParseTTLSeconds(gjson.GetBytes(meta, "ttl").Int())
	n, ierr := k.Incr(ctx, tenant, ns, key, by, ttl)
	if ierr != nil {
		return kvErr(ierr.Error()), ierr
	}
	into := intoPath(meta, "_kv")
	resp, _ := sjson.Set(`{}`, into, n)
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// kvList writes the user keys under (tenant, namespace) into the envelope at
// `into` (default `_kv`) as {keys:[…], next:"…", count:n}. Read-only and
// WINDOWED: at most `limit` keys (default/max kvstore.MaxListLimit) that sort
// after the `after` cursor; `next` is the cursor to pass on the following call
// ("" when the namespace is exhausted). There's no per-key prefix scan — the KV
// NAMESPACE is the prefix, so bucket a listable set (e.g. subscribers) in its
// own namespace. No `key` param. With `values = true` it also writes
// rows:[{key, value}] — values the store read for the listing anyway — metered
// per MiB returned.
func kvList(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	tenant, ns, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}
	meta := []byte(operation.MetaFromContext(ctx))
	after := gjson.GetBytes(meta, "after").String()
	limit := int(gjson.GetBytes(meta, "limit").Int())
	values := gjson.GetBytes(meta, "values").Bool()
	pairs, next, lerr := k.ListPairsPage(ctx, tenant, ns, after, limit)
	if lerr != nil {
		return kvErr(lerr.Error()), lerr
	}
	keys := make([]string, 0, len(pairs)) // emit [] not null for an empty/exhausted page
	var rows strings.Builder
	var valueBytes int64
	rows.WriteByte('[')
	for i, p := range pairs {
		keys = append(keys, p.Key)
		if !values {
			continue
		}
		if i > 0 {
			rows.WriteByte(',')
		}
		row := jsonx.NewObject()
		row.Set("key", p.Key)
		row.SetRaw("value", string(p.Value))
		rows.WriteString(row.String())
		valueBytes += int64(len(p.Value))
	}
	rows.WriteByte(']')
	into := intoPath(meta, "_kv")
	blob, _ := json.Marshal(keys)
	resp := jsonx.NewObject()
	resp.SetRaw(into+".keys", string(blob))
	resp.Set(into+".next", next)
	resp.Set(into+".count", len(keys))
	if values {
		resp.SetRaw(into+".rows", rows.String())
		chargePerMiB(ctx, valueBytes, processor.FuelCostKVPerMiB, in)
	}
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// kvMGet reads many keys in one dispatch. `items` is an array whose entries are
// a bare key string (in the call's namespace, as kvScope resolves it) or an
// object {key, namespace?}, where an item's own namespace overrides the call's.
// It writes {items:[{namespace, key, found, value?}], count, found} at `into`
// (default `_kv`), in item order. Every item is validated before anything is
// read, and more than kvstore.MaxBatchItems items is refused rather than
// clamped: a dropped or skipped item would read exactly like a missing key, and
// a sweep that deletes what it cannot find would act on it. Values are metered
// per MiB returned.
func kvMGet(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	tenant, defaultNS, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}
	meta := []byte(operation.MetaFromContext(ctx))
	refs, rerr := kvItemRefs(gjson.GetBytes(meta, "items"), defaultNS)
	if rerr != nil {
		e := "kv/mget: " + rerr.Error()
		return kvErr(e), errors.New(e)
	}
	hits, gerr := k.GetMany(ctx, tenant, refs)
	if gerr != nil {
		return kvErr(gerr.Error()), gerr
	}

	var items strings.Builder
	var found int
	var valueBytes int64
	items.WriteByte('[')
	for i, h := range hits {
		if i > 0 {
			items.WriteByte(',')
		}
		row := jsonx.NewObject()
		row.Set("namespace", refs[i].Namespace)
		row.Set("key", refs[i].Key)
		row.Set("found", h.Found)
		if h.Found {
			row.SetRaw("value", string(h.Value))
			found++
			valueBytes += int64(len(h.Value))
		}
		items.WriteString(row.String())
	}
	items.WriteByte(']')

	into := intoPath(meta, "_kv")
	resp := jsonx.NewObject()
	resp.SetRaw(into+".items", items.String())
	resp.Set(into+".count", len(hits))
	resp.Set(into+".found", found)
	chargePerMiB(ctx, valueBytes, processor.FuelCostKVPerMiB, in)
	return event.Payload{Raw: resp.String(), Type: event.JSON}, nil
}

// kvItemRefs parses the `items` of kv/mget and kv/mdelete into store refs. An
// item's empty or absent namespace takes the call's, as an empty WITH
// `namespace` does for kv/get.
func kvItemRefs(items gjson.Result, defaultNS string) ([]kvstore.Ref, error) {
	if !items.IsArray() {
		return nil, errors.New("need `items`, an array of keys or {key, namespace} objects")
	}
	arr := items.Array()
	if len(arr) > kvstore.MaxBatchItems {
		return nil, fmt.Errorf("%d items exceeds the %d-item cap; split the batch", len(arr), kvstore.MaxBatchItems)
	}
	refs := make([]kvstore.Ref, len(arr))
	for i, it := range arr {
		ref := kvstore.Ref{Namespace: defaultNS}
		switch {
		case it.Type == gjson.String:
			ref.Key = it.String()
		case it.IsObject():
			ref.Key = it.Get("key").String()
			if ns := it.Get("namespace").String(); ns != "" {
				n, err := kvNamespace(ns)
				if err != nil {
					return nil, fmt.Errorf("item %d: %w", i, err)
				}
				ref.Namespace = n
			}
		default:
			return nil, fmt.Errorf("item %d: want a key string or a {key, namespace} object", i)
		}
		if ref.Key == "" {
			return nil, fmt.Errorf("item %d: missing `key`", i)
		}
		refs[i] = ref
	}
	return refs, nil
}

// kvMSet writes many keys atomically — all or none — from `items`, an array of
// {key, namespace?, value | from, ttl?} objects. An item's namespace and ttl
// default to the call's WITH `namespace` and `ttl`; `value` is a literal and
// `from` an envelope path, exactly as for kv/set. Every item is validated
// before anything is written, a key may appear once, and more than
// kvstore.MaxBatchItems items is refused. Like kv/set it writes nothing to the
// envelope; the bytes written are metered per MiB.
func kvMSet(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	tenant, defaultNS, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}
	meta := []byte(operation.MetaFromContext(ctx))
	writes, werr := kvMSetWrites(meta, in, defaultNS)
	if werr != nil {
		e := "kv/mset: " + werr.Error()
		return kvErr(e), errors.New(e)
	}
	if serr := k.SetMany(ctx, tenant, writes); serr != nil {
		return kvErr(serr.Error()), serr
	}
	var n int64
	for _, w := range writes {
		n += int64(len(w.Value))
	}
	chargePerMiB(ctx, n, processor.FuelCostKVPerMiB, in)
	return event.Payload{Raw: `{}`, Type: event.JSON}, nil
}

// kvMSetWrites parses kv/mset's `items`. The cap is checked first, so an
// oversized batch is refused without resolving a single value.
func kvMSetWrites(meta, in []byte, defaultNS string) ([]kvstore.Write, error) {
	items := gjson.GetBytes(meta, "items")
	if !items.IsArray() {
		return nil, errors.New("need `items`, an array of {key, value} objects")
	}
	arr := items.Array()
	if len(arr) > kvstore.MaxBatchItems {
		return nil, fmt.Errorf("%d items exceeds the %d-item cap; split the batch", len(arr), kvstore.MaxBatchItems)
	}
	defaultTTL := gjson.GetBytes(meta, "ttl").Int()
	writes := make([]kvstore.Write, len(arr))
	for i, it := range arr {
		if !it.IsObject() {
			return nil, fmt.Errorf("item %d: want a {key, value} object", i)
		}
		w := kvstore.Write{Namespace: defaultNS, Key: it.Get("key").String()}
		if w.Key == "" {
			return nil, fmt.Errorf("item %d: missing `key`", i)
		}
		if ns := it.Get("namespace").String(); ns != "" {
			n, err := kvNamespace(ns)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			w.Namespace = n
		}
		raw, err := kvNewValue([]byte(it.Raw), in)
		if err != nil {
			return nil, fmt.Errorf("item %d: %w", i, err)
		}
		w.Value = json.RawMessage(raw)
		ttl := defaultTTL
		if t := it.Get("ttl"); t.Exists() {
			ttl = t.Int()
		}
		w.TTL = kvstore.ParseTTLSeconds(ttl)
		writes[i] = w
	}
	return writes, nil
}

// kvMDelete removes many keys atomically — all or none. `items` is parsed as
// for kv/mget: bare keys in the call's namespace, or {key, namespace?}
// objects. Missing keys are not an error; every item is validated before
// anything is deleted, and more than kvstore.MaxBatchItems is refused.
func kvMDelete(ctx context.Context, k *kvstore.KV, in []byte) (event.Payload, error) {
	tenant, defaultNS, err := kvScope(ctx, in)
	if err != nil {
		return kvErr(err.Error()), err
	}
	meta := []byte(operation.MetaFromContext(ctx))
	refs, rerr := kvItemRefs(gjson.GetBytes(meta, "items"), defaultNS)
	if rerr != nil {
		e := "kv/mdelete: " + rerr.Error()
		return kvErr(e), errors.New(e)
	}
	if derr := k.DeleteMany(ctx, tenant, refs); derr != nil {
		return kvErr(derr.Error()), derr
	}
	return event.Payload{Raw: `{}`, Type: event.JSON}, nil
}

// kvErr builds a structured error event.Payload (never includes values —
// only the human-readable reason).
func kvErr(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "kv-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}
