package websocket

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kvtools/boltdb"
	"github.com/kvtools/valkeyrie"

	kvstore "github.com/loremlabs/thanks-computer/chassis/kv"
)

func newTestKV(t *testing.T) *kvstore.KV {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := valkeyrie.NewStore(context.Background(), boltdb.StoreName,
		[]string{path}, &boltdb.Config{Bucket: "test", PersistConnection: true})
	if err != nil {
		t.Fatalf("boltdb: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return kvstore.New(s, 0, 0)
}

func TestKVDirectoryRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := NewKVDirectory(newTestKV(t), time.Hour)

	if _, found, err := d.Lookup(ctx, "acme", "ws_1"); err != nil || found {
		t.Fatalf("empty lookup = found:%v err:%v", found, err)
	}
	if err := d.Register(ctx, "acme", "ws_1", "fly-web-a", "counter"); err != nil {
		t.Fatal(err)
	}
	node, found, err := d.Lookup(ctx, "acme", "ws_1")
	if err != nil || !found || node != "fly-web-a" {
		t.Fatalf("lookup = %q found:%v err:%v", node, found, err)
	}
	// Per-tenant keys: another tenant's lookup of the same id misses.
	if _, found, _ := d.Lookup(ctx, "mallory", "ws_1"); found {
		t.Fatal("cross-tenant lookup found the entry")
	}
	if err := d.Refresh(ctx, "acme", "ws_1", "fly-web-b", "counter"); err != nil {
		t.Fatal(err)
	}
	if node, _, _ := d.Lookup(ctx, "acme", "ws_1"); node != "fly-web-b" {
		t.Fatalf("refresh did not rewrite: %q", node)
	}
	if err := d.Unregister(ctx, "acme", "ws_1"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := d.Lookup(ctx, "acme", "ws_1"); found {
		t.Fatal("entry survived unregister")
	}
	if err := d.Unregister(ctx, "acme", "ws_1"); err != nil {
		t.Fatalf("unregister of a missing entry must be a no-op: %v", err)
	}
}

func TestKVDirectoryLeaseExpires(t *testing.T) {
	ctx := context.Background()
	// The KV layer stamps expiry in whole seconds, so the smallest lease a
	// test can observe expiring is one second.
	d := NewKVDirectory(newTestKV(t), time.Second)
	if err := d.Register(ctx, "acme", "ws_2", "fly-web-a", "counter"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := d.Lookup(ctx, "acme", "ws_2"); !found {
		t.Fatal("fresh lease missing")
	}
	time.Sleep(1200 * time.Millisecond)
	if _, found, _ := d.Lookup(ctx, "acme", "ws_2"); found {
		t.Fatal("expired lease still found")
	}
}

func TestKVDirectoryUnreadableEntryIsMiss(t *testing.T) {
	ctx := context.Background()
	kv := newTestKV(t)
	d := NewKVDirectory(kv, time.Hour)
	if err := kv.Set(ctx, "acme", DirectoryNamespace, "ws_3", []byte(`{"stack":"x"}`), 0); err != nil {
		t.Fatal(err)
	}
	if _, found, err := d.Lookup(ctx, "acme", "ws_3"); found || err != nil {
		t.Fatalf("entry without a node = found:%v err:%v, want a plain miss", found, err)
	}
	if NewKVDirectory(kv, 0).(*kvDirectory).ttl != time.Hour {
		t.Fatal("zero ttl must default to an hour")
	}
}
