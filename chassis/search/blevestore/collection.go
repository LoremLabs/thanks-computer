package blevestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"

	"github.com/loremlabs/thanks-computer/chassis/search"
)

// Internal keys stored inside a collection index. They live in the index's own
// snapshot, so a copy of the index carries them with it.
var (
	keyAnalyzer = []byte("txco.analyzer_version")
	keyScoring  = []byte("txco.scoring_model")
)

// defaultLimit matches the vector store's default when a query gives none.
const defaultLimit = 10

// Collection is one open Bleve index: the physical form of one
// (tenant, collection). It is safe for concurrent use.
type Collection struct {
	idx bleve.Index
	an  analysis.Analyzer
}

// OpenCollection opens the index at dir, creating it when dir does not exist.
// An index written under another analyzer or scoring model is refused: its
// terms would not match this build's queries.
func OpenCollection(dir string) (*Collection, error) {
	im, err := newIndexMapping()
	if err != nil {
		return nil, err
	}
	an := im.AnalyzerNamed(AnalyzerVersion)
	if an == nil {
		return nil, fmt.Errorf("blevestore: analyzer %q did not register", AnalyzerVersion)
	}

	if _, serr := os.Stat(dir); errors.Is(serr, os.ErrNotExist) {
		idx, err := bleve.New(dir, im)
		if err != nil {
			return nil, fmt.Errorf("blevestore: create %s: %w", dir, err)
		}
		b := idx.NewBatch()
		b.SetInternal(keyAnalyzer, []byte(AnalyzerVersion))
		b.SetInternal(keyScoring, []byte(ScoringModel))
		if err := idx.Batch(b); err != nil {
			_ = idx.Close()
			return nil, fmt.Errorf("blevestore: stamp %s: %w", dir, err)
		}
		return &Collection{idx: idx, an: an}, nil
	}

	idx, err := bleve.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("blevestore: open %s: %w", dir, err)
	}
	for _, pin := range []struct {
		key  []byte
		want string
	}{{keyAnalyzer, AnalyzerVersion}, {keyScoring, ScoringModel}} {
		got, err := idx.GetInternal(pin.key)
		if err != nil || string(got) != pin.want {
			_ = idx.Close()
			return nil, fmt.Errorf("blevestore: %s: %s is %q, this build needs %q", dir, pin.key, got, pin.want)
		}
	}
	return &Collection{idx: idx, an: an}, nil
}

// Upsert indexes items as one batch: a new id inserts, an existing id replaces.
// internal is written in the same batch, so a caller can record its own cursor
// atomically with the records it covers.
func (c *Collection) Upsert(items []search.Item, internal map[string][]byte) error {
	b := c.idx.NewBatch()
	for _, it := range items {
		doc, err := buildDocument(it, c.an)
		if err != nil {
			return err
		}
		if err := b.IndexAdvanced(doc); err != nil {
			return err
		}
	}
	for k, v := range internal {
		b.SetInternal([]byte(k), v)
	}
	return c.idx.Batch(b)
}

// Query returns the best hits for q among the records f admits, best first.
func (c *Collection) Query(q string, limit int, f search.Filter) ([]search.Hit, error) {
	bq, err := buildQuery(c.an, q, f)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	req := bleve.NewSearchRequestOptions(bq, limit, 0, false)
	req.Fields = []string{srcField}
	// Ties break on id, so equal scores come back in a stable order.
	req.SortBy([]string{"-_score", "_id"})
	res, err := c.idx.Search(req)
	if err != nil {
		return nil, err
	}
	hits := make([]search.Hit, 0, len(res.Hits))
	for i, h := range res.Hits {
		hit := search.Hit{ID: h.ID, Rank: i + 1}
		if src, ok := h.Fields[srcField].(string); ok {
			var it search.Item
			if err := json.Unmarshal([]byte(src), &it); err == nil {
				hit.Text, hit.Metadata = it.Text, it.Metadata
			}
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

// Count is the number of records in the collection.
func (c *Collection) Count() (uint64, error) { return c.idx.DocCount() }

// Internal reads a value a batch stored with Upsert's internal map.
func (c *Collection) Internal(key string) ([]byte, error) { return c.idx.GetInternal([]byte(key)) }

// Index exposes the underlying Bleve index for the snapshot path.
func (c *Collection) Index() bleve.Index { return c.idx }

// Close flushes and releases the index. A Bleve index must be closed.
func (c *Collection) Close() error { return c.idx.Close() }
