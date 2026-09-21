// Package blevestore is the bundled lexical search backend: one Bleve index
// per (tenant, collection) on local disk.
//
// **One analyzer, pinned.** Every searched field is analysed the same way at
// index time and at query time: fold diacritics, split on anything that is not
// a letter or a digit, lowercase. No stemming and no stop words, so the
// behaviour is the same in every language and an identifier is never mangled.
// The analyzer's name is its version: a collection records it, and a change
// means re-indexing the collection.
//
// **An identifier is a phrase of its parts.** `TXC-4821` indexes as the tokens
// `txc` `4821`, and is queried as that phrase. The same rule covers
// `matt@example.com`, `foo.bar.baz`, `2026-09-21` and `github.com/foo/bar`.
// Nothing about this leaks into the stack-facing API.
package blevestore

import (
	"unicode"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/char/asciifolding"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/character"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/registry"
	index "github.com/blevesearch/bleve_index_api"
)

const (
	// AnalyzerVersion names the analyzer and pins its behaviour. Change the
	// analysis and this name together.
	AnalyzerVersion = "txco_v1"

	// ScoringModel is pinned beside the analyzer. Bleve's own default is
	// tf-idf; BM25 is opt-in through the index mapping.
	ScoringModel = index.BM25Scoring

	tokenizerName = "txco_alnum"
)

func init() {
	registry.RegisterTokenizer(tokenizerName, func(map[string]interface{}, *registry.Cache) (analysis.Tokenizer, error) {
		return character.NewCharacterTokenizer(isTokenRune), nil
	})
}

// isTokenRune keeps letters and digits of any script. Everything else, from
// whitespace to `-`, `_`, `.`, `@` and `/`, separates tokens.
func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

// newIndexMapping builds the mapping every collection index is created with.
// Documents are built field by field (document.go), never through Bleve's
// dynamic mapping, so the mapping's job here is the analyzer registry, the
// default analyzer that query-time analysis resolves to, and the scoring model.
func newIndexMapping() (*mapping.IndexMappingImpl, error) {
	im := mapping.NewIndexMapping()
	if err := im.AddCustomAnalyzer(AnalyzerVersion, map[string]interface{}{
		"type":          custom.Name,
		"char_filters":  []string{asciifolding.Name},
		"tokenizer":     tokenizerName,
		"token_filters": []string{lowercase.Name},
	}); err != nil {
		return nil, err
	}
	im.DefaultAnalyzer = AnalyzerVersion
	im.ScoringModel = ScoringModel
	// Nothing is indexed through the mapping, so keep it from guessing.
	im.DefaultMapping.Dynamic = false
	im.IndexDynamic = false
	im.StoreDynamic = false
	im.DocValuesDynamic = false
	return im, nil
}

// tokens runs text through the pinned analyzer and returns the terms in order.
func tokens(an analysis.Analyzer, text string) []string {
	ts := an.Analyze([]byte(text))
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, string(t.Term))
	}
	return out
}
