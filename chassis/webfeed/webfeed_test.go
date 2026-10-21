package webfeed

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const rssBody = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:atom="http://www.w3.org/2005/Atom">
<channel>
  <title>Example &amp; Sons</title>
  <atom:link href="https://example.test/feed.xml" rel="self" type="application/rss+xml"/>
  <link>https://example.test/</link>
  <description><![CDATA[News from <b>Example</b>]]></description>
  <lastBuildDate>Tue, 20 Oct 2026 09:30:00 GMT</lastBuildDate>
  <item>
    <title><![CDATA[First &amp; foremost]]></title>
    <link>/posts/first</link>
    <guid isPermaLink="false">post-1</guid>
    <pubDate>Mon, 19 Oct 2026 08:00:00 EST</pubDate>
    <description>&lt;p&gt;A &lt;a href="x"&gt;teaser&lt;/a&gt;&amp;nbsp;here.&lt;/p&gt;&lt;script&gt;alert(1)&lt;/script&gt;</description>
    <content:encoded><![CDATA[<p>The whole thing.</p>]]></content:encoded>
    <dc:creator>Ada</dc:creator>
  </item>
  <item>
    <title>Second</title>
    <guid>https://example.test/posts/second</guid>
    <pubDate>Sun, 18 Oct 2026 10:00:00 +0200</pubDate>
    <content:encoded><![CDATA[<div>Only <em>content</em>.</div>]]></content:encoded>
    <author>bob@example.test (Bob)</author>
  </item>
  <item>
    <description>no title, no link: dropped</description>
  </item>
  <item>
    <title>Odd date</title>
    <link>ftp://example.test/nope</link>
    <pubDate>yesterday</pubDate>
  </item>
</channel>
</rss>`

func TestReadRSS(t *testing.T) {
	doc, err := Read([]byte(rssBody), "application/rss+xml; charset=utf-8", "https://example.test/feed.xml")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Format != "rss" || doc.Total != 3 || doc.Truncated {
		t.Fatalf("doc = %+v", doc)
	}
	if doc.Feed.Title != "Example & Sons" || doc.Feed.Link != "https://example.test/" || doc.Feed.Description != "News from Example" || doc.Feed.Updated == nil || *doc.Feed.Updated != "2026-10-20T09:30:00Z" {
		t.Fatalf("feed = %+v", doc.Feed)
	}
	a := doc.Items[0]
	if a.Title != "First & foremost" || a.URL != "https://example.test/posts/first" || a.Source != "example.test" || a.ID != "post-1" || a.Author != "Ada" {
		t.Fatalf("item 0 = %+v", a)
	}
	if a.Date == nil || *a.Date != "2026-10-19T13:00:00Z" {
		t.Fatalf("item 0 date = %v", a.Date)
	}
	if a.Summary != "A teaser here." {
		t.Fatalf("item 0 summary = %q", a.Summary)
	}
	b := doc.Items[1]
	if b.URL != "https://example.test/posts/second" || b.ID != "https://example.test/posts/second" || b.Summary != "Only content." || b.Author != "Bob" || b.Date == nil || *b.Date != "2026-10-18T08:00:00Z" {
		t.Fatalf("item 1 = %+v", b)
	}
	c := doc.Items[2]
	if c.Title != "Odd date" || c.URL != "" || c.Date != nil {
		t.Fatalf("item 2 = %+v", c)
	}
}

func TestReadRDF(t *testing.T) {
	body := `<?xml version="1.0"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
  <channel rdf:about="https://old.test/"><title>Old school</title><link>https://old.test/</link><description>RSS 1.0</description><dc:date>2026-10-01T00:00:00+00:00</dc:date></channel>
  <item rdf:about="https://old.test/a"><title>A</title><link>https://old.test/a</link><dc:date>2026-10-02T12:00:00+02:00</dc:date><dc:creator>Carol</dc:creator><description>a</description></item>
  <item rdf:about="https://old.test/b"><title>B</title><link>https://old.test/b</link></item>
