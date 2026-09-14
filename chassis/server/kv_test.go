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
		"mget no items":   {kvMGet, `{}`},
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

func TestKVMGetOp(t *testing.T) {
	k := newKVHandle(t)
	for _, meta := range []string{
		`{"key":"k1","value":1}`, // the stack's namespace, "hello"
		`{"key":"triggers","namespace":"pony-a","value":{"at":"09:00"}}`,
		`{"key":"triggers","namespace":"pony-b","value":{"at":"10:00"}}`,
	} {
		if _, err := callKV(t, kvSet, k, "t1", "hello", meta, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Per-item namespaces, a bare key in the stack's namespace, a miss — in order.
	pay, err := callKV(t, kvMGet, k, "t1", "hello", `{"items":[
		{"key":"triggers","namespace":"pony-b"},
		"k1",
		{"key":"nope","namespace":"pony-a"},
		{"key":"triggers","namespace":"pony-a"}]}`, "")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"_kv.count":             "4",
		"_kv.found":             "3",
		"_kv.items.0.namespace": "pony-b",
		"_kv.items.0.found":     "true",
		"_kv.items.0.value.at":  "10:00",
		"_kv.items.1.namespace": "hello",
		"_kv.items.1.key":       "k1",
		"_kv.items.1.value":     "1",
		"_kv.items.2.key":       "nope",
		"_kv.items.2.found":     "false",
		"_kv.items.3.value.at":  "09:00",
	} {
		if got := gjson.Get(pay.Raw, path).String(); got != want {
			t.Fatalf("%s = %q, want %q: raw=%s", path, got, want, pay.Raw)
		}
	}
	if gjson.Get(pay.Raw, "_kv.items.2.value").Exists() {
		t.Fatalf("a miss must carry no value: raw=%s", pay.Raw)
	}

	// `into`, and a `_`-nested inlet sub-stack defaulting to the app's namespace.
	pay, err = callKV(t, kvMGet, k, "t1", "hello/_mail", `{"items":["k1"],"into":"_m"}`, "")
	if err != nil || gjson.Get(pay.Raw, "_m.items.0.namespace").String() != "hello" ||
		gjson.Get(pay.Raw, "_m.items.0.value").Int() != 1 {
		t.Fatalf("nested stack / into: raw=%s err=%v", pay.Raw, err)
	}

	// The call's namespace is the default for items that don't name one.
	pay, err = callKV(t, kvMGet, k, "t1", "other",
		`{"namespace":"pony-a","items":["triggers",{"key":"triggers","namespace":"pony-b"}]}`, "")
	if err != nil || gjson.Get(pay.Raw, "_kv.items.0.value.at").String() != "09:00" ||
		gjson.Get(pay.Raw, "_kv.items.1.value.at").String() != "10:00" {
		t.Fatalf("call namespace default: raw=%s err=%v", pay.Raw, err)
	}

	// No items: an empty answer, not an error.
	pay, err = callKV(t, kvMGet, k, "t1", "hello", `{"items":[]}`, "")
	if err != nil || gjson.Get(pay.Raw, "_kv.items").Raw != "[]" ||
		gjson.Get(pay.Raw, "_kv.count").Int() != 0 || !gjson.Get(pay.Raw, "_kv.found").Exists() {
		t.Fatalf("empty items: raw=%s err=%v", pay.Raw, err)
	}

	// Refusals take the whole call and write nothing.
	over, _ := sjson.Set(`{}`, "items", make([]string, 201))
	for name, meta := range map[string]string{
		"reserved item namespace": `{"items":["k1",{"key":"x","namespace":"_txc.blob"}]}`,
		"slash in a key":          `{"items":["k1","a/b"]}`,
		"item without a key":      `{"items":[{"namespace":"pony-a"}]}`,
		"a number item":           `{"items":[42]}`,
		"items not an array":      `{"items":"k1"}`,
		"over the item cap":       over,
	} {
		pay, err := callKV(t, kvMGet, k, "t1", "hello", meta, "")
		if err == nil || gjson.Get(pay.Raw, "_kv").Exists() {
			t.Fatalf("%s: must refuse the whole call: raw=%s err=%v", name, pay.Raw, err)
		}
	}
}

func TestKVListValues(t *testing.T) {
	k := newKVHandle(t)
	for _, kv := range [][2]string{{"b", `{"n":2}`}, {"a", `{"n":1}`}, {"c", `{"n":3}`}} {
		meta := `{"key":"` + kv[0] + `","namespace":"subs","value":` + kv[1] + `}`
		if _, err := callKV(t, kvSet, k, "t1", "hello", meta, ""); err != nil {
			t.Fatal(err)
		}
	}

	// Keys only by default: no rows.
	pay, err := callKV(t, kvList, k, "t1", "hello", `{"namespace":"subs","limit":2}`, "")
	if err != nil || gjson.Get(pay.Raw, "_kv.keys").Raw != `["a","b"]` ||
		gjson.Get(pay.Raw, "_kv.rows").Exists() {
		t.Fatalf("keys only: raw=%s err=%v", pay.Raw, err)
	}

	// values = true: rows beside unchanged keys / next / count, page by page.
	pay, err = callKV(t, kvList, k, "t1", "hello", `{"namespace":"subs","limit":2,"values":true}`, "")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"_kv.keys":         `["a","b"]`,
		"_kv.next":         `"b"`,
		"_kv.count":        "2",
		"_kv.rows.#":       "2",
		"_kv.rows.0.key":   `"a"`,
		"_kv.rows.0.value": `{"n":1}`,
		"_kv.rows.1.value": `{"n":2}`,
	} {
		if got := gjson.Get(pay.Raw, path).Raw; got != want {
			t.Fatalf("%s = %s, want %s: raw=%s", path, got, want, pay.Raw)
		}
	}
	pay, _ = callKV(t, kvList, k, "t1", "hello", `{"namespace":"subs","limit":2,"values":true,"after":"b"}`, "")
	if gjson.Get(pay.Raw, "_kv.rows.#").Int() != 1 || gjson.Get(pay.Raw, "_kv.rows.0.key").String() != "c" ||
		gjson.Get(pay.Raw, "_kv.rows.0.value").Raw != `{"n":3}` || gjson.Get(pay.Raw, "_kv.next").String() != "" {
		t.Fatalf("last page: raw=%s", pay.Raw)
	}

	// An empty namespace: [] for both, never null.
	pay, _ = callKV(t, kvList, k, "t1", "hello", `{"namespace":"empty","values":true}`, "")
	if gjson.Get(pay.Raw, "_kv.rows").Raw != "[]" || gjson.Get(pay.Raw, "_kv.keys").Raw != "[]" {
		t.Fatalf("empty namespace: raw=%s", pay.Raw)
	}
}

