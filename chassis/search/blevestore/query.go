package blevestore

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/blevesearch/bleve/v2/analysis"
	"github.com/blevesearch/bleve/v2/search/query"

	"github.com/loremlabs/thanks-computer/chassis/search"
	"github.com/loremlabs/thanks-computer/chassis/vector"
)

// maxQueryTerms caps the analysed terms one query contributes. A whole
// natural-language message is a legitimate query, so the cap is generous, and
// past it the tail is dropped, never an error.
const maxQueryTerms = 64

// Terms are matched in fieldAll only (document.go says why). A phrase is
// matched there too, and again in each short field, where finding it intact
// means more: the record is the file of that name, or the section of that
// title. The weights are ranking policy, not API contract.
var phraseFields = []struct {
	name  string
	boost float64
}{
	{fieldAll, 1.0},
	{fieldTitle, 2.0},
	{fieldHeading, 1.5},
	{fieldName, 2.0},
	{fieldEntities, 1.5},
}

const phraseBoost = 3.0

// parsedQuery is a query string reduced to what the engine is asked for: the
// terms to OR, and the phrases that should lift a record holding them intact.
type parsedQuery struct {
	terms   []string
	phrases [][]string
}

// parseQuery applies the v1 query semantics:
//
//   - a quoted string is a phrase, when the quotes are a closed pair and the
//     run is short enough to be one (maxPhraseTerms);
//   - a word made of several parts (`TXC-4821`, `matt@example.com`,
//     `foo.bar.baz`) is an identifier, and is a phrase of its parts;
//   - every term, from phrases too, is OR-ed, so a whole message can be passed
//     as the query and the best records still rise.
//
// No engine syntax is recognised: `+`, `-`, `field:` and `*` are just
// separators.
func parseQuery(an analysis.Analyzer, q string) parsedQuery {
	var pq parsedQuery
	room := func() int { return maxQueryTerms - len(pq.terms) }
	add := func(toks []string, phrase bool) {
		if len(toks) > room() {
			toks = toks[:room()]
		}
		pq.terms = append(pq.terms, toks...)
		if phrase && len(toks) >= 2 {
			pq.phrases = append(pq.phrases, toks)
		}
	}

	for _, seg := range splitQuoted(q) {
		if room() == 0 {
			break
		}
		if seg.quoted {
			if toks := tokens(an, seg.text); len(toks) <= maxPhraseTerms {
				add(toks, true)
				continue
			}
			// Too long to be a phrase: read it as the plain words it is.
		}
		for _, word := range strings.Fields(seg.text) {
			if room() == 0 {
				break
			}
			add(tokens(an, word), isIdentifier(word))
		}
	}
	return pq
}

type segment struct {
	text   string
	quoted bool
}

// maxPhraseTerms is the longest quoted run that is still a phrase. People
// quote a clause or a title; a quoted paragraph is a pasted passage, and its
// words are worth more OR-ed than as one phrase nothing will match.
const maxPhraseTerms = 16

// splitQuoted cuts q into quoted and unquoted runs. Only a CLOSED pair of
// double quotes (straight, or curly open then close) delimits a quoted run. A
// quote with no partner is just punctuation: real messages are full of them
// (an inch mark, a bad paste, a mis-decoded dash), and one stray mark must not
// turn the rest of a message into a phrase.
func splitQuoted(q string) []segment {
	var out []segment
	rs := []rune(q)
	plain := func(from, to int) {
		if to > from {
			out = append(out, segment{text: string(rs[from:to])})
		}
	}
	isOpen := func(r rune) bool { return r == '"' || r == '\u201c' }
	isClose := func(r rune) bool { return r == '"' || r == '\u201d' }
	start := 0
	for i := 0; i < len(rs); i++ {
		if !isOpen(rs[i]) {
			continue
		}
		end := -1
		for j := i + 1; j < len(rs); j++ {
			if isClose(rs[j]) {
				end = j
				break
			}
		}
		if end < 0 {
			break // no partner: this and any later quote are plain text
		}
		plain(start, i)
		if end > i+1 {
			out = append(out, segment{text: string(rs[i+1 : end]), quoted: true})
		}
		start, i = end+1, end
	}
	plain(start, len(rs))
	return out
}

// isIdentifier reports whether a whitespace-delimited word is several token
// runs joined by something other than an apostrophe. `TXC-4821` and
// `well-known` are; `don't` and `(hello),` are not.
func isIdentifier(word string) bool {
	word = strings.TrimFunc(word, func(r rune) bool { return !isTokenRune(r) })
	joined := false
	for _, r := range word {
		if isTokenRune(r) || unicode.Is(unicode.Mn, r) {
			continue
		}
		if r == '\'' || r == '’' || r == 'ʼ' {
			continue
		}
		joined = true
	}
	return joined
}

// buildQuery turns a query string and a filter into one Bleve query. The text
// clauses score; the filter clause only admits or rejects, so metadata never
// moves the ranking.
func buildQuery(an analysis.Analyzer, q string, f search.Filter) (query.Query, error) {
	pq := parseQuery(an, q)
	if len(pq.terms) == 0 {
		return query.NewMatchNoneQuery(), nil
	}
	mq := query.NewMatchQuery(strings.Join(pq.terms, " "))
	mq.Analyzer = AnalyzerVersion
	mq.SetField(fieldAll)
	should := []query.Query{mq}
	for _, p := range pq.phrases {
		for _, pf := range phraseFields {
			pp := query.NewMatchPhraseQuery(strings.Join(p, " "))
			pp.Analyzer = AnalyzerVersion
			pp.SetField(pf.name)
			pp.SetBoost(pf.boost * phraseBoost)
			should = append(should, pp)
		}
	}
	bq := query.NewBooleanQuery(nil, should, nil)
	fq, err := buildFilter(f)
	if err != nil {
		return nil, err
	}
	bq.AddFilter(fq)
	return bq, nil
}

