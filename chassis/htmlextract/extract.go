package htmlextract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// Result is one extraction: the data object, fields in spec order.
type Result struct {
	Data      json.RawMessage
	Truncated bool // a value was cut at MaxValueBytes, or text collection hit MaxTextVisits
	Nodes     int
	Depth     int
	Charset   string
}

// errBudget stops the parser's reads once the time budget is spent.
var errBudget = errors.New("htmlextract: time budget spent")

// Extract parses body (decoded to UTF-8 from the charset the header or
// the document declares) and evaluates every spec against it, within
// lim.MaxTime of CPU and ctx's deadline. Every phase checks the budget as
// it goes — the parser through the reader it pulls from, the walks every
// few thousand nodes — so nothing outlives it by more than a moment.
func Extract(ctx context.Context, body []byte, contentType string, specs []Spec, lim Limits) (*Result, error) {
	parent := ctx
	if lim.MaxTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lim.MaxTime)
		defer cancel()
	}
	spent := func() *Error {
		if parent.Err() != nil {
			return &Error{Code: CodeTimeout, Message: "the extraction did not finish before the op's deadline"}
		}
		return &Error{Code: CodeDocumentTooComplex, Message: fmt.Sprintf("the document took more than %s to process", lim.MaxTime)}
	}

	res := &Result{}
	_, name, certain := charset.DetermineEncoding(body, contentType)
	if !certain && name == "windows-1252" && utf8.Valid(body) {
		// Nothing declared, and the first KiB was plain ASCII: the
		// spec's windows-1252 fallback would garble a UTF-8 page.
		name = "utf-8"
	}
	if name != "" && name != "utf-8" {
		enc, _ := charset.Lookup(name)
		if enc != nil {
			decoded, err := enc.NewDecoder().Bytes(body)
			if err != nil {
				return nil, &Error{Code: CodeParseFailed, Message: "the document does not decode as " + name}
			}
			body = decoded
		}
		res.Charset = name
	} else {
		res.Charset = "utf-8"
	}

	_, clean, perr := prescan(ctx, body, lim)
	if perr != nil {
		if ctx.Err() != nil {
			return nil, spent()
		}
		return nil, perr
	}
	doc, err := html.Parse(&budgetReader{ctx: ctx, r: bytes.NewReader(clean)})
	if err != nil {
		if ctx.Err() != nil {
			return nil, spent()
		}
		// The parser refuses more than 512 open elements with an error
		// of its own; that is a complexity limit, not a broken document.
		if strings.Contains(err.Error(), "open stack of elements") {
			return nil, &Error{Code: CodeDocumentTooComplex, Message: "the document nests deeper than the parser allows (512)"}
		}
		return nil, &Error{Code: CodeParseFailed, Message: "the document could not be parsed"}
	}
	elems, nodes, depth, cerr := collect(ctx, doc, lim)
	if cerr != nil {
		if ctx.Err() != nil {
			return nil, spent()
		}
		return nil, cerr
	}
	res.Nodes, res.Depth = nodes, depth

	// The selector work is bounded before any of it runs: each field
	// costs one match per element per alternative, and a descendant
	// combinator walks up to `depth` ancestors.
	var weight int64
	for _, s := range specs {
		weight += int64(s.alts) + int64(s.desc)*int64(depth)
	}
	if int64(len(elems))*weight > lim.MaxWork {
		return nil, &Error{Code: CodeDocumentTooComplex, Message: fmt.Sprintf(
			"the document (%d elements, %d deep) is too large for these selectors; use fewer fields, or `>` in place of a descendant space", len(elems), depth)}
	}

	tw := &textWalker{ctx: ctx, max: lim.MaxValueBytes, visits: lim.MaxTextVisits}
	var out bytes.Buffer
	out.WriteByte('{')
	for i, s := range specs {
		if i > 0 {
			out.WriteByte(',')
		}
		name, _ := json.Marshal(s.Name)
		out.Write(name)
		out.WriteByte(':')
		var vals []string
		for k, e := range elems {
			if k&1023 == 0 && ctx.Err() != nil {
				return nil, spent()
			}
			if !s.group.Match(e) {
				continue
			}
			if s.Text {
				vals = append(vals, tw.text(e))
			} else if v, ok := attrOf(e, s.Attr); ok {
				vals = append(vals, v)
			} else {
				continue
			}
			if len(vals) >= s.Limit {
				break
			}
		}
		if ctx.Err() != nil {
			return nil, spent()
		}
		for j, v := range vals {
			v = strings.ToValidUTF8(v, "\uFFFD")
			if len(v) > lim.MaxValueBytes {
				v = cut(v, lim.MaxValueBytes)
				res.Truncated = true
			}
			vals[j] = v
		}
		var b []byte
		switch {
		case s.All:
			if vals == nil {
				vals = []string{}
			}
			b, _ = json.Marshal(vals)
		case len(vals) == 0:
			b = []byte("null")
		default:
			b, _ = json.Marshal(vals[0])
		}
		out.Write(b)
		if out.Len() > lim.MaxOutputBytes {
			return nil, &Error{Code: CodeOutputLimit, Message: fmt.Sprintf(
				"the extracted values are larger than %d bytes; narrow the selectors or lower their limits", lim.MaxOutputBytes)}
		}
	}
	out.WriteByte('}')
	if tw.exhausted {
		res.Truncated = true
	}
	res.Data = out.Bytes()
	return res, nil
}