</rdf:RDF>`
	doc, err := Read([]byte(body), "text/xml", "https://old.test/rss")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Format != "rss" || doc.Total != 2 || doc.Feed.Title != "Old school" || doc.Feed.Updated == nil || *doc.Feed.Updated != "2026-10-01T00:00:00Z" {
		t.Fatalf("doc = %+v", doc)
	}
	if a := doc.Items[0]; a.Title != "A" || a.Author != "Carol" || *a.Date != "2026-10-02T10:00:00Z" {
		t.Fatalf("item = %+v", a)
	}
	if b := doc.Items[1]; b.Date != nil || b.URL != "https://old.test/b" {
		t.Fatalf("item = %+v", b)
	}
}

const atomBody = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://atom.test/">
  <title type="text">Atom &amp; co</title>
  <subtitle>Entries</subtitle>
  <link rel="self" href="https://atom.test/feed"/>
  <link rel="alternate" type="text/html" href="https://atom.test/"/>
  <updated>2026-10-20T10:00:00Z</updated>
  <author><name>Site Author</name></author>
  <entry>
    <id>tag:atom.test,2026:1</id>
    <title type="html">One &lt;em&gt;emphasised&lt;/em&gt;</title>
    <link rel="enclosure" type="audio/mpeg" href="https://cdn.test/1.mp3"/>
    <link rel="alternate" type="text/html" href="/entries/1"/>
    <published>2026-10-19T09:00:00-04:00</published>
    <updated>2026-10-19T12:00:00Z</updated>
    <summary type="html">&lt;p&gt;Short &amp;amp; sweet.&lt;/p&gt;</summary>
    <content type="html">&lt;p&gt;Long.&lt;/p&gt;</content>
    <author><name>Dee</name><email>dee@atom.test</email></author>
  </entry>
  <entry>
    <id>tag:atom.test,2026:2</id>
    <title>Two</title>
    <link href="https://atom.test/entries/2"/>
    <updated>2026-10-18T12:00:00Z</updated>
    <content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>First para.</p><p>Second <b>para</b>.</p></div></content>
  </entry>
</feed>`

func TestReadAtom(t *testing.T) {
	doc, err := Read([]byte(atomBody), "application/atom+xml", "https://atom.test/feed")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Format != "atom" || doc.Total != 2 || doc.Feed.Title != "Atom & co" || doc.Feed.Link != "https://atom.test/" || doc.Feed.Description != "Entries" || *doc.Feed.Updated != "2026-10-20T10:00:00Z" {
		t.Fatalf("doc = %+v feed = %+v", doc, doc.Feed)
	}
	a := doc.Items[0]
	if a.Title != "One emphasised" || a.URL != "https://atom.test/entries/1" || a.Source != "atom.test" || a.ID != "tag:atom.test,2026:1" || a.Author != "Dee" || a.Summary != "Short & sweet." || *a.Date != "2026-10-19T13:00:00Z" {
		t.Fatalf("entry 0 = %+v", a)
	}
	b := doc.Items[1]
	if b.Title != "Two" || b.URL != "https://atom.test/entries/2" || b.Author != "Site Author" || b.Summary != "First para. Second para ." && b.Summary != "First para. Second para." || *b.Date != "2026-10-18T12:00:00Z" {
		t.Fatalf("entry 1 = %+v", b)
	}
}

