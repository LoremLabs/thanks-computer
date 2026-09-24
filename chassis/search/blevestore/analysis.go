// Package blevestore is the bundled lexical search backend: one Bleve index
// per (tenant, collection) on local disk.
//
// **One analyzer, pinned.** Every searched field is analysed the same way at
// index time and at query time: fold diacritics, split on anything that is not
// a letter or a digit, lowercase. No stemming and no stop words, so an
// identifier is never mangled. The analyzer's name is its version: a
// collection records it, and a change means re-indexing the collection.
//
// **A script without spaces is read in pairs.** Splitting on non-letters says
// nothing in Chinese or Japanese, where a whole clause is one unbroken run of
// letters and would become one token that no query ever asks for — measured on
// production, a Chinese sentence matched nothing while a two-character term
// matched, because that term happened to BE a whole run (2026-09-24). So a run
// of Han or kana is indexed as its overlapping character pairs: 人工智能 ->
// 人工, 工智, 智能. A query is cut the same way, so any part of a sentence
// finds it. Korean is deliberately not in that set: Hangul is written with
// spaces between words, so the rule above already fits it.
//
// Everything written with spaces is untouched by this — the Latin token
// stream is byte-identical to txco_v1, which is what the tokenizer tests
// assert.
//
// **An identifier is a phrase of its parts.** `TXC-4821` indexes as the tokens
// `txc` `4821`, and is queried as that phrase. The same rule covers
// `matt@example.com`, `foo.bar.baz`, `2026-09-21` and `github.com/foo/bar`.
// Nothing about this leaks into the stack-facing API.
package blevestore

import (
	"bytes"
	"unicode"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/char/asciifolding"
	"github.com/blevesearch/bleve/v2/analysis/lang/cjk"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/character"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/blevesearch/bleve/v2/registry"
	index "github.com/blevesearch/bleve_index_api"
)

const (
	// AnalyzerVersion names the analyzer and pins its behaviour. Change the
	// analysis and this name together — ALWAYS. Index-time analysis uses the
	// analyzer built in this process, query-time uses the one persisted inside
	// the index, so a chain that changes under an unchanged name writes terms
	// one way and looks them up another, with no error anywhere. The name is
	// also what makes an existing collection refuse to open (collection.go),
	// which is the signal to re-index it.
	//
	// txco_v2 (2026-09-24) added CJK pair indexing; txco_v1 was the same
	// chain without it.
	AnalyzerVersion = "txco_v2"

	// ScoringModel is pinned beside the analyzer. Bleve's own default is
	// tf-idf; BM25 is opt-in through the index mapping.
	ScoringModel = index.BM25Scoring

	tokenizerName = "txco_alnum"

	// markFilterName types the CJK parts of a token as ideographic, which is
	// what Bleve's cjk_bigram acts on. Without it that filter is a silent
	// no-op here: it only touches tokens typed Ideographic, and the character
	// tokenizer types everything it emits AlphaNumeric.
	markFilterName = "txco_cjk_mark"
)

func init() {
	registry.RegisterTokenizer(tokenizerName, func(map[string]interface{}, *registry.Cache) (analysis.Tokenizer, error) {
		return character.NewCharacterTokenizer(isTokenRune), nil
	})
	registry.RegisterTokenFilter(markFilterName, func(map[string]interface{}, *registry.Cache) (analysis.TokenFilter, error) {
		return cjkMarkFilter{}, nil
	})
}

// cjkMarkFilter splits a token where its script changes and types the Han and
// kana parts Ideographic, leaving every other part exactly as it was. Two jobs
// in one pass:
//
//   - the split, because the tokenizer keeps letters and digits together and
//     so hands over `中国2026` as one token, whose halves belong to different
//     alphabets and must not be paired across the seam;
//   - the type, because that is the only thing cjk_bigram looks at.
//
// Hangul is not CJK for this purpose: Korean puts spaces between its words, so
// the tokenizer already splits it the way it is written.
type cjkMarkFilter struct{}

func isIdeographicRune(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana)
}

func (cjkMarkFilter) Filter(input analysis.TokenStream) analysis.TokenStream {
	out := make(analysis.TokenStream, 0, len(input))
	pos := 1
	emit := func(src *analysis.Token, term []byte, start int, ideographic bool) {
		t := &analysis.Token{
			Term:     term,
			Start:    start,
			End:      start + len(term),
			Position: pos,
			Type:     src.Type,
			KeyWord:  src.KeyWord,
		}
		if ideographic {
			t.Type = analysis.Ideographic
		}
		pos++
		out = append(out, t)
	}
	for _, tok := range input {
		// The common case, and the one that must stay free: no CJK at all.
		if bytes.IndexFunc(tok.Term, isIdeographicRune) < 0 {
			tok.Position = pos
			pos++
			out = append(out, tok)
			continue
		}
		runStart, runIdeo := 0, false
		for i := 0; i < len(tok.Term); {
			r, size := utf8.DecodeRune(tok.Term[i:])
			ideo := isIdeographicRune(r)
			if i == 0 {
				runIdeo = ideo
			} else if ideo != runIdeo {
				emit(tok, tok.Term[runStart:i], tok.Start+runStart, runIdeo)
				runStart, runIdeo = i, ideo
			}
			i += size
		}
		emit(tok, tok.Term[runStart:], tok.Start+runStart, runIdeo)
	}
	return out
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
	// cjk_width folds full-width forms onto their ordinary ones first (a
	// full-width "Ａ" is the letter A, a half-width katakana is the kana), so
	// the rest of the chain sees one spelling. cjk_bigram runs LAST, on the
	// types the mark filter has just set; output_unigram is off because it
	// already emits a lone character where a run is one character long.
	if err := im.AddCustomAnalyzer(AnalyzerVersion, map[string]interface{}{
		"type":          custom.Name,
		"char_filters":  []string{asciifolding.Name},
		"tokenizer":     tokenizerName,
		"token_filters": []string{cjk.WidthName, lowercase.Name, markFilterName, cjk.BigramName},
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
