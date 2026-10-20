package htmlextract

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func read(t *testing.T, page string, opts ReadOptions) *ReadResult {
	t.Helper()
	res, err := Read(context.Background(), []byte(page), "text/html; charset=utf-8", opts, DefaultLimits())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return res
}

func readErr(t *testing.T, page string, opts ReadOptions, lim Limits) (*ReadResult, *Error) {
	t.Helper()
	res, err := Read(context.Background(), []byte(page), "text/html", opts, lim)
	if err == nil {
		t.Fatalf("Read answered %q, want an error", res.Text)
	}
	var he *Error
	if !errors.As(err, &he) {
		t.Fatalf("not an *Error: %v", err)
	}
	return res, he
}

const article = `<!doctype html><html><head><title> The  Page </title><base href="https://example.test/docs/"></head>
<body>
<nav><a href="/">Home</a></nav>
<main>
<h1>A   heading</h1>
<p>Words with <b>bold</b>, <i>italic</i> and <code>code</code>. A <a href="../x?a=(1)">link</a>, an <a href="#top">anchor</a> and <a href="javascript:void(0)">no link</a>.</p>
<img src="/i.png" alt="An image">
<ul><li>one</li><li>two <ul><li>two a</li></ul></li></ul>
<ol start="3"><li>three</li><li>four</li></ol>
<blockquote><p>quoted</p><p>twice</p></blockquote>
<pre><code>line 1
  line 2</code></pre>
<table><thead><tr><th>a</th><th>b | c</th></tr></thead><tbody><tr><td>1</td><td><a href="/r">2</a></td></tr><tr><td>3</td></tr></tbody></table>
<hr>
<dl><dt>term</dt><dd>meaning</dd></dl>
<p hidden>hidden</p><p aria-hidden="true">also hidden</p>
<script>alert(1)</script><style>p{}</style><template><p>never</p></template>
<p>End<br>line</p>
</main>
<footer>footer</footer>
</body></html>`

func TestReadRendersMarkdown(t *testing.T) {
	res := read(t, article, ReadOptions{BaseURL: "https://example.test/a/b"})
	want := strings.Join([]string{
		"# A heading",
		"",
		"Words with **bold**, _italic_ and `code`. A [link](https://example.test/x?a=%281%29), an anchor and no link.",
		"",
		"An image",
		"",
		"- one",
		"- two",
		"  - two a",
		"",
		"3. three",
		"4. four",
		"",
		"> quoted",
		">",
		"> twice",
		"",
		"```",
		"line 1",
		"  line 2",
		"```",
		"",
		"| a | b \\| c |",
		"| --- | --- |",
		"| 1 | 2 |",
		"| 3 |  |",
		"",
		"---",
		"",
		"term",
		": meaning",
		"",
		"End",
		"line",
	}, "\n")
	if res.Text != want {
		t.Errorf("rendered:\n%s\n\nwant:\n%s", res.Text, want)
	}
	if res.Title != "The Page" || res.Root != "main" || res.Part != 1 || res.Parts != 1 || res.Truncated {
		t.Errorf("title %q root %q part %d/%d truncated %v", res.Title, res.Root, res.Part, res.Parts, res.Truncated)
	}
	if res.Chars != utf8.RuneCountInString(want) || res.TotalChars != res.Chars {
		t.Errorf("chars %d total %d want %d", res.Chars, res.TotalChars, utf8.RuneCountInString(want))
	}
	// The nav and the footer are outside main: not read.
	if strings.Contains(res.Text, "Home") || strings.Contains(res.Text, "footer") || strings.Contains(res.Text, "hidden") || strings.Contains(res.Text, "never") {
		t.Errorf("read past the root or a skipped element: %q", res.Text)
	}
}

func TestReadPicksTheRoot(t *testing.T) {
	// No main: the article.
	page := `<html><head><title>t</title></head><body><nav>nav words</nav><header>site header</header><article><p>the article</p></article><aside>aside words</aside></body></html>`
	res := read(t, page, ReadOptions{})
	if res.Root != "article" || res.Text != "the article" {
		t.Errorf("root %q text %q", res.Root, res.Text)
	}
	// No main, no article: the body, with its furniture left out.
	page = `<html><body><nav>nav words</nav><header>site header</header><div role="navigation">menu</div><div class="post"><p>the post</p></div><footer>foot</footer></body></html>`
	res = read(t, page, ReadOptions{})
	if res.Root != "body" || res.Text != "the post" {
		t.Errorf("root %q text %q", res.Root, res.Text)
	}
	// A named root: a header inside it is read (only the body fallback
	// leaves furniture out); navigation inside it is not, wherever it sits.
	page = `<html><body><main><p>main words</p></main><div id="x"><header><h2>Inner</h2></header><nav>tools</nav><div role="navigation">more tools</div><p>x words</p></div></body></html>`
	res = read(t, page, ReadOptions{Root: "#x"})
	if res.Root != "#x" || res.Text != "## Inner\n\nx words" {
		t.Errorf("root %q text %q", res.Root, res.Text)
	}
	_, err := readErr(t, page, ReadOptions{Root: "#nope"}, DefaultLimits())
	if err.Code != CodeRootNotFound {
		t.Errorf("code %s", err.Code)
	}
	_, err = readErr(t, page, ReadOptions{Root: "div:has(p)"}, DefaultLimits())
	if err.Code != CodeInvalidSelectors || !strings.Contains(err.Message, "root") {
		t.Errorf("%s: %s", err.Code, err.Message)
	}
}