// buildFilterQuery matches every record the filter admits, with no text
// clause. Delete-by-filter and update-by-filter use it.
func buildFilterQuery(f search.Filter) (query.Query, error) {
	fq, err := buildFilter(f)
	if err != nil {
		return nil, err
	}
	if fq == nil {
		return query.NewMatchAllQuery(), nil
	}
	return fq, nil
}

// buildFilter translates the vector store's filter grammar. It returns nil for
// an empty filter. Conditions are AND-ed. `not_in` becomes a must-not clause,
// which a record lacking the field passes, as it does in the vector store.
func buildFilter(f search.Filter) (query.Query, error) {
	if len(f.Conditions) == 0 {
		return nil, nil
	}
	var must, mustNot []query.Query
	for _, c := range f.Conditions {
		if c.Field == "" || strings.ContainsRune(c.Field, 0) {
			return nil, &search.InvalidArgError{Reason: "filter field name required"}
		}
		switch c.Op {
		case vector.OpEq:
			q, err := eqQuery(c.Field, c.Value)
			if err != nil {
				return nil, err
			}
			must = append(must, q)
		case vector.OpIn, vector.OpNotIn:
			var any []query.Query
			for _, v := range asSlice(c.Value) {
				q, err := eqQuery(c.Field, v)
				if err != nil {
					return nil, err
				}
				any = append(any, q)
			}
			switch {
			case c.Op == vector.OpIn && len(any) == 0:
				must = append(must, query.NewMatchNoneQuery()) // IN () matches nothing
			case c.Op == vector.OpIn:
				must = append(must, query.NewDisjunctionQuery(any))
			default:
				mustNot = append(mustNot, any...) // NOT IN () excludes nothing
			}
		case vector.OpGte, vector.OpLte, vector.OpGt, vector.OpLt:
			q, err := rangeQuery(c.Field, c.Op, c.Value)
			if err != nil {
				return nil, err
			}
			must = append(must, q)
		default:
			return nil, &search.InvalidArgError{Reason: fmt.Sprintf("unsupported filter op %q", c.Op)}
		}
	}
	if len(must) == 0 && len(mustNot) == 0 {
		// Only `not_in []` conditions: nothing is excluded. An all-empty
		// boolean query would match nothing, so say "no filter" instead.
		return nil, nil
	}
	return query.NewBooleanQuery(must, nil, mustNot), nil
}

// eqQuery matches one exact value. The value's type picks the field type, so
// the string "3" never equals the number 3, as in the vector store.
func eqQuery(field string, v any) (query.Query, error) {
	if field == "id" {
		s, ok := v.(string)
		if !ok {
			return nil, &search.InvalidArgError{Reason: "filter on `id` takes strings"}
		}
		return query.NewDocIDQuery([]string{s}), nil
	}
	name := metaPrefix + field
	switch x := v.(type) {
	case string:
		q := query.NewTermQuery(x)
		q.SetField(name)
		return q, nil
	case bool:
		q := query.NewBoolFieldQuery(x)
		q.SetField(name)
		return q, nil
	}
	if n, ok := asFloat(v); ok {
		incl := true
		q := query.NewNumericRangeInclusiveQuery(&n, &n, &incl, &incl)
		q.SetField(name)
		return q, nil
	}
	return nil, &search.InvalidArgError{Reason: fmt.Sprintf("filter on %q: unsupported value type %T", field, v)}
}

func rangeQuery(field string, op vector.Op, v any) (query.Query, error) {
	if field == "id" {
		return nil, &search.InvalidArgError{Reason: "filter on `id` supports eq, in and not_in"}
	}
	name := metaPrefix + field
	incl := op == vector.OpGte || op == vector.OpLte
	lower := op == vector.OpGte || op == vector.OpGt
	if n, ok := asFloat(v); ok {
		var q *query.NumericRangeQuery
		if lower {
			q = query.NewNumericRangeInclusiveQuery(&n, nil, &incl, nil)
		} else {
			q = query.NewNumericRangeInclusiveQuery(nil, &n, nil, &incl)
		}
		q.SetField(name)
		return q, nil
	}
	if s, ok := v.(string); ok && s != "" {
		var q *query.TermRangeQuery
		if lower {
			q = query.NewTermRangeInclusiveQuery(s, "", &incl, nil)
		} else {
			q = query.NewTermRangeInclusiveQuery("", s, nil, &incl)
		}
		q.SetField(name)
		return q, nil
	}
	return nil, &search.InvalidArgError{Reason: fmt.Sprintf("filter on %q: %s takes a number or a non-empty string", field, op)}
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func asSlice(v any) []any {
	switch s := v.(type) {
	case []any:
		return s
	case []string:
		out := make([]any, len(s))
		for i, e := range s {
			out[i] = e
		}
		return out
	case nil:
		return nil
	}
	return []any{v}
}
