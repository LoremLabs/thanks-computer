package boltstore

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/kvtools/valkeyrie"
	"github.com/kvtools/valkeyrie/store"
	"go.etcd.io/bbolt"
)

func openTestStore(t *testing.T, persist bool) *Store {
	t.Helper()
	s, err := New(context.Background(), []string{filepath.Join(t.TempDir(), "kv.db")},
		&Config{Bucket: "test", PersistConnection: persist})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestRegisteredAsBoltDB(t *testing.T) {
	kv, err := valkeyrie.NewStore(context.Background(), StoreName,
		[]string{filepath.Join(t.TempDir(), "kv.db")}, &Config{Bucket: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := kv.(*Store); !ok {
		t.Fatalf("the registry built %T, want *Store", kv)
	}
}

// The on-disk format is upstream's — an 8-byte little-endian index before
// the value — so a store written before the vendoring opens unchanged.
func TestOnDiskFormatUnchanged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kv.db")
	s, err := New(ctx, []string{path}, &Config{Bucket: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "a", []byte("hello"), nil); err != nil {
		t.Fatal(err)
	}
	db, err := bbolt.Open(path, filePerm, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_ = db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket([]byte("test")).Get([]byte("a"))
		if len(raw) != metadataLen+5 || string(raw[metadataLen:]) != "hello" ||
			binary.LittleEndian.Uint64(raw[:metadataLen]) == 0 {
			t.Fatalf("stored bytes = %q", raw)
		}
		return nil
	})
}

func TestSingleKeyOps(t *testing.T) {
	for name, persist := range map[string]bool{"transient": false, "persistent": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t, persist)

			if _, err := s.Get(ctx, "ns/a", nil); !errors.Is(err, store.ErrKeyNotFound) {
				t.Fatalf("Get missing: %v", err)
			}
			if err := s.Put(ctx, "ns/a", []byte("v1"), nil); err != nil {
				t.Fatal(err)
			}
			pair, err := s.Get(ctx, "ns/a", nil)
			if err != nil || string(pair.Value) != "v1" {
				t.Fatalf("Get = %v, %v", pair, err)
			}
			if pairs, err := s.List(ctx, "ns/", nil); err != nil || len(pairs) != 1 {
				t.Fatalf("List = %v, %v", pairs, err)
			}
			if _, _, err := s.AtomicPut(ctx, "ns/a", []byte("v2"), pair, nil); err != nil {
				t.Fatalf("AtomicPut: %v", err)
			}
			if _, _, err := s.AtomicPut(ctx, "ns/a", []byte("v3"), pair, nil); !errors.Is(err, store.ErrKeyModified) {
				t.Fatalf("AtomicPut against a stale pair: %v", err)
			}
			if err := s.Delete(ctx, "ns/a"); err != nil {
				t.Fatal(err)
			}
			if ok, _ := s.Exists(ctx, "ns/a", nil); ok {
				t.Fatal("key still exists after Delete")
			}
		})
	}
}

func TestGetMulti(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, false)

	// No bucket yet: every key is a miss, not an error.
	pairs, err := s.GetMulti(ctx, []string{"a", "b"})
	if err != nil || len(pairs) != 2 || pairs[0] != nil || pairs[1] != nil {
		t.Fatalf("before any write: %v, %v", pairs, err)
	}

	for _, k := range []string{"a", "c"} {
		if err := s.Put(ctx, k, []byte("v"+k), nil); err != nil {
			t.Fatal(err)
		}
	}
	pairs, err = s.GetMulti(ctx, []string{"c", "b", "a", "c"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vc", "", "va", "vc"}
	for i, p := range pairs {
		switch {
		case want[i] == "" && p != nil:
			t.Fatalf("pair %d: want a miss, got %q", i, p.Value)
		case want[i] != "" && (p == nil || string(p.Value) != want[i]):
			t.Fatalf("pair %d = %v, want %s", i, p, want[i])
		}
	}
}

func TestPutMultiIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, false)
	if err := s.Put(ctx, "keep", []byte("old"), nil); err != nil {
		t.Fatal(err)
	}

	// bbolt refuses an empty key, failing the transaction at the SECOND write:
	// the first must be rolled back and the third never lands.
	err := s.PutMulti(ctx, []*store.KVPair{
		{Key: "keep", Value: []byte("new")},
		{Key: "", Value: []byte("x")},
		{Key: "c", Value: []byte("y")},
	}, nil)
	if err == nil {
		t.Fatal("a batch with an empty key must fail")
	}
	if p, _ := s.Get(ctx, "keep", nil); p == nil || string(p.Value) != "old" {
		t.Fatalf("a failed batch changed keep: %v", p)
	}
	if _, err := s.Get(ctx, "c", nil); !errors.Is(err, store.ErrKeyNotFound) {
		t.Fatalf("a failed batch wrote c: %v", err)
	}

	// A clean batch lands whole.
	if err := s.PutMulti(ctx, []*store.KVPair{
		{Key: "keep", Value: []byte("new")},
		{Key: "c", Value: []byte("y")},
	}, nil); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"keep": "new", "c": "y"} {
		if p, _ := s.Get(ctx, k, nil); p == nil || string(p.Value) != want {
			t.Fatalf("%s = %v, want %s", k, p, want)
		}
	}
}

func TestDeleteMulti(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, false)

	if err := s.DeleteMulti(ctx, []string{"a"}); err != nil {
		t.Fatalf("delete before any write: %v", err)
	}
	for _, k := range []string{"a", "b", "c"} {
		if err := s.Put(ctx, k, []byte("v"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteMulti(ctx, []string{"a", "c", "missing", "a"}); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]bool{"a": false, "b": true, "c": false} {
		if ok, _ := s.Exists(ctx, k, nil); ok != want {
			t.Fatalf("%s exists = %v, want %v", k, ok, want)
		}
	}
}