func TestReadJSONFeed(t *testing.T) {
	body := `{"version": "https://jsonfeed.org/version/1.1", "title": "JSON &amp; co", "home_page_url": "https://json.test/", "description": "d",
	  "authors": [{"name": "Feed Author"}],
	  "items": [
	    {"id": "1", "url": "https://json.test/1", "title": "One", "content_html": "<p>Hi <b>there</b></p>", "date_published": "2026-10-19T09:00:00Z", "authors": [{"name": "Eve"}]},
	    {"id": "2", "external_url": "https://elsewhere.test/2", "content_text": "Just text", "date_modified": "2026-10-18T09:00:00+01:00"},
	    {"id": "3"}
	  ]}`
	doc, err := Read([]byte(body), "application/feed+json", "https://json.test/feed.json")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Format != "json" || doc.Total != 2 || doc.Feed.Title != "JSON & co" || doc.Feed.Link != "https://json.test/" {
		t.Fatalf("doc = %+v feed = %+v", doc, doc.Feed)
	}
	if a := doc.Items[0]; a.Title != "One" || a.Summary != "Hi there" || a.Author != "Eve" || a.URL != "https://json.test/1" || *a.Date != "2026-10-19T09:00:00Z" {
		t.Fatalf("item 0 = %+v", a)
	}
	if b := doc.Items[1]; b.Title != "" || b.URL != "https://elsewhere.test/2" || b.Source != "elsewhere.test" || b.Summary != "Just text" || b.Author != "Feed Author" || *b.Date != "2026-10-18T08:00:00Z" {
		t.Fatalf("item 1 = %+v", b)
	}
	if _, err := Read([]byte(`{"hello": "world"}`), "application/json", ""); err == nil || err.Code != CodeNotAFeed {
		t.Fatalf("err = %v", err)
	}
	if _, err := Read([]byte(`{"items": [`), "application/json", ""); err == nil || err.Code != CodeInvalidFeed {
		t.Fatalf("err = %v", err)
	}
}

