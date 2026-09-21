package blevestore_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/search/blevestore"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// TestMeasure prints the numbers a capacity decision needs. It asserts
// nothing and is skipped unless asked for:
//
//	TXCO_SEARCH_MEASURE=50000 go test -tags sqlite_fts5 -run TestMeasure -v ./chassis/search/blevestore/
func TestMeasure(t *testing.T) {
	n, _ := strconv.Atoi(os.Getenv("TXCO_SEARCH_MEASURE"))
	if n <= 0 {
		t.Skip("set TXCO_SEARCH_MEASURE=<records> to run")
	}
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "search")
	st, err := blevestore.New(root, 2000)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// One large collection: indexing rate, size, latency, filter writes.
	words := []string{"pool", "pump", "invoice", "gate", "cleaning", "rota", "boiler", "contract", "parking", "luggage", "reception", "inspection", "plant", "garden", "laundry", "keys"}
	text := func(i int) string {
		s := fmt.Sprintf("Ticket TXC-%06d.", i)
		for k := 0; k < 120; k++ {
			s += " " + words[(i*7+k*k+k)%len(words)]
		}
		return s
	}
	if err := st.EnsureCollection(ctx, "acme", search.Collection{Name: "big"}); err != nil {
		t.Fatal(err)
	}
	var bytes int
	start := time.Now()
	for i := 0; i < n; i += 500 {
		var items []search.Item
		for j := i; j < i+500 && j < n; j++ {
			tx := text(j)
			bytes += len(tx)
			items = append(items, search.Item{ID: fmt.Sprintf("d%d:%d", j/100, j%100), Text: tx, Name: fmt.Sprintf("ticket-%06d.md", j),
				Metadata: map[string]any{"slug": "acme", "doc_key": fmt.Sprintf("d%d", j/100), "idx": float64(j % 100), "audience": "anyone"}})
		}
		if _, err := st.Upsert(ctx, "acme", "big", items); err != nil {
			t.Fatal(err)
		}
	}
	took := time.Since(start)
	t.Logf("index   %d records, %.1f MB text, %s (%.0f records/s)", n, float64(bytes)/1e6, took.Round(time.Millisecond), float64(n)/took.Seconds())

	var lat []time.Duration
	for i := 0; i < 300; i++ {
		q := fmt.Sprintf("hello, what happened with TXC-%06d and the %s %s near reception?", (i*977)%n, words[i%len(words)], words[(i+5)%len(words)])
		t0 := time.Now()
		if _, err := st.Query(ctx, "acme", "big", q, 6, search.Filter{Conditions: []search.Condition{{Field: "audience", Op: vector.OpEq, Value: "anyone"}}}); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(t0))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("query   p50 %s  p95 %s  p99 %s (filtered, limit 6, a sentence with an identifier)", lat[150].Round(10*time.Microsecond), lat[285].Round(10*time.Microsecond), lat[297].Round(10*time.Microsecond))

	for _, k := range []int{1, 10} { // documents of 100 chunks each
		f := search.Filter{Conditions: []search.Condition{{Field: "doc_key", Op: vector.OpIn, Value: docKeys(k, 0)}}}
		t0 := time.Now()
		got, err := st.Update(ctx, "acme", "big", f, search.Change{Merge: map[string]any{"audience": "team"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("update  %5d records by filter: %s", got, time.Since(t0).Round(time.Millisecond))
		f = search.Filter{Conditions: []search.Condition{{Field: "doc_key", Op: vector.OpIn, Value: docKeys(k, 50)}}}
		t0 = time.Now()
		got, err = st.Delete(ctx, "acme", "big", search.Selector{Filter: f})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("delete  %5d records by filter: %s", got, time.Since(t0).Round(time.Millisecond))
	}

	// Many small collections: what an open index costs, and a cold open.
	const small = 300
	for c := 0; c < small; c++ {
		name := fmt.Sprintf("pony-%03d", c)
		if err := st.EnsureCollection(ctx, "acme", search.Collection{Name: name}); err != nil {
			t.Fatal(err)
		}
		var items []search.Item
		for j := 0; j < 50; j++ {
			items = append(items, search.Item{ID: fmt.Sprintf("c%d", j), Text: text(c*50 + j)})
		}
		if _, err := st.Upsert(ctx, "acme", name, items); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("disk    %.1f MB for the big collection + %d small ones (%.2fx the big one's text)", float64(dirBytes(root))/1e6, small, float64(dirBytes(root))/float64(bytes))

	st2, err := blevestore.New(root, 2000)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	g0 := runtime.NumGoroutine()
	t0 := time.Now()
	for c := 0; c < small; c++ {
		if _, err := st2.Query(ctx, "acme", fmt.Sprintf("pony-%03d", c), "pool pump", 6, search.Filter{}); err != nil {
			t.Fatal(err)
		}
	}
	cold := time.Since(t0)
	runtime.GC()
	runtime.ReadMemStats(&m1)
	t.Logf("open    %d small indexes: %.1f ms each cold (open + first query); per open index: %.0f KB heap, %.1f goroutines",
		small, float64(cold.Milliseconds())/small, float64(m1.HeapAlloc-m0.HeapAlloc)/1024/small,
		float64(runtime.NumGoroutine()-g0)/small)
}

func docKeys(k, from int) []any {
	out := make([]any, k)
	for i := range out {
		out[i] = fmt.Sprintf("d%d", from+i)
	}
	return out
}

func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}
