package websearch

import (
	"strings"
	"testing"
)

func TestParseDefaultsAndTrim(t *testing.T) {
	req, err := Parse([]byte(`{"query": "  French   employment\tlaw  "}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Request{Query: "French employment law", Kind: "web", Count: DefaultCount, Freshness: "any", SafeSearch: "moderate"}
	if req != want {
		t.Fatalf("req = %+v, want %+v", req, want)
	}
}

func TestParseEveryField(t *testing.T) {
	req, err := Parse([]byte(`{"query": "q", "kind": "News", "count": 8, "country": "fr", "language": "FR", "freshness": "Month", "safesearch": "strict",
		"into": "_found", "timeout": "3s", "mode": "sync", "redact": "_found.results", "omit": "x", "secrets": {"a": {"secret": "B"}}, "grant": "g"}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Request{Query: "q", Kind: "news", Count: 8, Country: "FR", Language: "fr", Freshness: "month", SafeSearch: "strict"}
	if req != want {
		t.Fatalf("req = %+v, want %+v", req, want)
	}
	if _, err := Parse([]byte(`{"query": "q", "country": "all"}`)); err != nil {
		t.Fatalf("ALL: %v", err)
	}
	// A whole number written as a string is a whole number; an optional
	// field set to null is one not given.
	req, err = Parse([]byte(`{"query": "q", "kind": null, "count": "3", "country": null, "language": null, "freshness": null, "safesearch": null}`))
	if err != nil {
		t.Fatal(err)
	}
	if (req != Request{Query: "q", Kind: "web", Count: 3, Freshness: "any", SafeSearch: "moderate"}) {
		t.Fatalf("req = %+v", req)
	}
}

func TestParseRefusals(t *testing.T) {
	long := strings.Repeat("a", MaxQueryChars+1)
	words := strings.Repeat("w ", MaxQueryWords+1)
	cases := map[string]string{
		`{}`:                                 "`query` is required",
		`{"query": 3}`:                       "`query` must be a string",
		`{"query": "   "}`:                   "`query` is empty",
		`{"query": "` + long + `"}`:          "characters; at most 500",
		`{"query": "` + words + `"}`:         "words; at most 75",
		`{"query": "q", "kind": "images"}`:   "`kind` must be web or news",
		`{"query": "q", "kind": true}`:       "`kind` must be web or news",
		`{"query": "q", "count": 0}`:         "`count` must be from 1 to 20",
		`{"query": "q", "count": 21}`:        "`count` must be from 1 to 20",
		`{"query": "q", "count": 2.5}`:       "`count` must be a whole number",
		`{"query": "q", "count": "five"}`:    "`count` must be a whole number",
		`{"query": "q", "count": "2.5"}`:     "`count` must be a whole number",
		`{"query": "q", "count": "0"}`:       "`count` must be from 1 to 20",
		`{"query": null}`:                    "`query` is required",
		`{"query": "q", "country": "FRA"}`:   "`country` must be two letters, or ALL",
		`{"query": "q", "language": "fra"}`:  "`language` must be a two-letter",
		`{"query": "q", "freshness": "pd"}`:  "`freshness` must be any, day, week, month or year",
		`{"query": "q", "safesearch": "on"}`: "`safesearch` must be off, moderate or strict",
		`{"query": "q", "freshnes": "day"}`:  "`freshnes` is not a field of this operation",
		`{"query": "q", "api_key": "x"}`:     "`api_key` is not a field of this operation",
		`{"query": "q", "headers": {}}`:      "`headers` is not a field of this operation",
	}
	for meta, want := range cases {
		_, err := Parse([]byte(meta))
		if err == nil {
			t.Fatalf("%s: accepted", meta)
		}
		if err.Code != CodeInvalidRequest || !strings.Contains(err.Message, want) {
			t.Fatalf("%s: %s %q, want %q", meta, err.Code, err.Message, want)
		}
	}
	// 500 characters of multi-byte text is 500 characters, not bytes.
	if _, err := Parse([]byte(`{"query": "` + strings.Repeat("é", MaxQueryChars) + `"}`)); err != nil {
		t.Fatalf("500 runes: %v", err)
	}
}

func date(s string) *string { return &s }

func TestNormalize(t *testing.T) {
	in := []Result{
		{Title: " <b>Hello</b> &amp; world ", URL: "HTTPS://Example.FR/a?x=1", Description: "<p>one  two</p>", PageDate: date("2026-10-01T00:00:00Z")},
		{Title: "", URL: "https://example.test/notitle"},
		{Title: "no url"},
		{Title: "bad scheme", URL: "ftp://example.test/"},
		{Title: "userinfo", URL: "https://u:p@example.test/"},
		{Title: "long url", URL: "https://example.test/" + strings.Repeat("p", MaxURLBytes)},
		{Title: strings.Repeat("é", MaxTitleBytes), URL: "https://example.test/t", Description: strings.Repeat("d", MaxDescriptionBytes+5)},
	}
	out, truncated := Normalize(in)
	if !truncated {
		t.Fatal("truncated should be set: a long URL was dropped and a title cut")
	}
	if len(out) != 2 {
		t.Fatalf("got %d results: %+v", len(out), out)
	}
	first := out[0]
	if first.Title != "Hello & world" || first.Description != "one two" || first.Source != "example.fr" || first.URL != "HTTPS://Example.FR/a?x=1" || *first.PageDate != "2026-10-01T00:00:00Z" {
		t.Fatalf("first = %+v", first)
	}
	second := out[1]
	if len(second.Title) > MaxTitleBytes || !strings.HasSuffix(second.Title, "é") || len(second.Description) != MaxDescriptionBytes {
		t.Fatalf("cuts: title %d bytes, description %d bytes", len(second.Title), len(second.Description))
	}
}

func TestNormalizeEmptyIsAnArray(t *testing.T) {
	out, truncated := Normalize(nil)
	if out == nil || len(out) != 0 || truncated {
		t.Fatalf("out = %#v truncated = %v", out, truncated)
	}
}

func TestNormalizeTotalCap(t *testing.T) {
	var in []Result
	for i := 0; i < 100; i++ {
		in = append(in, Result{Title: "t", URL: "https://example.test/", Description: strings.Repeat("d", MaxDescriptionBytes)})
	}
	out, truncated := Normalize(in)
	if !truncated || len(out) >= 100 || len(out) < 20 {
		t.Fatalf("kept %d, truncated %v", len(out), truncated)
	}
}
