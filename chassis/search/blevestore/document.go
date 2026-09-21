package blevestore

import (
	"encoding/json"
	"fmt"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/document"
	index "github.com/blevesearch/bleve_index_api"

	"github.com/loremlabs/thanks-computer/chassis/search"
)

// Field names inside a collection index. Searched fields carry the stack's own
// names. Metadata lives under metaPrefix so an author's key can never collide
// with a searched field, and srcField holds the raw record.
const (
	fieldAll      = "all"
	fieldTitle    = "title"
	fieldHeading  = "heading"
	fieldName     = "name"
	fieldEntities = "entities"
	metaPrefix    = "m."
	srcField      = "_src"

	// maxMetadataKeys bounds the distinct fields one record can add to an index.
	maxMetadataKeys = 64
)

const (
	// Searched fields need positions for phrase queries, and nothing else.
	searchedOpts = index.IndexField | index.IncludeTermVectors
	// Metadata is matched exactly and never scored.
	metaTextOpts = index.IndexField | index.SkipFreqNorm
	metaOpts     = index.IndexField
)

// buildDocument turns an Item into a Bleve document, field by field. Building
// it by hand, not through Bleve's dynamic mapping, keeps two surprises out: a
// metadata string that looks like a date is not silently indexed as a datetime,
// and every metadata string is matched whole.
//
// The raw record is stored in srcField. A hit returns its text and metadata
// from there, a metadata update re-indexes from there, and an analyzer upgrade
// can re-index a collection from there without the source documents.
func buildDocument(it search.Item, an analysis.Analyzer) (*document.Document, error) {
	if it.ID == "" {
		return nil, fmt.Errorf("item id required")
	}
	if len(it.Metadata) > maxMetadataKeys {
		return nil, fmt.Errorf("item %q: %d metadata keys (max %d)", it.ID, len(it.Metadata), maxMetadataKeys)
	}
	doc := document.NewDocument(it.ID)

	// Every searched value is indexed twice. Once into fieldAll, where terms
	// are matched: one field means one set of term statistics, so a common
	// word stays common. (Scored per field, "we" or "use" is rare among
	// headings and would outrank a truly rare word in the body.) And once
	// under its own name, where only phrases are matched, so a filename or a
	// title found intact can lift a record.
	//
	// Each value is its own array element of fieldAll. Bleve matches a phrase
	// inside one element only, so a phrase never spans from a title into the
	// text after it.
	var all uint64
	addText := func(name string, pos []uint64, val string) {
		if val == "" {
			return
		}
		if name != "" {
			doc.AddField(document.NewTextFieldCustom(name, pos, []byte(val), searchedOpts, an))
		}
		doc.AddField(document.NewTextFieldCustom(fieldAll, []uint64{all}, []byte(val), searchedOpts, an))
		all++
	}
	addText("", nil, it.Text)
	addText(fieldTitle, nil, it.Title)
	addText(fieldHeading, nil, it.Heading)
	addText(fieldName, nil, it.Name)
	for i, e := range it.Entities {
		addText(fieldEntities, []uint64{uint64(i)}, e)
	}

	for k, v := range it.Metadata {
		addMeta(doc, metaPrefix+k, nil, v)
	}

	src, err := json.Marshal(it)
	if err != nil {
		return nil, fmt.Errorf("item %q: %w", it.ID, err)
	}
	doc.AddField(document.NewTextFieldCustom(srcField, nil, src, index.StoreField, nil))
	return doc, nil
}

// addMeta indexes one metadata value under name. A string is one exact term, a
// number is numeric, a bool is boolean, and a list indexes each scalar element,
// so `eq` matches any of them. Nested objects and nulls are kept in srcField
// but are not filterable.
func addMeta(doc *document.Document, name string, pos []uint64, v any) {
	switch x := v.(type) {
	case string:
		// A nil analyzer indexes the whole value as a single term.
		doc.AddField(document.NewTextFieldCustom(name, pos, []byte(x), metaTextOpts, nil))
	case bool:
		doc.AddField(document.NewBooleanFieldWithIndexingOptions(name, pos, x, metaOpts))
	case float64:
		doc.AddField(document.NewNumericFieldWithIndexingOptions(name, pos, x, metaOpts))
	case float32:
		doc.AddField(document.NewNumericFieldWithIndexingOptions(name, pos, float64(x), metaOpts))
	case int:
		doc.AddField(document.NewNumericFieldWithIndexingOptions(name, pos, float64(x), metaOpts))
	case int64:
		doc.AddField(document.NewNumericFieldWithIndexingOptions(name, pos, float64(x), metaOpts))
	case json.Number:
		if f, err := x.Float64(); err == nil {
			doc.AddField(document.NewNumericFieldWithIndexingOptions(name, pos, f, metaOpts))
		}
	case []any:
		if pos != nil {
			return // one level of list only
		}
		for i, e := range x {
			addMeta(doc, name, []uint64{uint64(i)}, e)
		}
	case []string:
		for i, e := range x {
			addMeta(doc, name, []uint64{uint64(i)}, e)
		}
	}
}
