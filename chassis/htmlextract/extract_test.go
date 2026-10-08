package htmlextract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
)

const page = `<!doctype html>
<html><head>
  <base href="https://cdn.example.com/site/">
  <title>  Example
     Article  </title>
  <meta name="description" content="An interesting article">
  <meta property="og:title" content="OG Title">
  <meta property="og:image" content="/images/cover.jpg">
  <meta name="twitter:card" content="summary">
  <link rel="canonical" href="https://example.com/article">
  <link rel="icon shortcut" href="/favicon.ico">
  <script>var title = "not this";</script>
  <style>h1 { color: red }</style>
</head><body>
  <h1>Introduction</h1>
  <p>Some <b>bold</b>
     text.</p>
  <h2>Background</h2>
  <h2>Conclusion <script>ignored()</script></h2>
  <a href="/about">About</a> <a href="/products">Products</a> <a href="https://example.org">Out</a>
  <p>Ünïcødé — 日本語 ✓</p>
  <div><p>unclosed <i>and <b>misnested</i> markup</b>
</body></html>`

func specs(t *testing.T, raw string) []Spec {
	t.Helper()
	s, err := ParseSpecs(raw, DefaultLimits())
	if err != nil {
		t.Fatalf("ParseSpecs(%s): %v", raw, err)
	}
	return s
}

func extract(t *testing.T, body, contentType, raw string) map[string]any {
	t.Helper()
	res, err := Extract(context.Background(), []byte(body), contentType, specs(t, raw), DefaultLimits())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(res.Data, &m); err != nil {
		t.Fatalf("data %s: %v", res.Data, err)
	}
	return m
}

