package blevestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/loremlabs/thanks-computer/chassis/search"
)

// Internal keys stored inside a collection index. They live in the index's own
// snapshot, so a copy of the index carries them with it.
var (
	keyAnalyzer = []byte("txco.analyzer_version")
	keyScoring  = []byte("txco.scoring_model")
)

const (
	// idPage is how many matching ids one search page collects.
	idPage = 1000
	// rewritePage is how many records one update batch re-indexes.
	rewritePage = 200
)

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
		key         []byte
		field, want string
	}{{keyAnalyzer, "analyzer_version", AnalyzerVersion}, {keyScoring, "scoring_model", ScoringModel}} {
		got, err := idx.GetInternal(pin.key)
		if err != nil || string(got) != pin.want {
			_ = idx.Close()
			return nil, &search.AnalyzerMismatchError{Collection: dir, Field: pin.field, Existing: string(got), Requested: pin.want}
		}
	}
	return &Collection{idx: idx, an: an}, nil
}

// Apply performs one mutation. It is the only write path: every Store method,
// and later a sequenced replay, comes through here, so a write means the same
// thing however it arrived.
//
// internal is written in the SAME batch as the mutation's last change, so a
// caller can record its own cursor atomically with the records it covers. A
// delete or an update by filter is evaluated here, against the index as it
// stands now. Both are safe to apply twice.
//
// The count is the records written (upsert), removed (delete) or matched
// (update).
func (c *Collection) Apply(ctx context.Context, m search.Mutation, internal map[string][]byte) (int, error) {
	switch m.Op {
	case search.OpUpsert:
		if err := search.ValidateItems(m.Items); err != nil {
			return 0, err
		}
		b := c.idx.NewBatch()
		for _, it := range m.Items {
			doc, err := buildDocument(it, c.an)
			if err != nil {
				return 0, err
			}
			if err := b.IndexAdvanced(doc); err != nil {
				return 0, err
			}
		}
		return len(m.Items), c.commit(b, internal)

	case search.OpDelete:
		if err := search.ValidateSelector(m.Select); err != nil {
			return 0, err
		}
		var q query.Query = query.NewDocIDQuery(m.Select.IDs)
		if len(m.Select.IDs) == 0 {
			fq, err := buildFilterQuery(m.Select.Filter)
			if err != nil {
				return 0, err
			}
			q = fq
		}
		ids, err := c.matchingIDs(ctx, q)
		if err != nil {
			return 0, err
		}
		b := c.idx.NewBatch()
		for _, id := range ids {
			b.Delete(id)
		}
		return len(ids), c.commit(b, internal)

	case search.OpUpdate:
		if err := search.ValidateUpdate(m.Filter, m.Change); err != nil {
			return 0, err
		}
		fq, err := buildFilterQuery(m.Filter)
		if err != nil {
			return 0, err
		}
		// Collect first, rewrite second: a merge can change the very field the
		// filter reads, which would shift a paged result set under the loop.
		ids, err := c.matchingIDs(ctx, fq)
		if err != nil {
			return 0, err
		}
		for i := 0; i < len(ids); i += rewritePage {
			page := ids[i:min(i+rewritePage, len(ids))]
			b, err := c.rewrite(ctx, page, m.Change)
			if err != nil {
				return 0, err
			}
			var in map[string][]byte
			if i+rewritePage >= len(ids) {
				in = internal // the cursor rides the last batch only
			}
			if err := c.commit(b, in); err != nil {
				return 0, err
			}
		}
		if len(ids) == 0 {
			return 0, c.commit(c.idx.NewBatch(), internal)
		}
		return len(ids), nil
	}
	return 0, &search.InvalidArgError{Reason: fmt.Sprintf("a collection cannot apply op %q", m.Op)}
}

// commit writes a batch. An empty batch with nothing internal is skipped.
func (c *Collection) commit(b *bleve.Batch, internal map[string][]byte) error {
	for k, v := range internal {
		b.SetInternal([]byte(k), v)
	}
	if b.Size() == 0 && len(internal) == 0 {
		return nil
	}
	return c.idx.Batch(b)
}

