// Package brave is the Brave Search adapter for txco://web/search: the
// Web Search API, one GET with the tenant's key in X-Subscription-Token.
//
// Brave's own parameter list is the authority on which countries and
// languages it serves. The adapter carries only the spellings where
// Brave departs from ISO 639-1, and lets Brave refuse the rest: a 422
// comes back to the rule as txco_web_invalid_request, unbilled on Brave's
// side, which bills successful requests only.
package brave

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/fetch"
	"github.com/loremlabs/thanks-computer/chassis/websearch"
)

// Name is the registered provider name.
const Name = "brave"

// DefaultURL is the public Web Search endpoint.
const DefaultURL = "https://api.search.brave.com/res/v1/web/search"

// SecretName is the tenant secret that holds the key.
const SecretName = "BRAVE_SEARCH_KEY"

func init() {
	websearch.Register(Name, New)
}

type provider struct {
	base string
}

// New builds the adapter. The endpoint is the operator's to set
// (--web-search-brave-url); a rule never chooses it.
func New(cfg websearch.Config) (websearch.Provider, error) {
	base := cfg.BraveURL
	if base == "" {
		base = DefaultURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" {
		return nil, errors.New("brave: the endpoint must be an http(s) URL with no query")
	}
	return &provider{base: base}, nil
}

func (p *provider) Name() string              { return Name }
func (p *provider) RequiredSecrets() []string { return []string{SecretName} }

// freshness maps the contract's words to Brave's. `any` sends nothing.
var freshness = map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}

// languages maps an ISO 639-1 code to Brave's spelling where they
// differ. Everything else goes through as written.
var languages = map[string]string{"ja": "jp", "zh": "zh-hans", "pt": "pt-br"}

// Build makes the request. Always `result_filter=web` (only web results
// are read) and `text_decorations=false` (plain snippets; Brave's default
// marks them up).
func (p *provider) Build(req websearch.Request, key []byte) (fetch.Request, error) {
	q := url.Values{}
	q.Set("q", req.Query)
	q.Set("count", strconv.Itoa(req.Count))
	if req.Country != "" {
		q.Set("country", req.Country)
	}
	if req.Language != "" {
		l := req.Language
		if m, ok := languages[l]; ok {
			l = m
		}
		q.Set("search_lang", l)
	}
	if f := freshness[req.Freshness]; f != "" {
		q.Set("freshness", f)
	}
	if req.SafeSearch != "" {
		q.Set("safesearch", req.SafeSearch)
	}
	q.Set("result_filter", "web")
	q.Set("text_decorations", "false")
	h := http.Header{}
	h.Set("X-Subscription-Token", string(key))
	return fetch.Request{URL: p.base + "?" + q.Encode(), Header: h}, nil
}

// Parse reads web.results[]. Everything else Brave sends (deep results,
// profiles, thumbnails, schemas) is ignored.
func (p *provider) Parse(body []byte) ([]websearch.Result, error) {
	if !gjson.ValidBytes(body) {
		return nil, errors.New("brave: the answer is not JSON")
	}
	doc := gjson.ParseBytes(body)
	if !doc.IsObject() {
		return nil, errors.New("brave: the answer is not an object")
	}
	hits := doc.Get("web.results")
	if !hits.Exists() {
		// No web results at all: an empty answer, not a broken one.
		return nil, nil
	}
	if !hits.IsArray() {
		return nil, errors.New("brave: web.results is not an array")
	}
	var out []websearch.Result
	hits.ForEach(func(_, r gjson.Result) bool {
		if !r.IsObject() {
			return true
		}
		out = append(out, websearch.Result{
			Title:       r.Get("title").String(),
			URL:         r.Get("url").String(),
			Description: r.Get("description").String(),
			PageDate:    pageDate(r.Get("page_age").String()),
		})
		return true
	})
	return out, nil
}

// pageDate reads Brave's page_age, a date-time without a zone or a date,
// into RFC 3339 UTC; nil when absent or unreadable.
func pageDate(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			v := t.UTC().Format(time.RFC3339)
			return &v
		}
	}
	return nil
}

// Failure maps Brave's non-2xx answers. The body is never read.
func (p *provider) Failure(status int, header http.Header) *websearch.Error {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &websearch.Error{Code: websearch.CodeProviderAuth, Message: "the search provider refused the key", Status: status}
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return &websearch.Error{Code: websearch.CodeInvalidRequest, Message: "the search provider refused a parameter of the request", Status: status}
	case http.StatusTooManyRequests:
		return &websearch.Error{Code: websearch.CodeRateLimited, Message: "the search provider's rate limit was reached", Status: status, RetryAfter: retryAfter(header)}
	}
	return &websearch.Error{Code: websearch.CodeProviderError, Message: "the search provider answered " + strconv.Itoa(status), Status: status}
}

// retryAfter reads Brave's rate-limit headers, one value per window
// ("1, 1000" / "1, 1419704"): the longest reset among the windows that
// are spent, else 1 second.
func retryAfter(h http.Header) int {
	remaining := strings.Split(h.Get("X-RateLimit-Remaining"), ",")
	reset := strings.Split(h.Get("X-RateLimit-Reset"), ",")
	best := 0
	for i := range reset {
		spent := i >= len(remaining)
		if !spent {
			n, err := strconv.Atoi(strings.TrimSpace(remaining[i]))
			spent = err == nil && n <= 0
		}
		if !spent {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(reset[i])); err == nil && n > best {
			best = n
		}
	}
	if best <= 0 {
		return 1
	}
	return best
}