func TestExtractPreviewFields(t *testing.T) {
	got := extract(t, page, "text/html; charset=utf-8", `{
		"title":       {"selector": "title", "text": true},
		"description": {"selector": "meta[name='description']", "attr": "content"},
		"og_title":    {"selector": "meta[property='og:title']", "attr": "content"},
		"og_image":    {"selector": "meta[property=\"og:image\"]", "attr": "content"},
		"og_site":     {"selector": "meta[property='og:site_name']", "attr": "content"},
		"canonical":   {"selector": "link[rel=canonical]", "attr": "href"},
		"icon":        {"selector": "link[rel~=icon]", "attr": "href"},
		"base":        {"selector": "base[href]", "attr": "href"}
	}`)
	want := map[string]any{
		"title":       "Example Article",
		"description": "An interesting article",
		"og_title":    "OG Title",
		"og_image":    "/images/cover.jpg",
		"og_site":     nil,
		"canonical":   "https://example.com/article",
		"icon":        "/favicon.ico",
		"base":        "https://cdn.example.com/site/",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
}

func TestExtractMultipleMatchesInDocumentOrder(t *testing.T) {
	got := extract(t, page, "text/html", `{
		"headings": {"selector": "h1, h2", "text": true, "all": true, "limit": 20},
		"links":    {"selector": "a[href]", "attr": "href", "all": true},
		"two":      {"selector": "a", "attr": "href", "all": true, "limit": 2},
		"none":     {"selector": "table td", "text": true, "all": true},
		"missing":  {"selector": "table", "text": true}
	}`)
	assertJSON(t, got["headings"], `["Introduction","Background","Conclusion"]`)
	assertJSON(t, got["links"], `["/about","/products","https://example.org"]`)
	assertJSON(t, got["two"], `["/about","/products"]`)
	assertJSON(t, got["none"], `[]`)
	if got["missing"] != nil {
		t.Fatalf("missing = %#v, want null", got["missing"])
	}
}

func assertJSON(t *testing.T, v any, want string) {
	t.Helper()
	b, _ := json.Marshal(v)
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}

func TestTextNormalizationAndUnicode(t *testing.T) {
	got := extract(t, page, "text/html", `{
		"para":    {"selector": "body > p", "text": true, "all": true},
		"messy":   {"selector": "div > p", "text": true}
	}`)
	assertJSON(t, got["para"], `["Some bold text.","Ünïcødé — 日本語 ✓"]`)
	if got["messy"] != "unclosed and misnested markup" {
		t.Fatalf("messy = %#v", got["messy"])
	}
}

// An element matched for an attribute it lacks is skipped: the first
// element that has it answers.
func TestAttrSkipsElementsWithoutIt(t *testing.T) {
	got := extract(t, `<a>no href</a><a href="/x">x</a>`, "text/html", `{"h": {"selector": "a", "attr": "href"}}`)
	if got["h"] != "/x" {
		t.Fatalf("h = %#v", got["h"])
	}
}

func TestEmptyAndOddDocuments(t *testing.T) {
	for _, body := range []string{"", "   ", "plain text, no tags", "<<<>>>", "<html", "\x00\xff\xfe"} {
		got := extract(t, body, "text/html", `{"t": {"selector": "title", "text": true}, "all": {"selector": "p", "text": true, "all": true}}`)
		if got["t"] != nil {
			t.Fatalf("%q: t = %#v", body, got["t"])
		}
		assertJSON(t, got["all"], `[]`)
	}
}

func TestFieldOrderIsKept(t *testing.T) {
	res, err := Extract(context.Background(), []byte(page), "text/html", specs(t, `{"z": {"selector": "h1", "text": true}, "a": {"selector": "h1", "text": true}}`), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Data) != `{"z":"Introduction","a":"Introduction"}` {
		t.Fatalf("data = %s", res.Data)
	}
}

func TestCharsets(t *testing.T) {
	sjis, _ := japanese.ShiftJIS.NewEncoder().String(`<title>日本語のページ</title>`)
	got := extract(t, sjis, "text/html; charset=Shift_JIS", `{"t": {"selector": "title", "text": true}}`)
	if got["t"] != "日本語のページ" {
		t.Fatalf("shift_jis: %#v", got["t"])
	}
	// Declared in the document, not the header.
	meta, _ := japanese.ShiftJIS.NewEncoder().String(`<meta charset="shift_jis"><title>日本語</title>`)
	got = extract(t, meta, "text/html", `{"t": {"selector": "title", "text": true}}`)
	if got["t"] != "日本語" {
		t.Fatalf("meta shift_jis: %#v", got["t"])
	}
	latin, _ := charmap.Windows1252.NewEncoder().String(`<title>Café € naïve</title>`)
	got = extract(t, latin, "text/html; charset=windows-1252", `{"t": {"selector": "title", "text": true}}`)
	if got["t"] != "Café € naïve" {
		t.Fatalf("windows-1252: %#v", got["t"])
	}
	// Undeclared, ASCII for the first KiB, UTF-8 after: still UTF-8.
	late := "<title>" + strings.Repeat("a", 2000) + " café</title>"
	got = extract(t, late, "text/html", `{"t": {"selector": "title", "text": true}}`)
	if !strings.HasSuffix(got["t"].(string), " café") {
		t.Fatalf("late utf-8: %#v", got["t"])
	}
}

func TestValuesAreTruncatedAtAUTF8Boundary(t *testing.T) {
	long := strings.Repeat("日", 2000) // 6000 bytes
	res, err := Extract(context.Background(), []byte(`<p>`+long+`</p><meta name="x" content="`+long+`">`), "text/html",
		specs(t, `{"p": {"selector": "p", "text": true}, "m": {"selector": "meta", "attr": "content"}}`), DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false")
	}
	var m map[string]string
	json.Unmarshal(res.Data, &m)
	for k, v := range m {
		if len(v) > 4096 || len(v) < 4090 || !strings.HasPrefix(long, v) {
			t.Fatalf("%s: %d bytes, not a clean prefix", k, len(v))
		}
	}
}

func TestOutputLimit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, "<p>%s</p>", strings.Repeat("x", 4000))
	}
	_, err := Extract(context.Background(), []byte(b.String()), "text/html", specs(t, `{"p": {"selector": "p", "text": true, "all": true, "limit": 50}}`), DefaultLimits())
	wantCode(t, err, CodeOutputLimit)
}

func wantCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
	return e
}

func TestSpecValidation(t *testing.T) {
	bad := map[string]string{
		`[]`:                           "",
		`{}`:                           "",
		`{"t": "title"}`:               "t",
		`{"t": {"text": true}}`:        "t",
		`{"t": {"selector": "title"}}`: "t",
		`{"t": {"selector": "title", "text": true, "attr": "x"}}`:             "t",
		`{"t": {"selector": "title", "text": "yes"}}`:                         "t",
		`{"t": {"selector": "a", "attr": "href", "all": true, "limit": 51}}`:  "t",
		`{"t": {"selector": "a", "attr": "href", "all": true, "limit": 0}}`:   "t",
		`{"t": {"selector": "a", "attr": "href", "all": true, "limit": 2.5}}`: "t",
		`{"t": {"selector": "a", "atr": "href"}}`:                             "t",
		`{"t": {"selector": "a[", "text": true}}`:                             "t",
		`{"t": {"selector": "", "text": true}}`:                               "t",
		`{"has.dot": {"selector": "a", "text": true}}`:                        "",
		`{"1st": {"selector": "a", "text": true}}`:                            "",
		`{"t": {"selector": "a::before", "text": true}}`:                      "t",
	}
	for raw, field := range bad {
		_, err := ParseSpecs(raw, DefaultLimits())
		e := wantCode(t, err, CodeInvalidSelectors)
		if e.Field != field {
			t.Errorf("%s: field = %q, want %q (%s)", raw, e.Field, field, e.Message)
		}
	}
	var many []string
	for i := 0; i < 21; i++ {
		many = append(many, fmt.Sprintf(`"f%d": {"selector": "a", "text": true}`, i))
	}
	_, err := ParseSpecs("{"+strings.Join(many, ",")+"}", DefaultLimits())
	wantCode(t, err, CodeInvalidSelectors)
	_, err = ParseSpecs(`{"t": {"selector": "`+strings.Repeat("a,", 300)+`a", "text": true}}`, DefaultLimits())
	wantCode(t, err, CodeInvalidSelectors)
}

