// Command searchbench indexes a file of records into one blevestore collection
// and runs a file of queries against it. It exists to measure lexical
// retrieval offline: ranked ids per query go to stdout as JSON lines, and the
// index size, indexing rate and query latency go to stderr.
//
//	go run -tags sqlite_fts5 ./chassis/search/blevestore/searchbench \
//	    -records chunks.jsonl -queries golden.jsonl -limit 20 \
//	    -filter '{"audience":"anyone"}' > lexical.jsonl
//
// A record line is {"id","text","metadata"}, the shape a vector store dump has.
// With -lift (the default) metadata.title, metadata.heading and metadata.doc
// are also indexed as the searched fields title, heading and name, which is
// what a stack does when it upserts the same chunk to txco://search.
//
// A query line is any JSON object with a "q" string; the whole object is echoed
// beside its hits, so a golden set can carry its expectations through.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/search/blevestore"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

func main() {
	records := flag.String("records", "", "JSON lines of records to index (required)")
	queries := flag.String("queries", "", "JSON lines of queries, each with a \"q\" string (required)")
	limit := flag.Int("limit", 20, "hits per query")
	filterJSON := flag.String("filter", "", "metadata filter as JSON, vector-store grammar: scalar=eq, array=in, {op: value}")
	lift := flag.Bool("lift", true, "index metadata.title/heading/doc as the searched fields title/heading/name")
	batch := flag.Int("batch", 200, "records per index batch")
	keep := flag.String("keep", "", "keep the index at this directory instead of a temp dir")
	flag.Parse()
	if *records == "" || *queries == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*records, *queries, *limit, *filterJSON, *lift, *batch, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "searchbench:", err)
		os.Exit(1)
	}
}

func run(recordsPath, queriesPath string, limit int, filterJSON string, lift bool, batchSize int, keep string) error {
	filter, err := parseFilter(filterJSON)
	if err != nil {
		return err
	}
	dir := keep
	if dir == "" {
		tmp, err := os.MkdirTemp("", "searchbench-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		dir = filepath.Join(tmp, "idx")
	}
	col, err := blevestore.OpenCollection(dir)
	if err != nil {
		return err
	}
	var once sync.Once
	closeCol := func() { once.Do(func() { _ = col.Close() }) }
	defer closeCol()

	var items []search.Item
	var textBytes int
	if err := eachLine(recordsPath, func(line []byte) error {
		var it search.Item
		if err := json.Unmarshal(line, &it); err != nil {
			return err
		}
		if lift {
			liftFields(&it)
		}
		textBytes += len(it.Text)
		items = append(items, it)
		return nil
	}); err != nil {
		return fmt.Errorf("records: %w", err)
	}

	start := time.Now()
	for i := 0; i < len(items); i += batchSize {
		end := min(i+batchSize, len(items))
		if err := col.Upsert(items[i:end], nil); err != nil {
			return fmt.Errorf("index: %w", err)
		}
	}
	indexed := time.Since(start)

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	var lat []time.Duration
	if err := eachLine(queriesPath, func(line []byte) error {
		var q map[string]any
		if err := json.Unmarshal(line, &q); err != nil {
			return err
		}
		text, _ := q["q"].(string)
		t0 := time.Now()
		hits, err := col.Query(text, limit, filter)
		if err != nil {
			return err
		}
		lat = append(lat, time.Since(t0))
		type outHit struct {
			ID   string `json:"id"`
			Rank int    `json:"rank"`
			Doc  any    `json:"doc,omitempty"`
		}
		oh := make([]outHit, 0, len(hits))
		for _, h := range hits {
			oh = append(oh, outHit{ID: h.ID, Rank: h.Rank, Doc: h.Metadata["doc"]})
		}
		q["hits"] = oh
		enc, err := json.Marshal(q)
		if err != nil {
			return err
		}
		_, err = out.Write(append(enc, '\n'))
		return err
	}); err != nil {
		return fmt.Errorf("queries: %w", err)
	}

	// Close before measuring, so the persister has flushed what it holds.
	n, _ := col.Count()
	closeCol()
	size := dirBytes(dir)
	fmt.Fprintf(os.Stderr, "records        %d\n", n)
	fmt.Fprintf(os.Stderr, "text bytes     %d\n", textBytes)
	fmt.Fprintf(os.Stderr, "index bytes    %d (%.2fx text)\n", size, ratio(size, int64(textBytes)))
	fmt.Fprintf(os.Stderr, "index time     %s (%.0f records/s)\n", indexed.Round(time.Millisecond), float64(len(items))/indexed.Seconds())
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		fmt.Fprintf(os.Stderr, "queries        %d\n", len(lat))
		fmt.Fprintf(os.Stderr, "query p50/p95  %s / %s\n", pct(lat, 50), pct(lat, 95))
	}
	return nil
}

// liftFields copies the chunk's title, heading and document name out of
// metadata into the searched fields, leaving metadata as it was.
func liftFields(it *search.Item) {
	str := func(k string) string { s, _ := it.Metadata[k].(string); return s }
	if it.Title == "" {
		it.Title = str("title")
	}
	if it.Heading == "" {
		it.Heading = str("heading")
	}
	if it.Name == "" {
		it.Name = str("doc")
	}
}

// parseFilter reads the txcl filter grammar from JSON: a scalar is eq, an
// array is in, and an object is {op: value} pairs.
func parseFilter(s string) (search.Filter, error) {
	var f search.Filter
	if s == "" {
		return f, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return f, fmt.Errorf("filter: %w", err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := m[k].(type) {
		case []any:
			f.Conditions = append(f.Conditions, search.Condition{Field: k, Op: vector.OpIn, Value: v})
		case map[string]any:
			for op, ov := range v {
				f.Conditions = append(f.Conditions, search.Condition{Field: k, Op: vector.Op(op), Value: ov})
			}
		default:
			f.Conditions = append(f.Conditions, search.Condition{Field: k, Op: vector.OpEq, Value: v})
		}
	}
	return f, nil
}

func eachLine(path string, fn func([]byte) error) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(line); err != nil {
			return fmt.Errorf("%s:%d: %w", path, n, err)
		}
	}
	return sc.Err()
}

func dirBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func pct(sorted []time.Duration, p int) time.Duration {
	i := (len(sorted)*p + 99) / 100
	if i > 0 {
		i--
	}
	return sorted[i].Round(10 * time.Microsecond)
}