// budgetReader feeds the parser 4 KiB at a time and stops it once ctx is
// done. The parser can't be interrupted otherwise, and some documents
// cost it far more than their size (500 open elements make every later
// tag walk the stack: 3 MiB took 1.9 s in bench_test.go); the small reads
// keep it from swallowing a large part of the input between checks.
type budgetReader struct {
	ctx context.Context
	r   io.Reader
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.ctx.Err() != nil {
		return 0, errBudget
	}
	if len(p) > 4096 {
		p = p[:4096]
	}
	return b.r.Read(p)
}

// formatting are the elements the HTML parser keeps in its list of
// active formatting elements, and recreates ("reconstructs") inside each
// new block while they remain unclosed.
var formatting = map[atom.Atom]bool{
	atom.A: true, atom.B: true, atom.Big: true, atom.Code: true, atom.Em: true, atom.Font: true,
	atom.I: true, atom.Nobr: true, atom.S: true, atom.Small: true, atom.Strike: true,
	atom.Strong: true, atom.Tt: true, atom.U: true,
}

// markers push a marker onto that list: reconstruction never reaches
// past one, and closing the element clears the list back to it. td, th
// and caption only count inside a table (elsewhere the parser ignores
// them).
var markers = map[atom.Atom]bool{
	atom.Applet: true, atom.Marquee: true, atom.Object: true, atom.Template: true,
	atom.Td: true, atom.Th: true, atom.Caption: true,
}

var tableMarker = map[atom.Atom]bool{atom.Td: true, atom.Th: true, atom.Caption: true}

// level is one marker's stretch of the active formatting list.
type level struct {
	opener atom.Atom
	total  int
	byName map[atom.Atom]int
}

// prescan runs the linear tokenizer over the document before the tree
// builder sees it, and refuses a document the builder would make too
// big. It counts tokens and estimates the nodes (and attributes) the
// builder will create.
// (Depth needs no pre-pass: the parser refuses more than 512 open
// elements itself, which also bounds its own walks of that stack.)
//
// The estimate is what makes the builder safe to run. The HTML parsing
// algorithm recreates every unclosed formatting element (b, i, a, font…)
// inside each new block while it stays unclosed, so a few hundred
// unclosed <b>s followed by paragraphs grow the tree quadratically: 28 KB
// of input became 1.5M nodes and 229 MB in a measurement. Each token
// triggers at most one reconstruction, of at most the formatting elements
// still in the parser's list, so the estimate adds that count for every
// text and start-tag token: an upper bound on reconstructed nodes. (The
// builder's other additions — implied html/head/body/tbody, the adoption
// agency's clones on a misnested end tag — are linear in the tokens, and
// the token cap bounds them.)
//
// The count errs high, never low: it sums the list across every marker
// (td, th, object…) rather than only past the last one, and forgets a
// marker's entries only when the element that opened it closes. Markers
// are not opened inside svg or math, where those names are not HTML.
// FuzzPrescanBoundsTheTree checks the bound against the real parser.
//
// The parser never sees the original bytes: it parses the token stream
// prescan re-renders (text escaped, comments dropped). A standalone
// tokenizer can't tell, as the parser can, that <iframe> inside <svg> is
// not raw text, so on the original bytes the two would disagree about
// which tags exist, and a document could hide tags from the estimate.
// Re-rendered, every tag the parser reads is one prescan counted, and
// raw text holds no '<' for either to read differently.
// scanStats are prescan's counts, for tests.
type scanStats struct {
	tokens, estimate int
	adoptions        int // formatting end tags, and <a>/<nobr> while one is open: each may clone up to 32 nodes
}

