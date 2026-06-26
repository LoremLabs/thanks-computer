package kvseed_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
	"github.com/loremlabs/thanks-computer/chassis/storeseed/kvseed"
)

func newKV(t *testing.T) *kvstore.KV {
	t.Helper()
	ctx := context.Background()
	s, err := valkeyrie.NewStore(ctx, boltdb.StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &boltdb.Config{Bucket: "txco"})
	if err != nil {
		t.Fatalf("open boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return kvstore.New(s, 0, 0)
}

func pack(name string, lines ...string) storeseed.RawPack {
	var body []byte
	for i, ln := range lines {
		if i > 0 {
			body = append(body, '\n')
		}
		body = append(body, []byte(ln)...)
	}
	p, _ := storeseed.NewRawPack("KV/"+name+".jsonl", body)
	return p
}

func has(ks []string, want string) bool {
	for _, k := range ks {
		if k == want {
			return true
		}
	}
	return false
}

func TestReconcileSetDeleteMissing(t *testing.T) {
	ctx := context.Background()
	kv := newKV(t)
	m := kvseed.New(kv, false)
	scope := storeseed.Scope{Tenant: "acme", Stack: "recs", Version: 1}

	p1 := pack("config",
		`{"key":"welcome","value":{"subject":"Hi","body":"Welcome"}}`,
		`{"key":"max_recs","value":3}`,
		`{"key":"flag","value":true}`,
	)
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{p1}); err != nil {
		t.Fatalf("reconcile v1: %v", err)
	}
	// Values round-trip.
	if v, found, _ := kv.Get(ctx, "acme", "config", "max_recs"); !found || string(v) != "3" {
		t.Fatalf("max_recs = %q found=%v want 3", v, found)
	}
	if v, found, _ := kv.Get(ctx, "acme", "config", "welcome"); !found || string(v) != `{"subject":"Hi","body":"Welcome"}` {
		t.Fatalf("welcome = %q found=%v", v, found)
	}
	keys, err := kv.ListKeys(ctx, "acme", "config")
	if err != nil || len(keys) != 3 {
		t.Fatalf("ListKeys v1: %v err=%v want 3", keys, err)
	}

	// v2: drop flag, change max_recs, add footer → delete-missing removes flag.
	p2 := pack("config",
		`{"key":"welcome","value":{"subject":"Hi","body":"Welcome"}}`,
		`{"key":"max_recs","value":5}`,
		`{"key":"footer","value":"unsub"}`,
	)
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{p2}); err != nil {
		t.Fatalf("reconcile v2: %v", err)
	}
	keys, _ = kv.ListKeys(ctx, "acme", "config")
	if len(keys) != 3 || has(keys, "flag") || !has(keys, "footer") {
		t.Fatalf("after v2: %v want [welcome max_recs footer] (flag dropped)", keys)
	}
	if v, _, _ := kv.Get(ctx, "acme", "config", "max_recs"); string(v) != "5" {
		t.Fatalf("max_recs not updated: %q want 5", v)
	}
	if _, found, _ := kv.Get(ctx, "acme", "config", "flag"); found {
		t.Fatal("flag should have been delete-missing'd")
	}

	// v3: empty pack → namespace emptied.
	if err := m.Reconcile(ctx, scope, []storeseed.RawPack{pack("config")}); err != nil {
		t.Fatalf("reconcile empty: %v", err)
	}
	if keys, _ := kv.ListKeys(ctx, "acme", "config"); len(keys) != 0 {
		t.Fatalf("after empty pack: %v want 0", keys)
	}
}

func TestReconcileMalformedPacks(t *testing.T) {
	ctx := context.Background()
	m := kvseed.New(newKV(t), false)
	scope := storeseed.Scope{Tenant: "acme", Version: 1}

	bad := map[string]storeseed.RawPack{
		"missing key":   pack("c", `{"value":1}`),
		"duplicate key": pack("c", `{"key":"a","value":1}`, `{"key":"a","value":2}`),
		"empty value":   pack("c", `{"key":"a"}`),
		"bad json":      pack("c", `{"key":"a","value":}`),
	}
	for name, p := range bad {
		if err := m.Reconcile(ctx, scope, []storeseed.RawPack{p}); err == nil {
			t.Errorf("%s: reconcile = nil, want error", name)
		}
	}
}

func TestFlags(t *testing.T) {
	if kvseed.New(newKV(t), true).Shared() != true {
		t.Error("Shared()=false, want true")
	}
	if k := kvseed.New(newKV(t), false).Kind(); k != storeseed.KindKV {
		t.Errorf("Kind()=%q, want %q", k, storeseed.KindKV)
	}
}
