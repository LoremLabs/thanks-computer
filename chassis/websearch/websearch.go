// Package websearch is the contract behind txco://web/search: the request
// a rule may make, the answer it gets back, and the seam a search
// provider implements. It does no networking. The handler in
// chassis/server fetches through chassis/fetch; a provider
// (websearch/brave) only builds the request and reads the body.
//
// A search is one question to a public search engine, under the tenant's
// own key, answered with titles, addresses and snippets, never the
// provider's raw body. Results are external text: the stack that reads
// them decides what they may do.
package websearch

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// The contract's bounds. Brave allows 600 characters and 75 words; the
// contract fits inside them.
const (
	MaxQueryChars = 500
	MaxQueryWords = 75
	MaxCount      = 20
	DefaultCount  = 10

	MaxTitleBytes       = 1 << 10
	MaxDescriptionBytes = 4 << 10
	MaxURLBytes         = 2 << 10
	MaxAnswerBytes      = 128 << 10
)

// DefaultInto is where the answer lands when a rule names no `into`.
const DefaultInto = "_websearch"

// Request is one validated search.
type Request struct {
	Query string
	Count int
	// Country is two upper-case letters, ALL, or "" for the provider's
	// default. Language is a lower-case ISO 639-1 code, or "" for the
	// provider's default. The provider refuses one it lacks.
	Country    string
	Language   string
	Freshness  string // any, day, week, month, year
	SafeSearch string // off, moderate, strict
}

// Result is one hit, in the provider's order.
type Result struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	// PageDate is the page's date as the provider reports it, RFC 3339 in
	// UTC, or null. Brave calls it page_age: "published or last modified".
	PageDate *string `json:"page_date"`
	// Source is the URL's host, lower case.
	Source string `json:"source"`
}

// Answer is the op's result object on success.
type Answer struct {
	OK        bool     `json:"ok"`
	Query     string   `json:"query"`
	Provider  string   `json:"provider"`
	Results   []Result `json:"results"`
	Count     int      `json:"count"`
	Truncated bool     `json:"truncated"`
}

// Failure is the op's result object when the search did not happen or
// did not answer. The provider's body is never in it.
type Failure struct {
	OK       bool   `json:"ok"`
	Query    string `json:"query,omitempty"`
	Provider string `json:"provider,omitempty"`
	Error    *Error `json:"error"`
}

// opKeys are the request's fields. chassisKeys are WITH keys the chassis
// reads for every op; a rule may set them beside the request's, and they
// are not the op's to refuse. Any other key is a mistake, and refused, so
// a typo such as `freshnes` fails instead of being ignored.
var (
	opKeys = map[string]bool{
		"query": true, "count": true, "country": true, "language": true,
		"freshness": true, "safesearch": true,
	}
	chassisKeys = map[string]bool{
		"into": true, "timeout": true, "mode": true, "redact": true, "omit": true,
		"secrets": true, "grant": true, "debug": true, "comment": true, "note": true,
		"stream": true, "max_duration": true, "continue_after": true,
		"checkpoint": true, "join_at_scope": true,
	}
	freshnessValues  = map[string]bool{"any": true, "day": true, "week": true, "month": true, "year": true}
	safeSearchValues = map[string]bool{"off": true, "moderate": true, "strict": true}
	countryShape     = regexp.MustCompile(`^[A-Za-z]{2}$`)
	languageShape    = regexp.MustCompile(`^[A-Za-z]{2}$`)
)

