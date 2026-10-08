package htmlextract

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Adversarial and ordinary documents of about 3 MiB (the fetch cap),
// each run against the link-preview spec and a wide 20-field spec. Run:
//
//	go test ./chassis/htmlextract -run '^$' -bench . -benchmem
//
// The numbers set DefaultLimits; docs/advanced/html-extract.md records
// them.
const benchBytes = 3 << 20

func fill(unit func(i int) string) string {
	var b strings.Builder
	for i := 0; b.Len() < benchBytes; i++ {
		b.WriteString(unit(i))
	}
	return b.String()
}

var benchDocs = map[string]string{
	// An ordinary large page: head metadata, then article markup.
	"ordinary": `<!doctype html><html><head><title>A page</title>` +
		`<meta property="og:title" content="A page"><meta name="description" content="About it">` +
		`<link rel="icon" href="/favicon.ico"></head><body>` +
		fill(func(i int) string {
			return fmt.Sprintf(`<article class="post"><h2>Post %d</h2><p>Some <b>bold</b> and <a href="/p/%d">a link</a>.</p><ul><li>one<li>two</ul></article>`, i, i)
		}),
	// Mostly one inline script, like a large app shell.
	"script-heavy": `<html><head><title>App</title><script>` + strings.Repeat("var a = '<div>' + 1;\n", benchBytes/22) + `</script></head><body></body></html>`,
	// The parser's own ceiling: 500 open elements, then siblings, so
	// every start tag's scope check walks a deep stack.
	"deep-stack": strings.Repeat("<div>", 500) + fill(func(int) string { return "<div></div>" }),
	// One flat run of elements.
	"wide": "<body>" + fill(func(i int) string { return "<i>x</i>" }),
	// Misnested formatting across blocks (the adoption agency).
	"misnest": fill(func(i int) string { return "<b>1<p>2</b>3</p>" }),
	// The reconstruction blow-up: refused by the prescan.
	"reconstruct": "<p>" + fill(func(i int) string {
		if i < 400 {
			return fmt.Sprintf("<b a=%d>", i)
		}
		if i == 400 {
			return "</p>"
		}
		return "<p>x</p>"
	}),
	// Elements carrying the most attributes allowed.
	"attr-flood": fill(func(i int) string {
		var b strings.Builder
		b.WriteString("<div")
		for k := 0; k < 250; k++ {
			fmt.Fprintf(&b, " a%d", k)
		}
		b.WriteString(">x</div>")
		return b.String()
	}),
	// Comments between every element (adjacent-sibling walks skip them).
	"comments": fill(func(int) string { return "<i>x</i><!-- c --><!-- c --><!-- c -->" }),
}

const previewSpec = `{
	"title":       {"selector": "title", "text": true},
	"description": {"selector": "meta[name=description]", "attr": "content"},
	"og_title":    {"selector": "meta[property='og:title']", "attr": "content"},
	"og_image":    {"selector": "meta[property='og:image']", "attr": "content"},
	"icon":        {"selector": "link[rel~=icon]", "attr": "href"},
	"canonical":   {"selector": "link[rel=canonical]", "attr": "href"},
	"base":        {"selector": "base[href]", "attr": "href"}
}`

func wideSpec() string {
	var f []string
	for i := 0; i < 20; i++ {
		f = append(f, fmt.Sprintf(`"f%d": {"selector": "body div:not(.x%d) + i, h2, a[href^='/p']", "text": true, "all": true, "limit": 50}`, i, i))
	}
	return "{" + strings.Join(f, ",") + "}"
}

func BenchmarkExtract(b *testing.B) {
	lim := DefaultLimits()
	for _, spec := range []struct{ name, raw string }{{"preview", previewSpec}, {"wide20", wideSpec()}} {
		sp, err := ParseSpecs(spec.raw, lim)
		if err != nil {
			b.Fatal(err)
		}
		for _, name := range []string{"ordinary", "script-heavy", "deep-stack", "wide", "misnest", "reconstruct", "attr-flood", "comments"} {
			doc := []byte(benchDocs[name])
			b.Run(spec.name+"/"+name, func(b *testing.B) {
				b.SetBytes(int64(len(doc)))
				var outcome string
				for i := 0; i < b.N; i++ {
					_, err := Extract(context.Background(), doc, "text/html", sp, lim)
					outcome = "ok"
					if e, ok := err.(*Error); ok {
						outcome = e.Code
					}
				}
				b.ReportMetric(0, outcome)
			})
		}
	}
}
