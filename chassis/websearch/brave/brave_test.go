package brave

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/websearch"
)

func newP(t *testing.T, base string) websearch.Provider {
	t.Helper()
	p, err := New(websearch.Config{BraveURL: base})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewRefusesABadEndpoint(t *testing.T) {
	for _, bad := range []string{"ftp://x/", "not a url", "https://api.search.brave.com/res/v1/web/search?x=1", "https:///nohost"} {
		if _, err := New(websearch.Config{BraveURL: bad}); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
	if newP(t, "").Name() != "brave" {
		t.Fatal("name")
	}
	// A trailing slash on the root is the same root.
	fr, _ := newP(t, "https://brave.test/res/v1/").Build(websearch.Request{Query: "q", Kind: "web", Count: 1}, []byte("k"))
	if !strings.HasPrefix(fr.URL, "https://brave.test/res/v1/web/search?") {
		t.Fatalf("url = %s", fr.URL)
	}
}

func TestBuild(t *testing.T) {
	p := newP(t, "https://brave.test/res/v1")
	req := websearch.Request{Query: "law & order", Kind: "web", Count: 8, Country: "FR", Language: "ja", Freshness: "month", SafeSearch: "strict"}
	fr, err := p.Build(req, []byte("sk-key"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(fr.URL)
	if err != nil || u.Host != "brave.test" || u.Path != "/res/v1/web/search" {
		t.Fatalf("url = %s", fr.URL)
	}
	q := u.Query()
	want := map[string]string{
		"q": "law & order", "count": "8", "country": "FR", "search_lang": "jp", "freshness": "pm",
		"safesearch": "strict", "result_filter": "web", "text_decorations": "false",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("%s = %q, want %q (%s)", k, q.Get(k), v, fr.URL)
		}
	}
	if len(q) != len(want) {
		t.Fatalf("extra parameters: %v", q)
	}
	if fr.Header.Get("X-Subscription-Token") != "sk-key" || len(fr.Header) != 1 {
		t.Fatalf("headers = %v", fr.Header)
	}
	if strings.Contains(fr.URL, "sk-key") {
		t.Fatal("the key is in the URL")
	}

	// Defaults send nothing for country, language or freshness.
	fr, _ = p.Build(websearch.Request{Query: "q", Count: 10, Freshness: "any", SafeSearch: "moderate"}, []byte("k"))
	q = mustQuery(t, fr.URL)
	for _, k := range []string{"country", "search_lang", "freshness"} {
		if q.Has(k) {
			t.Fatalf("%s sent: %s", k, fr.URL)
		}
	}
	for in, out := range map[string]string{"zh": "zh-hans", "pt": "pt-br", "fr": "fr", "de": "de"} {
		fr, _ = p.Build(websearch.Request{Query: "q", Count: 1, Language: in}, []byte("k"))
		if got := mustQuery(t, fr.URL).Get("search_lang"); got != out {
			t.Fatalf("language %s → %s, want %s", in, got, out)
		}
	}
}

// A news search goes to the News Search API with the same parameters,
// less the two only the Web Search API reads.
func TestBuildNews(t *testing.T) {
	p := newP(t, "https://brave.test/res/v1")
	req := websearch.Request{Query: "site:nytimes.com", Kind: "news", Count: 5, Country: "US", Language: "en", Freshness: "day", SafeSearch: "moderate"}
	fr, err := p.Build(req, []byte("sk-key"))
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(fr.URL)
	if err != nil || u.Host != "brave.test" || u.Path != "/res/v1/news/search" {
		t.Fatalf("url = %s", fr.URL)
	}
	q := u.Query()
	want := map[string]string{
		"q": "site:nytimes.com", "count": "5", "country": "US", "search_lang": "en", "freshness": "pd", "safesearch": "moderate",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Fatalf("%s = %q, want %q (%s)", k, q.Get(k), v, fr.URL)
		}
	}
	if len(q) != len(want) {
		t.Fatalf("extra parameters: %v", q)
	}
	if fr.Header.Get("X-Subscription-Token") != "sk-key" || len(fr.Header) != 1 || strings.Contains(fr.URL, "sk-key") {
		t.Fatalf("headers = %v url = %s", fr.Header, fr.URL)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestParse(t *testing.T) {
	p := newP(t, "")
	body := `{"type": "search", "query": {"original": "q"}, "web": {"type": "search", "results": [
		{"title": "One", "url": "https://one.test/", "description": "first", "page_age": "2026-10-01T09:49:11", "profile": {"name": "x"}, "thumbnail": {"src": "y"}},
		{"title": "Two", "url": "https://two.test/", "description": "second", "page_age": "2026-09-30"},
		{"title": "Three", "url": "https://three.test/", "description": "third", "page_age": "yesterday"},
		{"title": "Four", "url": "https://four.test/"},
		"not an object"
	]}, "news": {"results": [{"title": "ignored"}]}}`
	out, err := p.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 4 {
		t.Fatalf("got %d results", len(out))
	}
	if *out[0].PageDate != "2026-10-01T09:49:11Z" || *out[1].PageDate != "2026-09-30T00:00:00Z" || out[2].PageDate != nil || out[3].PageDate != nil {
		t.Fatalf("dates: %v %v %v %v", out[0].PageDate, out[1].PageDate, out[2].PageDate, out[3].PageDate)
	}
	if out[0].Title != "One" || out[0].URL != "https://one.test/" || out[0].Description != "first" {
		t.Fatalf("first = %+v", out[0])
	}

	// No web block is an empty answer; a wrong shape is an error.
	if out, err := p.Parse([]byte(`{"type": "search", "query": {}}`)); err != nil || len(out) != 0 {
		t.Fatalf("no web: %v %v", out, err)
	}
	for _, bad := range []string{``, `not json`, `[]`, `{"web": {"results": {}}}`} {
		if _, err := p.Parse([]byte(bad)); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

// A news answer's results are at the top, not under web.
func TestParseNews(t *testing.T) {
	p := newP(t, "")
	body := `{"type": "news", "query": {"original": "site:nytimes.com"}, "results": [
		{"type": "news_result", "title": "Trump Rallies Red States", "url": "https://www.nytimes.com/2026/10/19/us/politics/trump.html",
		 "description": "The president <strong>campaigned</strong>", "age": "3 hours ago", "page_age": "2026-10-19T19:05:00",
		 "meta_url": {"hostname": "www.nytimes.com"}, "thumbnail": {"src": "x"}, "breaking": false},
		{"title": "Second", "url": "https://www.nytimes.com/2", "page_age": "2026-10-19"}
	]}`
	out, err := p.Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Title != "Trump Rallies Red States" || out[0].URL != "https://www.nytimes.com/2026/10/19/us/politics/trump.html" ||
		*out[0].PageDate != "2026-10-19T19:05:00Z" || *out[1].PageDate != "2026-10-19T00:00:00Z" {
		t.Fatalf("out = %+v", out)
	}
	if out, err := p.Parse([]byte(`{"type": "news", "query": {}}`)); err != nil || len(out) != 0 {
		t.Fatalf("no results: %v %v", out, err)
	}
	if _, err := p.Parse([]byte(`{"type": "news", "results": {}}`)); err == nil {
		t.Fatal("results as an object parsed")
	}
}

func TestFailure(t *testing.T) {
	p := newP(t, "")
	for status, code := range map[int]string{
		401: websearch.CodeProviderAuth, 403: websearch.CodeProviderAuth,
		400: websearch.CodeInvalidRequest, 422: websearch.CodeInvalidRequest,
		429: websearch.CodeRateLimited, 500: websearch.CodeProviderError, 503: websearch.CodeProviderError,
	} {
		e := p.Failure(status, nil)
		if e.Code != code || e.Status != status {
			t.Fatalf("%d → %s %d, want %s", status, e.Code, e.Status, code)
		}
	}
	h := http.Header{}
	h.Set("X-RateLimit-Limit", "1, 15000")
	h.Set("X-RateLimit-Remaining", "0, 1000")
	h.Set("X-RateLimit-Reset", "1, 1419704")
	if e := p.Failure(429, h); e.RetryAfter != 1 {
		t.Fatalf("per-second window spent: retry_after = %d", e.RetryAfter)
	}
	h.Set("X-RateLimit-Remaining", "1, 0")
	if e := p.Failure(429, h); e.RetryAfter != 1419704 {
		t.Fatalf("monthly window spent: retry_after = %d", e.RetryAfter)
	}
	if e := p.Failure(429, http.Header{}); e.RetryAfter != 1 {
		t.Fatalf("no headers: retry_after = %d", e.RetryAfter)
	}
}