func TestSelectorGrammar(t *testing.T) {
	ok := []string{
		"title", "*", "#main", ".card", "meta[property='og:title']", "link[rel~=icon]",
		"a[href^='https:']", "a[href$='.pdf' i]", "a[href*=x]", "[lang|=en]", "a[data-x!=y]",
		"head > meta", "h1 + p", "article p", "main > article h2", "body > main > p",
		"p:not(.ad)", "li:first-child", "li:last-child", ":root", "h1, h2, h3",
		"article p, section p", "p:not(.a, .b)",
	}
	for _, sel := range ok {
		if _, err := ParseSpecs(fmt.Sprintf(`{"f": {"selector": %q, "text": true}}`, sel), DefaultLimits()); err != nil {
			t.Errorf("%s: %v", sel, err)
		}
	}
	refused := []string{
		"div:has(p)", "div:haschild(p)", "p:contains(x)", "p:containsOwn(x)", "p:matches(x+)",
		"p:matchesOwn(x)", "a[href#=(x+)]", "li:nth-child(2n)", "li:nth-of-type(1)", "li:first-of-type",
		"li:last-of-type", "li:only-child", "li:only-of-type", "h1 ~ p", "html body p",
		"article p:not(div p)", "div:not(main p) span", "p:empty", "p:lang(en)", "input:checked",
		"a:link", "input:enabled",
	}
	for _, sel := range refused {
		_, err := ParseSpecs(fmt.Sprintf(`{"f": {"selector": %q, "text": true}}`, sel), DefaultLimits())
		wantCode(t, err, CodeInvalidSelectors)
	}
}

func TestDocumentCaps(t *testing.T) {
	sp := specs(t, `{"t": {"selector": "title", "text": true}}`)
	lim := DefaultLimits()

	small := lim
	small.MaxTokens = 100
	_, err := Extract(context.Background(), []byte(strings.Repeat("<i>x</i>", 100)), "text/html", sp, small)
	wantCode(t, err, CodeDocumentTooComplex)

	small = lim
	small.MaxDepth = 50
	_, err = Extract(context.Background(), []byte(strings.Repeat("<div>", 60)), "text/html", sp, small)
	wantCode(t, err, CodeDocumentTooComplex)
	// Unclosed <p> and <li> don't count toward the naive depth.
	if _, err = Extract(context.Background(), []byte("<ul>"+strings.Repeat("<li><p>x", 200)+"</ul>"), "text/html", sp, small); err != nil {
		t.Fatalf("optional end tags: %v", err)
	}

	small = lim
	small.MaxNodes = 100
	_, err = Extract(context.Background(), []byte(strings.Repeat("<i>x</i>", 100)), "text/html", sp, small)
	wantCode(t, err, CodeDocumentTooComplex)

	var attrs strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&attrs, " a%d", i)
	}
	_, err = Extract(context.Background(), []byte("<div"+attrs.String()+">x</div>"), "text/html", sp, lim)
	wantCode(t, err, CodeDocumentTooComplex)
}

// The work estimate refuses a deep document for descendant selectors
// before matching starts, and lets the same document through for `>`.
func TestWorkEstimate(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxWork = 1_000_000
	deep := strings.Repeat("<div>", 400) + strings.Repeat("</div>", 400)
	body := strings.Repeat(deep, 20) // 8000 elements, 400 deep
	desc := specs(t, `{"a": {"selector": "section div", "text": true}, "b": {"selector": "main div", "text": true}}`)
	_, err := Extract(context.Background(), []byte(body), "text/html", desc, lim)
	wantCode(t, err, CodeDocumentTooComplex)
	child := specs(t, `{"a": {"selector": "section > div", "text": true}}`)
	if _, err := Extract(context.Background(), []byte(body), "text/html", child, lim); err != nil {
		t.Fatalf("child combinator: %v", err)
	}
}

func TestTextVisitBudget(t *testing.T) {
	lim := DefaultLimits()
	lim.MaxTextVisits = 50
	res, err := Extract(context.Background(), []byte("<div>"+strings.Repeat("<i>x</i>", 100)+"</div>"), "text/html",
		specs(t, `{"d": {"selector": "div", "text": true}}`), lim)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("Truncated = false after the visit budget ran out")
	}
}
