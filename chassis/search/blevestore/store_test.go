package blevestore_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/search/blevestore"
	"github.com/loremlabs/thanks-computer/chassis/search/searchtest"
)

func newStore(t *testing.T) search.Store {
	t.Helper()
	s, err := blevestore.New(filepath.Join(t.TempDir(), "search"), 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) { searchtest.RunConformance(t, newStore) }

// The suite again with room for only two open indexes, so nearly every call
// reopens a collection that was closed to make room. Behaviour must not depend
// on what happens to be open.
func TestConformanceUnderEviction(t *testing.T) {
	searchtest.RunConformance(t, func(t *testing.T) search.Store {
		s, err := blevestore.New(filepath.Join(t.TempDir(), "search"), 2)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return s
	})
}

func TestRegisteredAsBleve(t *testing.T) {
	s, err := search.Open("bleve", search.Config{Path: filepath.Join(t.TempDir(), "search")})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if _, ok := s.(*blevestore.Store); !ok {
		t.Fatalf("Open(bleve) = %T", s)
	}
	if s, err := search.Open(search.StoreNone, search.Config{}); s != nil || err != nil {
		t.Fatalf("Open(none) = %v, %v; want nil, nil", s, err)
	}
	if _, err := search.Open("nope", search.Config{}); err == nil {
		t.Fatalf("Open(nope): want an error")
	}
}

// What the conformance suite cannot express: the records are on disk, and a
// fresh process finds them.
func TestDurabilityAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "search")
	s, err := blevestore.New(dir, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.EnsureCollection(ctx, "acme", search.Collection{Name: "docs"}); err != nil {
		t.Fatalf("EnsureCollection: %v", err)
	}
	if _, err := s.Upsert(ctx, "acme", "docs", []search.Item{{ID: "a", Text: "durable words", Metadata: map[string]any{"k": "v"}}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := s.Query(ctx, "acme", "docs", "durable", 5, search.Filter{}); err == nil {
		t.Fatalf("Query after Close: want an error")
	}

	s2, err := blevestore.New(dir, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	hits, err := s2.Query(ctx, "acme", "docs", "durable", 5, search.Filter{})
	if err != nil || len(hits) != 1 || hits[0].ID != "a" || hits[0].Metadata["k"] != "v" {
		t.Fatalf("after reopen: %+v, %v", hits, err)
	}
	cols, err := s2.ListCollections(ctx, "acme")
	if err != nil || len(cols) != 1 || cols[0].Name != "docs" || cols[0].Records != 1 {
		t.Fatalf("ListCollections after reopen: %+v, %v", cols, err)
	}
}

// Many goroutines over more collections than may be open at once: no call
// fails, no record is lost, and the open set returns to its bound.
func TestConcurrentUseUnderEviction(t *testing.T) {
	ctx := context.Background()
	const maxOpen, collections, workers, rounds = 3, 8, 12, 25
	st, err := blevestore.New(filepath.Join(t.TempDir(), "search"), maxOpen)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer st.Close()
	for c := 0; c < collections; c++ {
		if err := st.EnsureCollection(ctx, "acme", search.Collection{Name: fmt.Sprintf("c%d", c)}); err != nil {
			t.Fatalf("EnsureCollection: %v", err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				coll := fmt.Sprintf("c%d", (w+r)%collections)
				id := fmt.Sprintf("w%d-r%d", w, r)
				if _, err := st.Upsert(ctx, "acme", coll, []search.Item{{ID: id, Text: "shared words " + id}}); err != nil {
					errs <- fmt.Errorf("upsert %s: %w", id, err)
					return
				}
				if hits, err := st.Query(ctx, "acme", coll, id, 3, search.Filter{}); err != nil || len(hits) == 0 || hits[0].ID != id {
					errs <- fmt.Errorf("query %s in %s: %v %v", id, coll, hits, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var total uint64
	cols, err := st.ListCollections(ctx, "acme")
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	for _, c := range cols {
		total += c.Records
	}
	if total != workers*rounds {
		t.Errorf("records = %d, want %d", total, workers*rounds)
	}
	if n := st.OpenCount(); n > maxOpen {
		t.Errorf("open indexes = %d, want <= %d once idle", n, maxOpen)
	}
}
