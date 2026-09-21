package blevestore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/search"
)

// The store keeps only recently used indexes open, so what a closed one costs
// to reopen decides how small that set can be.
func BenchmarkReopenCollection(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "idx")
	c, err := OpenCollection(dir)
	if err != nil {
		b.Fatal(err)
	}
	for batch := 0; batch < 5; batch++ {
		var items []search.Item
		for i := 0; i < 200; i++ {
			n := batch*200 + i
			items = append(items, search.Item{
				ID:       fmt.Sprintf("doc:%04d", n),
				Text:     fmt.Sprintf("Ticket TXC-%04d concerns the pool pump in building %d, reported by the front desk after the weekly inspection of the plant room.", n, n%7),
				Name:     fmt.Sprintf("ticket-%04d.md", n),
				Metadata: map[string]any{"slug": "paris", "idx": float64(n)},
			})
		}
		if err := c.Upsert(items, nil); err != nil {
			b.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := OpenCollection(dir)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := c.Query(context.Background(), "TXC-0417 pool pump", 6, search.Filter{}); err != nil {
			b.Fatal(err)
		}
		if err := c.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