// A `_`-nested inlet sub-stack (mail at <stack>/_mail, a WebSocket session
// at <stack>/_websocket) shares its app stack's KV namespace by default: the
// nested name is not a legal namespace segment (it contains `/`), and the
// app is the unit that owns state across inlets.
func TestKVNamespaceNestedInletStackSharesApp(t *testing.T) {
	for in, want := range map[string]string{
		"counter":               "counter",
		"counter/_websocket":    "counter",
		"test-01/_mail":         "test-01",
		"a/_mail/deeper":        "a",
		"plain/nested":          "plain/nested", // no inlet segment: untouched
		"_cron":                 "_cron",
		"":                      "",
		"counter/_websocket/_x": "counter",
	} {
		if got := appStackNamespace(in); got != want {
			t.Errorf("appStackNamespace(%q) = %q, want %q", in, got, want)
		}
	}

	k := newKVHandle(t)
	if _, err := callKV(t, kvSet, k, "t1", "counter", `{"key":"k","value":1}`, ""); err != nil {
		t.Fatal(err)
	}
	pay, err := callKV(t, kvGet, k, "t1", "counter/_websocket", `{"key":"k"}`, "")
	if err != nil {
		t.Fatalf("nested sub-stack kv op errored: %v", err)
	}
	if gjson.Get(pay.Raw, "_kv").Int() != 1 {
		t.Fatalf("nested sub-stack must read the app's namespace: raw=%s", pay.Raw)
	}
	pay, err = callKV(t, kvIncr, k, "t1", "counter/_websocket", `{"key":"n"}`, "")
	if err != nil || gjson.Get(pay.Raw, "_kv").Int() != 1 {
		t.Fatalf("incr from the nested sub-stack: raw=%s err=%v", pay.Raw, err)
	}
	pay, _ = callKV(t, kvGet, k, "t1", "counter", `{"key":"n"}`, "")
	if gjson.Get(pay.Raw, "_kv").Int() != 1 {
		t.Fatalf("the app stack must see the nested sub-stack's write: raw=%s", pay.Raw)
	}
}