// matchingIDs returns the id of every record q matches, in id order. It pages
// with search-after, so a large match never asks the engine for a deep offset.
func (c *Collection) matchingIDs(ctx context.Context, q query.Query) ([]string, error) {
	var ids []string
	var after []string
	for {
		req := bleve.NewSearchRequestOptions(q, idPage, 0, false)
		req.SortBy([]string{"_id"})
		if after != nil {
			req.SetSearchAfter(after)
		}
		res, err := c.idx.SearchInContext(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, h := range res.Hits {
			ids = append(ids, h.ID)
		}
		if len(res.Hits) < idPage {
			return ids, nil
		}
		after = []string{res.Hits[len(res.Hits)-1].ID}
	}
}

// rewrite builds the batch that re-indexes ids with ch applied. Bleve has no
// partial update, so each record is rebuilt whole from its stored source.
func (c *Collection) rewrite(ctx context.Context, ids []string, ch search.Change) (*bleve.Batch, error) {
	req := bleve.NewSearchRequestOptions(query.NewDocIDQuery(ids), len(ids), 0, false)
	req.Fields = []string{srcField}
	res, err := c.idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, err
	}
	b := c.idx.NewBatch()
	for _, h := range res.Hits {
		it, ok := sourceOf(h.Fields)
		if !ok {
			return nil, fmt.Errorf("blevestore: record %q has no stored source", h.ID)
		}
		for k, v := range ch.Merge {
			if v == nil {
				delete(it.Metadata, k)
				continue
			}
			if it.Metadata == nil {
				it.Metadata = map[string]any{}
			}
			it.Metadata[k] = v
		}
		if s := ch.Fields; s != nil {
			if s.Name != nil {
				it.Name = *s.Name
			}
			if s.Title != nil {
				it.Title = *s.Title
			}
			if s.Heading != nil {
				it.Heading = *s.Heading
			}
		}
		doc, err := buildDocument(it, c.an)
		if err != nil {
			return nil, err
		}
		if err := b.IndexAdvanced(doc); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Upsert indexes items as one batch. It is Apply with an upsert.
func (c *Collection) Upsert(items []search.Item, internal map[string][]byte) error {
	_, err := c.Apply(context.Background(), search.Mutation{Op: search.OpUpsert, Items: items}, internal)
	return err
}

// Query returns the best hits for q among the records f admits, best first.
func (c *Collection) Query(ctx context.Context, q string, limit int, f search.Filter) ([]search.Hit, error) {
	limit, err := search.ValidateQuery(q, limit)
	if err != nil {
		return nil, err
	}
	bq, err := buildQuery(c.an, q, f)
	if err != nil {
		return nil, err
	}
	req := bleve.NewSearchRequestOptions(bq, limit, 0, false)
	req.Fields = []string{srcField}
	// Ties break on id, so equal scores come back in a stable order.
	req.SortBy([]string{"-_score", "_id"})
	res, err := c.idx.SearchInContext(ctx, req)
	if err != nil {
		return nil, err
	}
	hits := make([]search.Hit, 0, len(res.Hits))
	for i, h := range res.Hits {
		hit := search.Hit{ID: h.ID, Rank: i + 1}
		if it, ok := sourceOf(h.Fields); ok {
			hit.Text, hit.Metadata = it.Text, it.Metadata
		}
		hits = append(hits, hit)
	}
	return hits, nil
}

func sourceOf(fields map[string]interface{}) (search.Item, bool) {
	var it search.Item
	src, ok := fields[srcField].(string)
	if !ok {
		return it, false
	}
	return it, json.Unmarshal([]byte(src), &it) == nil
}

// Count is the number of records in the collection.
func (c *Collection) Count() (uint64, error) { return c.idx.DocCount() }

// Internal reads a value a batch stored through Apply's internal map.
func (c *Collection) Internal(key string) ([]byte, error) { return c.idx.GetInternal([]byte(key)) }

// Index exposes the underlying Bleve index for the snapshot path.
func (c *Collection) Index() bleve.Index { return c.idx }

// Close flushes and releases the index. A Bleve index must be closed.
func (c *Collection) Close() error { return c.idx.Close() }
