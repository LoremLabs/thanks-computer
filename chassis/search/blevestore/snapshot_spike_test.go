package blevestore

import (
	"context"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/blevesearch/bleve/v2"

	"github.com/loremlabs/thanks-computer/chassis/search"
)

// The hosted design (overlay docs/todo-lexical-search-stateless.md §34) rests
// on three claims about Bleve. This test pins them before anything is built on
// top:
//
//  1. a cursor written with SetInternal in the same batch as the records
//     travels inside the index;
//  2. CopyTo, taken while the source stays open and keeps taking writes, yields
//     an index that opens on its own;
//  3. that copy answers queries exactly as the source did at the moment of the
//     copy, and reports the cursor of that moment, not a later one.
func TestSnapshotCarriesCursorAndResults(t *testing.T) {
	const cursorKey = "txco.applied_seq"
	seq := func(n uint64) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, n)
		return b
	}
	dir := t.TempDir()
	src, err := OpenCollection(filepath.Join(dir, "src"))
	if err != nil {
		t.Fatalf("OpenCollection: %v", err)
	}
	defer src.Close()

	// Several batches, so the index has more than one segment to copy.
	for batch := 0; batch < 5; batch++ {
		var items []search.Item
		for i := 0; i < 40; i++ {
			n := batch*40 + i
			items = append(items, search.Item{
				ID:       fmt.Sprintf("doc:%03d", n),
				Text:     fmt.Sprintf("Ticket TXC-%04d concerns the pool pump in building %d, reported by the front desk.", 4000+n, n%7),
				Metadata: map[string]any{"slug": "paris", "idx": float64(n)},
			})
		}
		if err := src.Upsert(items, map[string][]byte{cursorKey: seq(uint64(100 + batch))}); err != nil {
			t.Fatalf("Upsert batch %d: %v", batch, err)
		}
	}

	queries := []string{"TXC-4117", "pool pump building 3", `"front desk"`, "nothing matches this zebra"}
	before := map[string][]string{}
	for _, q := range queries {
		hits, err := src.Query(context.Background(), q, 6, search.Filter{})
		if err != nil {
			t.Fatalf("Query(%q): %v", q, err)
		}
		before[q] = hitIDs(hits)
	}

	copyable, ok := src.Index().(bleve.IndexCopyable)
	if !ok {
		t.Fatalf("index does not implement bleve.IndexCopyable")
	}
	dst := filepath.Join(dir, "copy")
	if err := copyable.CopyTo(bleve.FileSystemDirectory(dst)); err != nil {
		t.Fatalf("CopyTo: %v", err)
	}

	// The source moves on after the copy. The copy must not see this.
	if err := src.Upsert([]search.Item{{ID: "late", Text: "TXC-4117 again, a later duplicate about the pool pump."}},
		map[string][]byte{cursorKey: seq(999)}); err != nil {
		t.Fatalf("late Upsert: %v", err)
	}

	cp, err := OpenCollection(dst)
	if err != nil {
		t.Fatalf("open the copy: %v", err)
	}
	defer cp.Close()

	got, err := cp.Internal(cursorKey)
	if err != nil || binary.BigEndian.Uint64(got) != 104 {
		t.Fatalf("copy cursor = %v (err %v), want 104", got, err)
	}
	if n, _ := cp.Count(); n != 200 {
		t.Errorf("copy Count = %d, want 200", n)
	}
	for _, q := range queries {
		hits, err := cp.Query(context.Background(), q, 6, search.Filter{})
		if err != nil {
			t.Fatalf("copy Query(%q): %v", q, err)
		}
		if !reflect.DeepEqual(hitIDs(hits), before[q]) {
			t.Errorf("copy Query(%q) = %v, source gave %v", q, hitIDs(hits), before[q])
		}
	}

	// The copy is a full index: it takes writes of its own.
	if err := cp.Upsert([]search.Item{{ID: "replayed", Text: "zebra crossing"}}, map[string][]byte{cursorKey: seq(105)}); err != nil {
		t.Fatalf("write to the copy: %v", err)
	}
	if hits, _ := cp.Query(context.Background(), "zebra", 6, search.Filter{}); len(hits) != 1 || hits[0].ID != "replayed" {
		t.Errorf("copy after write = %v", hitIDs(hits))
	}
}