func TestReadNotAFeed(t *testing.T) {
	page := `<!doctype html><html><head><title>Site</title>
	<link rel="stylesheet" href="/s.css">
	<link rel="alternate" type="application/rss+xml" title="RSS" href="/feed.xml">
	<link rel="alternate" type="application/atom+xml" href="https://site.test/atom">
	<link rel="alternate" type="application/rss+xml" href="/feed.xml">
	<link rel="alternate" type="text/html" hreflang="fr" href="/fr">
	</head><body><link rel="alternate" type="application/rss+xml" href="/late.xml"></body></html>`
	_, err := Read([]byte(page), "text/html; charset=utf-8", "https://site.test/x/page")
	if err == nil || err.Code != CodeNotAFeed {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(err.Feeds, " ") != "https://site.test/feed.xml https://site.test/atom" || !strings.Contains(err.Message, "2 feeds") {
		t.Fatalf("err = %+v", err)
	}
	for _, body := range []string{"", "   ", "just words", "<foo><bar/></foo>", "<html><body>no feeds</body></html>"} {
		_, err := Read([]byte(body), "text/plain", "https://site.test/")
		if err == nil || err.Code != CodeNotAFeed || len(err.Feeds) != 0 {
			t.Fatalf("%q: err = %+v", body, err)
		}
	}
}

func TestReadBrokenXML(t *testing.T) {
	// Broken before any item: refused.
	if _, err := Read([]byte(`<rss><channel><title>x</title><item><title>a</title></ite`), "text/xml", ""); err == nil || err.Code != CodeInvalidFeed {
		t.Fatalf("err = %v", err)
	}
	// Broken after items: what was read, marked truncated.
	doc, err := Read([]byte(`<rss><channel><title>x</title><item><title>a</title><link>https://x.test/a</link></item><item><title>b</title><link>https://x.test/b</link></item><item><title>c`), "text/xml", "")
	if err != nil || doc.Total != 2 || !doc.Truncated {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
	// Unescaped ampersands and HTML entities are tolerated.
	doc, err = Read([]byte(`<rss><channel><title>Tom &amp; Jerry &nbsp; & co</title><item><title>a &copy; b</title><link>https://x.test/a?a=1&b=2</link></item></channel></rss>`), "text/xml", "")
	if err != nil || doc.Feed.Title != "Tom & Jerry & co" || doc.Items[0].Title != "a © b" || doc.Items[0].URL != "https://x.test/a?a=1&b=2" {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
	// Nested too deeply: refused.
	deep := strings.Repeat("<a>", 80) + strings.Repeat("</a>", 80)
	if _, err := Read([]byte(`<rss><channel>`+deep+`</channel></rss>`), "text/xml", ""); err == nil || err.Code != CodeInvalidFeed {
		t.Fatalf("err = %v", err)
	}
}

func TestReadCharsets(t *testing.T) {
	// Declared in the XML declaration: é as one Latin-1 byte.
	body := append([]byte(`<?xml version="1.0" encoding="ISO-8859-1"?><rss><channel><title>Caf`), 0xE9)
	body = append(body, []byte(`</title><item><title>Th`+"\xe9"+`</title><link>https://x.test/a</link></item></channel></rss>`)...)
	doc, err := Read(body, "application/rss+xml", "")
	if err != nil || doc.Feed.Title != "Café" || doc.Items[0].Title != "Thé" {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
	// Declared only by the Content-Type.
	body = append([]byte(`<rss><channel><title>Caf`), 0xE9)
	body = append(body, []byte(`</title><item><title>a</title><link>https://x.test/a</link></item></channel></rss>`)...)
	doc, err = Read(body, "text/xml; charset=iso-8859-1", "")
	if err != nil || doc.Feed.Title != "Café" {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
	// A UTF-8 BOM is fine.
	doc, err = Read(append([]byte{0xEF, 0xBB, 0xBF}, []byte(`<rss><channel><title>bom</title><item><title>a</title><link>https://x.test/a</link></item></channel></rss>`)...), "", "")
	if err != nil || doc.Feed.Title != "bom" {
		t.Fatalf("doc = %+v err = %v", doc, err)
	}
}

func TestReadStopsAtMaxScanItems(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<rss><channel><title>many</title>`)
	for i := 0; i < MaxScanItems+20; i++ {
		fmt.Fprintf(&b, `<item><title>n%d</title><link>https://x.test/%d</link></item>`, i, i)
	}
	b.WriteString(`</channel></rss>`)
	doc, err := Read([]byte(b.String()), "text/xml", "")
	if err != nil || doc.Total != MaxScanItems || !doc.Truncated || doc.Items[MaxScanItems-1].Title != fmt.Sprintf("n%d", MaxScanItems-1) {
		t.Fatalf("total %d truncated %v err %v", doc.Total, doc.Truncated, err)
	}
}

func TestParseDates(t *testing.T) {
	for in, want := range map[string]string{
		"2026-10-19T09:00:00Z":                   "2026-10-19T09:00:00Z",
		"2026-10-19T09:00:00.123+02:00":          "2026-10-19T07:00:00Z",
		"2026-10-19T09:00:00":                    "2026-10-19T09:00:00Z",
		"2026-10-19":                             "2026-10-19T00:00:00Z",
		"Mon, 19 Oct 2026 09:00:00 GMT":          "2026-10-19T09:00:00Z",
		"Mon, 19 Oct 2026 09:00:00 +0000":        "2026-10-19T09:00:00Z",
		"Mon, 19 Oct 2026 09:00:00 PDT":          "2026-10-19T16:00:00Z",
		"Mon, 19 Oct 2026 09:00 EST":             "2026-10-19T14:00:00Z",
		"Mon, 5 Oct 2026 09:00:00 +0100":         "2026-10-05T08:00:00Z",
		"19 Oct 2026 09:00:00 UT":                "2026-10-19T09:00:00Z",
		"Monday, 19 October 2026 09:00:00 -0700": "2026-10-19T16:00:00Z",
		"October 19, 2026":                       "2026-10-19T00:00:00Z",
		"  Mon,  19 Oct 2026 09:00:00 GMT ":      "2026-10-19T09:00:00Z",
	} {
		got := parseDate(in)
		if got == nil || *got != want {
			t.Errorf("%q: got %v, want %s", in, got, want)
		}
	}
	for _, in := range []string{"", "yesterday", "2026-13-01", "Mon, 19 Oct"} {
		if got := parseDate(in); got != nil {
			t.Errorf("%q: got %s, want nil", in, *got)
		}
	}
}

func TestParseRequest(t *testing.T) {
	req, err := Parse([]byte(`{"url": " https://x.test/feed ", "count": "7", "since": "2026-10-01", "into": "_f", "timeout": 3000}`))
	if err != nil || req.URL != "https://x.test/feed" || req.Count != 7 || req.Since == nil || !req.Since.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("req = %+v err = %v", req, err)
	}
	req, err = Parse([]byte(`{"url": "https://x.test/feed", "count": null, "since": null}`))
	if err != nil || req.Count != DefaultCount || req.Since != nil {
		t.Fatalf("req = %+v err = %v", req, err)
	}
	for _, meta := range []string{
		`{}`, `{"url": ""}`, `{"url": 7}`, `{"url": "https://x.test", "count": 0}`, `{"url": "https://x.test", "count": 51}`,
		`{"url": "https://x.test", "count": 1.5}`, `{"url": "https://x.test", "sinse": "2026-10-01"}`, `{"url": "https://x.test", "since": "last week"}`,
		`{"url": "https://x.test", "since": 1700000000}`, `not json`,
	} {
		if _, err := Parse([]byte(meta)); err == nil || err.Code != CodeInvalidRequest {
			t.Errorf("%s: err = %v", meta, err)
		}
	}
}

func d(s string) *string { return &s }

func TestSelect(t *testing.T) {
	items := []Item{
		{Title: "old", URL: "https://x.test/1", Date: d("2026-10-01T00:00:00Z")},
		{Title: "undated", URL: "https://x.test/2"},
		{Title: "new", URL: "https://x.test/3", Date: d("2026-10-20T00:00:00Z")},
		{Title: "mid", URL: "https://x.test/4", Date: d("2026-10-10T00:00:00Z")},
	}
	// since drops the old one, keeps the undated one, and with an undated
	// item the document's order stands.
	since := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	out, trunc := Select(items, Request{Count: 10, Since: &since})
	if trunc || titles(out) != "undated new mid" {
		t.Fatalf("out = %s trunc %v", titles(out), trunc)
	}
	// All dated: newest first; count cuts without truncation.
	out, trunc = Select([]Item{items[0], items[2], items[3]}, Request{Count: 2})
	if trunc || titles(out) != "new mid" {
		t.Fatalf("out = %s trunc %v", titles(out), trunc)
	}
	// Caps: a long title is cut, a long URL dropped, a long id dropped.
	long := Item{Title: strings.Repeat("é", MaxTitleBytes), URL: "https://x.test/" + strings.Repeat("p", MaxURLBytes), ID: strings.Repeat("i", MaxIDBytes+1), Source: "x.test", Summary: strings.Repeat("s", MaxSummaryBytes+1)}
	out, trunc = Select([]Item{long}, Request{Count: 5})
	if !trunc || len(out) != 1 || len(out[0].Title) > MaxTitleBytes || len(out[0].Title) < MaxTitleBytes-1 || out[0].URL != "" || out[0].Source != "" || out[0].ID != "" || len(out[0].Summary) != MaxSummaryBytes {
		t.Fatalf("out = %+v trunc %v", out, trunc)
	}
	// The answer as a whole: fifty maxed items are more than MaxAnswerBytes.
	var many []Item
	for i := 0; i < MaxCount; i++ {
		many = append(many, Item{Title: strings.Repeat("t", MaxTitleBytes), URL: "https://x.test/" + strings.Repeat("u", MaxURLBytes-20), ID: strings.Repeat("i", MaxIDBytes), Summary: strings.Repeat("s", MaxSummaryBytes), Author: strings.Repeat("a", MaxAuthorBytes)})
	}
	out, trunc = Select(many, Request{Count: MaxCount})
	if !trunc || len(out) == 0 || len(out) >= MaxCount {
		t.Fatalf("kept %d trunc %v", len(out), trunc)
	}
}

func titles(items []Item) string {
	var s []string
	for _, it := range items {
		s = append(s, it.Title)
	}
	return strings.Join(s, " ")
}

func TestPlainText(t *testing.T) {
	for in, want := range map[string]string{
		"":                                       "",
		"plain  text\n here":                     "plain text here",
		"Tom &amp; Jerry &#8217;s":               "Tom & Jerry ’s",
		"<p>One</p><p>Two</p>":                   "One Two",
		"<p>a<br/>b</p><script>x=1</script>c":    "a b c",
		"<style>p{}</style>Hi <img src=x> there": "Hi there",
		"a < b and c > d":                        "a < b and c > d",
		"<div><p>nested <em>deep</em> text</p></div>": "nested deep text",
	} {
		if got := plainText(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