func prescan(ctx context.Context, body []byte, lim Limits) (scanStats, []byte, *Error) {
	var st scanStats
	var out bytes.Buffer
	out.Grow(len(body))
	z := html.NewTokenizer(bytes.NewReader(body))
	tokens, tables, foreign := 0, 0, 0
	estimate := 4 // the document, html, head, body
	open := 0     // formatting elements in the list, all levels
	levels := []*level{{byName: map[atom.Atom]int{}}}
	tooComplex := func(format string, args ...any) *Error {
		return &Error{Code: CodeDocumentTooComplex, Message: fmt.Sprintf(format, args...)}
	}
	closeLevel := func() {
		top := levels[len(levels)-1]
		open -= top.total
		levels = levels[:len(levels)-1]
	}
	for {
		tt := z.Next()
		st.tokens, st.estimate = tokens, estimate
		if tt == html.ErrorToken {
			return st, out.Bytes(), nil // io.EOF, or a tokenizer error the parser would meet too
		}
		tokens++
		if tokens > lim.MaxTokens {
			return st, nil, tooComplex("the document has more than %d tokens", lim.MaxTokens)
		}
		if tokens&4095 == 0 && ctx.Err() != nil {
			return st, nil, tooComplex("the document took too long to scan")
		}
		tok := z.Token()
		top := levels[len(levels)-1]
		switch tt {
		case html.TextToken:
			out.WriteString(html.EscapeString(tok.Data))
			// Up to two nodes: the parser can split off leading
			// whitespace into a node of its own.
			estimate += 2 + open
		case html.CommentToken:
			// dropped: never extracted, and one less thing to re-render
		case html.DoctypeToken:
			out.WriteString(tok.String())
			estimate++
		case html.StartTagToken, html.SelfClosingTagToken:
			if len(tok.Attr) > lim.MaxAttrs {
				return st, nil, tooComplex("an element has more than %d attributes", lim.MaxAttrs)
			}
			out.WriteString(tok.String())
			a := tok.DataAtom
			// The element, its attributes (they cost about a node's memory
			// each), and a reconstruction of every open formatting element.
			estimate += 1 + len(tok.Attr) + open
			switch a {
			case atom.Td, atom.Th:
				estimate += 2 // an implied tbody and tr
			case atom.Tr, atom.Col:
				estimate++ // an implied tbody or colgroup
			case atom.A, atom.Nobr:
				if top.byName[a] > 0 {
					st.adoptions++
				}
			}
			switch {
			case a == atom.Svg || a == atom.Math:
				if tt == html.StartTagToken {
					foreign++
				}
			case a == atom.Table:
				tables++
			case formatting[a]:
				top.byName[a]++
				top.total++
				open++
			case markers[a] && foreign == 0 && (!tableMarker[a] || tables > 0):
				// A new cell closes the open one, clearing its entries.
				if (a == atom.Td || a == atom.Th) && (top.opener == atom.Td || top.opener == atom.Th) {
					closeLevel()
				}
				if len(levels) > lim.MaxDepth {
					return st, nil, tooComplex("the document nests deeper than %d", lim.MaxDepth)
				}
				levels = append(levels, &level{opener: a, byName: map[atom.Atom]int{}})
			}
		case html.EndTagToken:
			out.WriteString(tok.String())
			a := tok.DataAtom
			switch {
			case a == atom.P || a == atom.Br:
				estimate++ // a stray </p> or </br> inserts an element
			case (a == atom.Svg || a == atom.Math) && foreign > 0:
				foreign--
			case a == atom.Table && tables > 0:
				tables--
			case formatting[a]:
				st.adoptions++
				if top.byName[a] > 0 {
					top.byName[a]--
					top.total--
					open--
				}
			case len(levels) > 1 && top.opener == a:
				// Closing the marker's element clears the list back to it.
				closeLevel()
			}
		}
		if estimate > lim.MaxBuildNodes {
			return st, nil, tooComplex("the document would build more than %d nodes", lim.MaxBuildNodes)
		}
	}
}