func TestReadInParts(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body><main>")
	for i := 1; i <= 120; i++ {
		b.WriteString("<p>Paragraph " + strconv.Itoa(i) + " ")
		b.WriteString(strings.Repeat("w", 40))
		b.WriteString("</p>")
	}
	b.WriteString("</main></body></html>")
	page := b.String()
	first := read(t, page, ReadOptions{PartChars: 1000})
	if first.Part != 1 || first.Parts < 5 || first.Chars > 1000 || !strings.HasPrefix(first.Text, "Paragraph ") || !strings.HasSuffix(first.Text, "w") {
		t.Errorf("part %d/%d chars %d: %q", first.Part, first.Parts, first.Chars, first.Text[:60])
	}
	last := read(t, page, ReadOptions{PartChars: 1000, Last: true})
	if last.Part != first.Parts || last.Chars == 0 || last.TotalChars != first.TotalChars {
		t.Errorf("last = %d/%d chars %d total %d vs %d", last.Part, last.Parts, last.Chars, last.TotalChars, first.TotalChars)
	}
	third := read(t, page, ReadOptions{PartChars: 1000, Part: 3})
	if third.Part != 3 || strings.HasPrefix(third.Text, "Paragraph 1 ") {
		t.Errorf("part 3 repeats part 1: %q", third.Text[:40])
	}
	res, err := readErr(t, page, ReadOptions{PartChars: 1000, Part: first.Parts + 1}, DefaultLimits())
	if err.Code != CodeNoSuchPart || res == nil || res.Parts != first.Parts || !strings.Contains(err.Message, "there is no part") {
		t.Errorf("%v / %+v", err, res)
	}
	// A line longer than a part is cut where it must be.
	long := read(t, "<html><body><p>"+strings.Repeat("é", 700)+"</p></body></html>", ReadOptions{PartChars: 300})
	if long.Parts != 3 || long.Chars != 300 || !utf8.ValidString(long.Text) {
		t.Errorf("parts %d chars %d valid %v", long.Parts, long.Chars, utf8.ValidString(long.Text))
	}
	// The part size is clamped to the range.
	small := read(t, page, ReadOptions{PartChars: 10})
	if small.Chars > MinPartChars {
		t.Errorf("chars %d over the floor", small.Chars)
	}
}

func TestReadEmptyPage(t *testing.T) {
	res := read(t, `<html><body><script>x()</script><img src="a.png"></body></html>`, ReadOptions{Part: 3})
	if res.Parts != 0 || res.Part != 0 || res.Text != "" || res.TotalChars != 0 {
		t.Errorf("%+v", res)
	}
}

func TestReadIsBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("<html><body>")
	for i := 0; i < 5000; i++ {
		b.WriteString("<p>some words here</p>")
	}
	b.WriteString("</body></html>")
	lim := DefaultLimits()
	lim.MaxReadBytes = 2000
	res, err := Read(context.Background(), []byte(b.String()), "text/html", ReadOptions{}, lim)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TotalChars > 2100 || res.Parts == 0 {
		t.Errorf("truncated %v total %d parts %d", res.Truncated, res.TotalChars, res.Parts)
	}
	lim = DefaultLimits()
	lim.MaxTextVisits = 100
	res, err = Read(context.Background(), []byte(b.String()), "text/html", ReadOptions{}, lim)
	if err != nil || !res.Truncated {
		t.Errorf("visits: %v %+v", err, res)
	}
	// A spent time budget is an error, as for Extract.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Read(ctx, []byte(b.String()), "text/html", ReadOptions{}, DefaultLimits())
	var he *Error
	if !errors.As(err, &he) || (he.Code != CodeTimeout && he.Code != CodeDocumentTooComplex) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestReadCharsetAndLinks(t *testing.T) {
	// Latin-1 declared in the header; a relative link resolved against the
	// page's address when there is no <base>.
	body := []byte("<html><body><p>caf\xe9 <a href='p/q'>here</a> <a href='mailto:a@b.c'>mail</a> <a href='ftp://x/y'>ftp</a></p></body></html>")
	res, err := Read(context.Background(), body, "text/html; charset=iso-8859-1", ReadOptions{BaseURL: "https://h.test/a/b"}, DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "café [here](https://h.test/a/p/q) [mail](mailto:a@b.c) ftp" || (res.Charset != "iso-8859-1" && res.Charset != "windows-1252") {
		t.Errorf("%q (%s)", res.Text, res.Charset)
	}
	// An empty link, or an empty emphasis, leaves nothing behind.
	res = read(t, `<html><body><p>a <a href="/x"></a> b <b></b> c <b> </b>d</p></body></html>`, ReadOptions{BaseURL: "https://h.test/"})
	if res.Text != "a b c d" {
		t.Errorf("%q", res.Text)
	}
}
