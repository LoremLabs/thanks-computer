package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

func newKVHandle(t *testing.T) *kvstore.KV {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{path}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return kvstore.New(s, 65536, 0)
}

type kvHandler func(context.Context, *kvstore.KV, []byte) (event.Payload, error)

// callKV pins the tenant + WITH meta and builds an envelope carrying the
// routed stack, then invokes the handler. inExtra (optional) is merged into
// the envelope so kv/set `from` tests can supply a source value.
func callKV(t *testing.T, fn kvHandler, k *kvstore.KV, tenant, stack, metaJSON, inExtra string) (event.Payload, error) {
	t.Helper()
	ctx := processor.WithTenant(context.Background(), tenant)
	ctx = operation.WithMeta(ctx, metaJSON)
	in := `{}`
	if stack != "" {
		in, _ = sjson.Set(in, "_txc.route.stack", stack)
	}
	if inExtra != "" {
		gjson.Parse(inExtra).ForEach(func(key, val gjson.Result) bool {
			in, _ = sjson.SetRaw(in, key.String(), val.Raw)
			return true
		})
	}
	return fn(ctx, k, []byte(in))
}

func TestKVSetGetRoundTrip(t *testing.T) {
	k := newKVHandle(t)
	if _, err := callKV(t, kvSet, k, "t1", "hello",
		`{"key":"greeting","value":{"msg":"hi"}}`, ""); err != nil {
		t.Fatalf("set: %v", err)
	}
	pay, err := callKV(t, kvGet, k, "t1", "hello", `{"key":"greeting"}`, "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := gjson.Get(pay.Raw, "_kv.msg").String(); got != "hi" {
		t.Fatalf("value not round-tripped into _kv: raw=%s", pay.Raw)
	}
}

func TestKVSetFromPath(t *testing.T) {
	k := newKVHandle(t)
	// store from an envelope path
	if _, err := callKV(t, kvSet, k, "t1", "hello",
		`{"key":"k","from":".payload"}`, `{"payload":{"a":1}}`); err != nil {
		t.Fatalf("set from: %v", err)
	}
	pay, _ := callKV(t, kvGet, k, "t1", "hello", `{"key":"k"}`, "")
	if got := gjson.Get(pay.Raw, "_kv.a").Int(); got != 1 {
		t.Fatalf("from-path value wrong: raw=%s", pay.Raw)
	}
	// absent source path errors
	if _, err := callKV(t, kvSet, k, "t1", "hello", `{"key":"k2","from":".missing"}`, ""); err == nil {
		t.Fatal("absent from-path must error")
	}
}

func TestKVGetMissingFallback(t *testing.T) {
	k := newKVHandle(t)
	// missing + fallback → fallback lands at into
	pay, _ := callKV(t, kvGet, k, "t1", "hello", `{"key":"absent","fallback":{"d":true}}`, "")
	if !gjson.Get(pay.Raw, "_kv.d").Bool() {
		t.Fatalf("fallback not applied: raw=%s", pay.Raw)
	}
	// missing + no fallback → nothing written
	pay, _ = callKV(t, kvGet, k, "t1", "hello", `{"key":"absent"}`, "")
	if gjson.Get(pay.Raw, "_kv").Exists() {
		t.Fatalf("missing key must write nothing: raw=%s", pay.Raw)
	}
}

func TestKVIncrOp(t *testing.T) {
	k := newKVHandle(t)
	pay, _ := callKV(t, kvIncr, k, "t1", "hello", `{"key":"c"}`, "") // default by=1
	if gjson.Get(pay.Raw, "_kv").Int() != 1 {
		t.Fatalf("incr1: raw=%s", pay.Raw)
	}
	pay, _ = callKV(t, kvIncr, k, "t1", "hello", `{"key":"c","by":2}`, "")
	if gjson.Get(pay.Raw, "_kv").Int() != 3 {
		t.Fatalf("incr2: raw=%s", pay.Raw)
	}
}

