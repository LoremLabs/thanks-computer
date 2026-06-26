package sqlitevec_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/vector"
	"github.com/loremlabs/thanks-computer/chassis/vector/sqlitevec"
	"github.com/loremlabs/thanks-computer/chassis/vector/vectortest"
)

func newStore(t *testing.T) vector.Store {
	t.Helper()
	s, err := sqlitevec.New(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestConformance(t *testing.T) {
	vectortest.RunConformance(t, newStore)
}

// TestDurabilityAcrossReopen proves vectors persist on disk (the whole point
// of not holding them in RAM): write, close, reopen the same file, search.
func TestDurabilityAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vec.db")

	s1, err := sqlitevec.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s1.EnsureCollection(ctx, "t", vector.Collection{Name: "c", Dimensions: 3, Metric: vector.MetricCosine}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if _, err := s1.Upsert(ctx, "t", "c", []vector.Item{
		{ID: "x", Vector: []float32{1, 0, 0}, Text: "keep", Metadata: map[string]any{"k": "v"}},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := sqlitevec.New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, err := s2.Search(ctx, "t", "c", []float32{1, 0, 0}, 5, vector.Filter{})
	if err != nil {
		t.Fatalf("search after reopen: %v", err)
	}
	if len(got) != 1 || got[0].ID != "x" || got[0].Text != "keep" || got[0].Metadata["k"] != "v" {
		t.Fatalf("durability: %+v", got)
	}
}