// Parse reads and checks the op's WITH object. An *Error names the first
// field at fault; it is the whole of what a rule author sees.
func Parse(meta []byte) (Request, *Error) {
	req := Request{Count: DefaultCount, Freshness: "any", SafeSearch: "moderate"}
	if len(meta) > 0 && !gjson.ValidBytes(meta) {
		return req, invalid("the request does not parse")
	}
	var bad *Error
	gjson.ParseBytes(meta).ForEach(func(k, _ gjson.Result) bool {
		key := k.String()
		if !opKeys[key] && !chassisKeys[key] {
			bad = invalid("`%s` is not a field of this operation", key)
			return false
		}
		return true
	})
	if bad != nil {
		return req, bad
	}
	get := func(key string) gjson.Result { return gjson.GetBytes(meta, key) }
	// An optional field set to null is one not given: txcl can't leave a
	// WITH key out when the path behind it is missing, so a rule may pass
	// `count = @cap.input.count` whether or not the caller named one.
	given := func(key string) (gjson.Result, bool) {
		v := get(key)
		return v, v.Exists() && v.Type != gjson.Null
	}

	q := get("query")
	if !q.Exists() || q.Type == gjson.Null {
		return req, invalid("`query` is required")
	}
	if q.Type != gjson.String {
		return req, invalid("`query` must be a string")
	}
	req.Query = strings.Join(strings.Fields(q.String()), " ")
	if req.Query == "" {
		return req, invalid("`query` is empty")
	}
	if n := utf8.RuneCountInString(req.Query); n > MaxQueryChars {
		return req, invalid("`query` is %d characters; at most %d", n, MaxQueryChars)
	}
	if n := len(strings.Fields(req.Query)); n > MaxQueryWords {
		return req, invalid("`query` is %d words; at most %d", n, MaxQueryWords)
	}

	if v, ok := given("count"); ok {
		// A whole number, or one written as a string: query parameters
		// and a model's tool parameters often arrive that way.
		n, whole := wholeNumber(v)
		if !whole {
			return req, invalid("`count` must be a whole number")
		}
		req.Count = n
		if req.Count < 1 || req.Count > MaxCount {
			return req, invalid("`count` must be from 1 to %d", MaxCount)
		}
	}
	if v, ok := given("country"); ok {
		c := strings.ToUpper(strings.TrimSpace(v.String()))
		if v.Type != gjson.String || (c != "ALL" && !countryShape.MatchString(c)) {
			return req, invalid("`country` must be two letters, or ALL")
		}
		req.Country = c
	}
	if v, ok := given("language"); ok {
		l := strings.ToLower(strings.TrimSpace(v.String()))
		if v.Type != gjson.String || !languageShape.MatchString(l) {
			return req, invalid("`language` must be a two-letter ISO 639-1 code")
		}
		req.Language = l
	}
	if v, ok := given("freshness"); ok {
		f := strings.ToLower(strings.TrimSpace(v.String()))
		if v.Type != gjson.String || !freshnessValues[f] {
			return req, invalid("`freshness` must be any, day, week, month or year")
		}
		req.Freshness = f
	}
	if v, ok := given("safesearch"); ok {
		s := strings.ToLower(strings.TrimSpace(v.String()))
		if v.Type != gjson.String || !safeSearchValues[s] {
			return req, invalid("`safesearch` must be off, moderate or strict")
		}
		req.SafeSearch = s
	}
	return req, nil
}

// wholeNumber reads a JSON number, or a string holding one, as an int.
func wholeNumber(v gjson.Result) (int, bool) {
	switch v.Type {
	case gjson.Number:
		if v.Num != float64(int(v.Num)) {
			return 0, false
		}
		return int(v.Num), true
	case gjson.String:
		s := strings.TrimSpace(v.Str)
		if s == "" || len(s) > 6 {
			return 0, false
		}
		n := 0
		for _, c := range s {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int(c-'0')
		}
		return n, true
	}
	return 0, false
}

func invalid(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidRequest, Message: fmt.Sprintf(format, args...)}
}

// Normalize applies the answer's rules to a provider's results: a result
// with no title or an unusable URL is dropped; a URL over its cap drops
// its result (a cut URL points somewhere else); titles and descriptions
// are cut at their caps on a character boundary; and the answer as a
// whole stays under MaxAnswerBytes by dropping results from the end.
// Truncated reports any cut or drop for size.
func Normalize(in []Result) (out []Result, truncated bool) {
	out = []Result{}
	total := 0
	for _, r := range in {
		r.Title = Clean(r.Title)
		r.Description = Clean(r.Description)
		if r.Title == "" {
			continue
		}
		host, ok := checkURL(r.URL)
		if !ok {
			continue
		}
		if len(r.URL) > MaxURLBytes {
			truncated = true
			continue
		}
		r.Source = host
		if len(r.Title) > MaxTitleBytes {
			r.Title, truncated = cut(r.Title, MaxTitleBytes), true
		}
		if len(r.Description) > MaxDescriptionBytes {
			r.Description, truncated = cut(r.Description, MaxDescriptionBytes), true
		}
		size := len(r.Title) + len(r.URL) + len(r.Description) + len(r.Source) + 96
		if r.PageDate != nil {
			size += len(*r.PageDate)
		}
		if total+size > MaxAnswerBytes {
			truncated = true
			break
		}
		total += size
		out = append(out, r)
	}
	return out, truncated
}

// checkURL accepts an http(s) URL with a host and no userinfo, and
// returns its host, lower case.
func checkURL(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", false
	}
	return host, true
}

var tagShape = regexp.MustCompile(`<[^>]*>`)

// Clean makes provider text plain: tags stripped, entities decoded,
// whitespace collapsed.
func Clean(s string) string {
	if s == "" {
		return ""
	}
	if strings.ContainsRune(s, '<') {
		s = tagShape.ReplaceAllString(s, " ")
	}
	if strings.ContainsRune(s, '&') {
		s = html.UnescapeString(s)
	}
	return strings.Join(strings.Fields(s), " ")
}

// cut shortens s to at most n bytes on a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