func TestKVNamespaceDefaultsToStack(t *testing.T) {
	k := newKVHandle(t)
	// set in stack "alpha" (default namespace = stack)
	if _, err := callKV(t, kvSet, k, "t1", "alpha", `{"key":"k","value":1}`, ""); err != nil {
		t.Fatal(err)
	}
	// read from a different stack → different default namespace → miss
	pay, _ := callKV(t, kvGet, k, "t1", "beta", `{"key":"k"}`, "")
	if gjson.Get(pay.Raw, "_kv").Exists() {
		t.Fatalf("cross-stack default namespace must not see the key: raw=%s", pay.Raw)
	}
	// same stack → hit
	pay, _ = callKV(t, kvGet, k, "t1", "alpha", `{"key":"k"}`, "")
	if gjson.Get(pay.Raw, "_kv").Int() != 1 {
		t.Fatalf("same-stack read missed: raw=%s", pay.Raw)
	}
	// explicit shared namespace crosses stacks
	if _, err := callKV(t, kvSet, k, "t1", "alpha", `{"key":"s","namespace":"shared","value":9}`, ""); err != nil {
		t.Fatal(err)
	}
	pay, _ = callKV(t, kvGet, k, "t1", "beta", `{"key":"s","namespace":"shared"}`, "")
	if gjson.Get(pay.Raw, "_kv").Int() != 9 {
		t.Fatalf("explicit shared namespace must cross stacks: raw=%s", pay.Raw)
	}
}

func TestKVRequiresTenant(t *testing.T) {
	k := newKVHandle(t)
	// no tenant pinned on ctx → error
	ctx := operation.WithMeta(context.Background(), `{"key":"k","value":1}`)
	if _, err := kvSet(ctx, k, []byte(`{}`)); err == nil {
		t.Fatal("kv/set without tenant scope must error")
	}
}

func TestKVValidation(t *testing.T) {
	k := newKVHandle(t)
	cases := map[string]struct {
		fn   kvHandler
		meta string
	}{
		"get missing key": {kvGet, `{}`},
		"set missing key": {kvSet, `{"value":1}`},
		"set no value":    {kvSet, `{"key":"k"}`},
		"delete no key":   {kvDelete, `{}`},
		"incr no key":     {kvIncr, `{}`},
		"cas no value":    {kvCAS, `{"key":"k"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := callKV(t, c.fn, k, "t1", "hello", c.meta, ""); err == nil {
				t.Fatalf("%s must error", name)
			}
		})
	}
}

func TestKVCASOp(t *testing.T) {
	k := newKVHandle(t)

	// set-if-absent (no `expected`): first hit swaps + creates, second refuses.
	pay, _ := callKV(t, kvCAS, k, "t1", "hello", `{"key":"lock","value":"held"}`, "")
	if !gjson.Get(pay.Raw, "_kv.swapped").Bool() {
		t.Fatalf("first set-if-absent should swap: %s", pay.Raw)
	}
	pay, _ = callKV(t, kvCAS, k, "t1", "hello", `{"key":"lock","value":"again"}`, "")
	if gjson.Get(pay.Raw, "_kv.swapped").Bool() {
		t.Fatalf("second set-if-absent must not swap: %s", pay.Raw)
	}
	if gjson.Get(pay.Raw, "_kv.current").String() != "held" {
		t.Fatalf("current should report the held value: %s", pay.Raw)
	}

	// value-match: correct `expected` swaps.
	pay, _ = callKV(t, kvCAS, k, "t1", "hello", `{"key":"lock","expected":"held","value":"taken"}`, "")
	if !gjson.Get(pay.Raw, "_kv.swapped").Bool() {
		t.Fatalf("value-match should swap: %s", pay.Raw)
	}
	// stale `expected` must not swap, and reports the real current.
	pay, _ = callKV(t, kvCAS, k, "t1", "hello", `{"key":"lock","expected":"held","value":"x"}`, "")
	if gjson.Get(pay.Raw, "_kv.swapped").Bool() {
		t.Fatalf("stale expected must not swap: %s", pay.Raw)
	}
	if gjson.Get(pay.Raw, "_kv.current").String() != "taken" {
		t.Fatalf("current after failed swap: %s", pay.Raw)
	}

	// value from an envelope path.
	pay, _ = callKV(t, kvCAS, k, "t1", "hello", `{"key":"k2","from":".payload"}`, `{"payload":{"a":1}}`)
	if !gjson.Get(pay.Raw, "_kv.swapped").Bool() || gjson.Get(pay.Raw, "_kv.current.a").Int() != 1 {
		t.Fatalf("from-path cas should swap and report current: %s", pay.Raw)
	}
}
