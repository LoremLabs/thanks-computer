package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// kv.go holds the handler bodies for the op-writable KV ops (txco://kv/get,
// kv/set, kv/delete, kv/incr). They are the only ops that persist data
// across requests (the envelope is per-request). Storage is the configured
// KV backend (boltdb or redis); this layer adds the txcl surface: WITH params
// in, JSON value into/out of the envelope tree.
//
// Scoping is trusted: the tenant comes from processor.TenantScope(ctx)
// (the request-pinned tenant, NOT the mutable _txc.tenant), and the
// namespace defaults to the routed stack so one stack's keys don't collide
// with another's. Values land under a private `_kv` key by default (dropped
// from the web projection), mirroring read-file's `into` convention.

// kvScope resolves the trusted tenant + the namespace (WITH `namespace`,
// else the routed stack, else "default").
func kvScope(ctx context.Context, in []byte) (tenant, ns string, err error) {
	tenant = processor.TenantScope(ctx)
	if tenant == "" {
		return "", "", errors.New("kv: no tenant in request scope")
	}
	meta := []byte(operation.MetaFromContext(ctx))
	ns = gjson.GetBytes(meta, "namespace").String()
	if ns == "" {
		ns = gjson.GetBytes(in, "_txc.route.stack").String()
	}
	if ns == "" {
		ns = "default"
	}
	return tenant, ns, nil
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
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_kv"
	}
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
	var raw string
	switch {
	case gjson.GetBytes(meta, "from").Exists():
		from := normReadFilePath(gjson.GetBytes(meta, "from").String())
		src := gjson.GetBytes(in, from)
		if !src.Exists() {
			e := fmt.Sprintf("kv/set: source path %q is absent", from)
			return kvErr(e), errors.New(e)
		}
		raw = src.Raw
	case gjson.GetBytes(meta, "value").Exists():
		raw = gjson.GetBytes(meta, "value").Raw
	default:
		return kvErr("kv/set: need `from` or `value`"), errors.New("kv/set: no value")
	}

	ttl := kvstore.ParseTTLSeconds(gjson.GetBytes(meta, "ttl").Int())
	if serr := k.Set(ctx, tenant, ns, key, json.RawMessage(raw), ttl); serr != nil {
		return kvErr(serr.Error()), serr
	}
	return event.Payload{Raw: `{}`, Type: event.JSON}, nil
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
	into := normReadFilePath(gjson.GetBytes(meta, "into").String())
	if into == "" {
		into = "_kv"
	}
	resp, _ := sjson.Set(`{}`, into, n)
	return event.Payload{Raw: resp, Type: event.JSON}, nil
}

// kvErr builds a structured error event.Payload (never includes values —
// only the human-readable reason).
func kvErr(msg string) event.Payload {
	em, _ := sjson.Set(`{}`, "error.0", "kv-err")
	em, _ = sjson.Set(em, "errorMsg", msg)
	return event.Payload{Raw: `{}`, Type: event.Null, Meta: em}
}