// collect walks the parsed tree once, without recursion: every element
// in document order, plus the node count and depth, against the caps.
func collect(ctx context.Context, doc *html.Node, lim Limits) ([]*html.Node, int, int, *Error) {
	type item struct {
		n *html.Node
		d int
	}
	var elems []*html.Node
	nodes, maxDepth := 0, 0
	stack := []item{{doc, 0}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		nodes++
		if nodes&4095 == 0 && ctx.Err() != nil {
			return nil, 0, 0, &Error{Code: CodeDocumentTooComplex, Message: "the document took too long to walk"}
		}
		if nodes > lim.MaxNodes {
			return nil, 0, 0, &Error{Code: CodeDocumentTooComplex, Message: fmt.Sprintf("the document has more than %d nodes", lim.MaxNodes)}
		}
		if it.d > maxDepth {
			maxDepth = it.d
			if maxDepth > lim.MaxDepth {
				return nil, 0, 0, &Error{Code: CodeDocumentTooComplex, Message: fmt.Sprintf("the document nests deeper than %d", lim.MaxDepth)}
			}
		}
		if it.n.Type == html.ElementNode {
			if len(it.n.Attr) > lim.MaxAttrs {
				return nil, 0, 0, &Error{Code: CodeDocumentTooComplex, Message: fmt.Sprintf("an element has more than %d attributes", lim.MaxAttrs)}
			}
			elems = append(elems, it.n)
		}
		// Push children last-first so they pop in document order.
		for c := it.n.LastChild; c != nil; c = c.PrevSibling {
			stack = append(stack, item{c, it.d + 1})
		}
	}
	return elems, nodes, maxDepth, nil
}

func attrOf(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key || (a.Namespace != "" && a.Namespace+":"+a.Key == key) {
			return a.Val, true
		}
	}
	return "", false
}

// skipText are elements whose content is never text a reader sees.
var skipText = map[atom.Atom]bool{atom.Script: true, atom.Style: true, atom.Template: true, atom.Noscript: true}

// textWalker collects an element's text with whitespace collapsed,
// stopping a little past max bytes (the caller cuts and flags it) and
// sharing one visit budget across the whole extraction.
type textWalker struct {
	ctx       context.Context
	max       int
	visits    int
	exhausted bool
}

func (tw *textWalker) text(n *html.Node) string {
	var b strings.Builder
	space := false
	stack := []*html.Node{n}
	for len(stack) > 0 && b.Len() <= tw.max {
		if tw.visits <= 0 || (tw.visits&1023 == 0 && tw.ctx.Err() != nil) {
			tw.exhausted = true
			break
		}
		tw.visits--
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch c.Type {
		case html.TextNode:
			for _, r := range c.Data {
				if unicode.IsSpace(r) {
					space = true
					continue
				}
				if space && b.Len() > 0 {
					b.WriteByte(' ')
				}
				space = false
				b.WriteRune(r)
			}
			continue
		case html.ElementNode:
			if c != n && skipText[c.DataAtom] {
				continue
			}
		case html.CommentNode, html.DoctypeNode:
			continue
		}
		for k := c.LastChild; k != nil; k = k.PrevSibling {
			stack = append(stack, k)
		}
	}
	return b.String()
}

// cut shortens s to at most n bytes at a UTF-8 boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
