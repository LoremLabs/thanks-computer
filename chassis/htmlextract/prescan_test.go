package htmlextract

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// countNodes counts a parsed tree without recursion.
func countNodes(doc *html.Node) int {
	n := 0
	stack := []*html.Node{doc}
	for len(stack) > 0 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		n++
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			stack = append(stack, k)
		}
	}
	return n
}

// fuzzTokens is the alphabet the fuzzer composes documents from: the
// elements whose handling makes the parser add nodes of its own
// (formatting elements, markers, tables, foreign content, implied
// closes), plus text.
var fuzzTokens = []string{
	"x", " ", "<p>", "</p>", "<div>", "</div>", "<span>", "</span>",
	"<b>", "</b>", "<i>", "</i>", "<a>", "</a>", "<font>", "</font>", "<nobr>", "</nobr>",
	"<em>", "</em>", "<strong>", "</strong>", "<u>", "</u>", "<code>", "</code>",
	"<table>", "</table>", "<tr>", "</tr>", "<td>", "</td>", "<th>", "</th>", "<tbody>", "</tbody>",
	"<caption>", "</caption>", "<col>", "<colgroup>",
	"<object>", "</object>", "<applet>", "</applet>", "<marquee>", "</marquee>", "<template>", "</template>",
	"<svg>", "</svg>", "<math>", "</math>", "<foreignObject>", "</foreignObject>", "<mi>", "<mtext>",
	"<li>", "</li>", "<ul>", "</ul>", "<button>", "</button>", "<form>", "</form>", "<h1>", "</h1>",
	"<select>", "</select>", "<option>", "<optgroup>", "<frameset>", "<body>", "</body>", "<html>",
	"<head>", "</head>", "<br>", "</br>", "<hr>", "<img>", "<image>", "<pre>", "<textarea>", "</textarea>",
	"<dd>", "<dt>", "<dl>", "</dl>", "<address>", "<blockquote>", "</blockquote>", "<center>",
	"<input>", "<isindex>", "<plaintext>", "<xmp>", "<noscript>", "<iframe>", "<!-- c -->",
}

// render turns fuzz bytes into a document: each byte picks a token, and
// formatting start tags get a distinct attribute so the parser's
// three-identical-elements rule (which would hide a blow-up) never
// applies.
func render(data []byte) string {
	var b strings.Builder
	for i, c := range data {
		t := fuzzTokens[int(c)%len(fuzzTokens)]
		if strings.HasPrefix(t, "<") && !strings.HasPrefix(t, "</") && len(t) <= 8 && c%7 == 0 {
			t = fmt.Sprintf("%s k=%d>", strings.TrimSuffix(t, ">"), i)
		}
		b.WriteString(t)
	}
	return b.String()
}

// The prescan's estimate, within a constant factor plus the adoption
// agency's allowance, bounds the nodes the real parser creates. A shape
// the estimate misses would let a small document grow the tree past
// every cap.
func FuzzPrescanBoundsTheTree(f *testing.F) {
	seeds := []string{
		"",
		strings.Repeat("\x08", 40) + strings.Repeat("\x02x\x03", 40), // <b k>… then <p>x</p>
		"\x1a\x1e" + strings.Repeat("\x08", 30) + "\x1f" + strings.Repeat("\x02x", 30),
		"\x30" + strings.Repeat("\x08", 20) + "\x31" + strings.Repeat("\x02x\x03", 30),
		"\x34" + strings.Repeat("\x08", 20) + "\x35" + strings.Repeat("\x02x\x03", 30),
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	lim := DefaultLimits()
	lim.MaxBuildNodes = 1 << 30
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4000 {
			return
		}
		doc := render(data)
		st, clean, err := prescan(context.Background(), []byte(doc), lim)
		if err != nil {
			return // refused before parsing: nothing was built
		}
		tree, perr := html.Parse(bytes.NewReader(clean))
		if perr != nil {
			return
		}
		nodes := countNodes(tree)
		// Twice the estimate absorbs the parser's constant-factor extras
		// (implied elements in rare insertion modes); what this hunts is
		// growth the estimate misses, which outruns any constant.
		if allowed := 2*st.estimate + 32*st.adoptions + 16; nodes > allowed {
			t.Fatalf("parser built %d nodes, estimate %d, adoption allowance %d (%d tokens)\n%s",
				nodes, st.estimate, 32*st.adoptions, st.tokens, doc)
		}
	})
}

// The reconstruction blow-up is refused before the parser runs, whether
// the blocks close explicitly or implicitly.
func TestPrescanRefusesReconstructionBlowups(t *testing.T) {
	var open strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&open, "<b a=%d>", i)
	}
	for name, doc := range map[string]string{
		"explicit </p>":  "<p>" + open.String() + "</p>" + strings.Repeat("<p>x</p>", 2000),
		"implicit <p>":   "<p>" + open.String() + strings.Repeat("<p>x", 2000),
		"divs":           "<div>" + open.String() + "</div>" + strings.Repeat("<div>x</div>", 2000),
		"list items":     "<ul><li>" + open.String() + strings.Repeat("<li>x", 2000),
		"buttons":        "<button>" + open.String() + strings.Repeat("<button>x", 2000),
		"after a table":  "<p>" + open.String() + "</p><table><caption><td>x</td></table>" + strings.Repeat("<p>x</p>", 2000),
		"after svg":      "<p>" + open.String() + "</p><svg><object></svg>" + strings.Repeat("<p>x</p>", 2000),
		"after template": "<p>" + open.String() + "</p><template></template>" + strings.Repeat("<p>x</p>", 2000),
	} {
		_, err := Extract(context.Background(), []byte(doc), "text/html", specs(t, `{"t": {"selector": "title", "text": true}}`), DefaultLimits())
		if e := wantCode(t, err, CodeDocumentTooComplex); !strings.Contains(e.Message, "nodes") && !strings.Contains(e.Message, "deeper") {
			t.Errorf("%s: %s", name, e.Message)
		}
	}
}

// Ordinary markup is nowhere near the estimate's limit: formatting that
// closes, cells with their own formatting, a page wrapped in <font>.
func TestPrescanAdmitsOrdinaryPages(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<html><head><title>t</title></head><body><font face="x">`)
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, `<p>Some <b>bold</b> and <a href="/%d">a <i>link</i></a>.</p>`, i)
	}
	b.WriteString("<table>")
	for i := 0; i < 2000; i++ {
		b.WriteString("<tr><td><font>cell<td><b>cell</b></tr>")
	}
	b.WriteString("</table></font></body></html>")
	if _, err := Extract(context.Background(), []byte(b.String()), "text/html", specs(t, `{"t": {"selector": "title", "text": true}}`), DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}
