package htmlextract

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Read renders a page, or the element a selector picks out of it, as
// Markdown, and answers one part of that text (txco://html/read). The
// same bounded parse as Extract; the rendering is bounded by
// lim.MaxTextVisits (nodes walked), lim.MaxReadBytes (text rendered) and
// lim.MaxTime, and the text is cut into parts of at most opts.PartChars
// characters at line ends, so a long page is read in turns.
//
// What the Markdown keeps: headings, paragraphs, lists, links as
// [words](address) resolved against the page's address, emphasis, inline
// code and code blocks, block quotes, tables (one row a line), definition
// lists, horizontal rules, and an image's alt text. Script, style,
// template, noscript, forms' controls, embedded media and anything marked
// hidden are left out, and navigation (nav, aside) wherever it sits. When
// the page marks no main content (no <main>, no <article>) and the whole
// body is read, its header and footer are left out too.

// DefaultPartChars is the part size when the caller names none: what one
// turn of a model's loop comfortably takes.
const DefaultPartChars = 3000

// MaxPartChars is the largest part a caller may ask for.
const MaxPartChars = 20000

// MinPartChars is the smallest.
const MinPartChars = 200

// ReadOptions say what to read and which part to answer.
type ReadOptions struct {
	// Root is a CSS selector naming the element to read (the first match
	// in document order). Empty: <main>, else <article>, else <body>.
	Root string
	// Part is which part of the text to answer, from 1 (0 is 1); Last
	// asks for the last part instead.
	Part int
	Last bool
	// PartChars is the most characters in one part. 0 is DefaultPartChars.
	PartChars int
	// BaseURL is the page's own address (after redirects), for resolving
	// the links the text keeps. A <base href> in the page refines it.
	BaseURL string
}

// ReadResult is one read: the part asked for and what the page is.
type ReadResult struct {
	Title      string // the page's <title>, whitespace collapsed
	Root       string // what was read: "main", "article", "body", "document", or the selector
	Part       int    // the part answered (0 when the page has no text)
	Parts      int    // how many parts the text has
	Text       string // the part
	Chars      int    // characters in the part
	TotalChars int    // characters in the whole text
	Truncated  bool   // the rendering stopped at a cap: the parts are of what was rendered
	Nodes      int
	Depth      int
	Charset    string
}

// Read parses body and renders the root as Markdown within lim, then
// answers the part opts asks for. A part past the last is CodeNoSuchPart,
// with the result's Parts filled in so the caller can say how many there
// are. A root selector that matches nothing is CodeRootNotFound.
func Read(ctx context.Context, body []byte, contentType string, opts ReadOptions, lim Limits) (*ReadResult, error) {
	parent := ctx
	if lim.MaxTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lim.MaxTime)
		defer cancel()
	}
	spent := spentError(parent, lim, "the read")
	group, err := ParseRootSelector(opts.Root, lim)
	if err != nil {
		return nil, err
	}
	partChars := opts.PartChars
	if partChars <= 0 {
		partChars = DefaultPartChars
	}
	if partChars < MinPartChars {
		partChars = MinPartChars
	}
	if partChars > MaxPartChars {
		partChars = MaxPartChars
	}
	if opts.Part < 0 {
		return nil, &Error{Code: CodeInvalidRequest, Message: "`part` must be a whole number from 1, or \"last\""}
	}

	doc, elems, perr := parseDocument(ctx, parent, body, contentType, lim, "the read")
	if perr != nil {
		return nil, perr
	}
	res := &ReadResult{Nodes: doc.Nodes, Depth: doc.Depth, Charset: doc.Charset}

	// The base for links: the page's address, refined by its <base href>.
	var base *url.URL
	if opts.BaseURL != "" {
		base, _ = url.Parse(opts.BaseURL)
	}
	var root *html.Node
	for _, e := range elems {
		switch e.DataAtom {
		case atom.Title:
			if res.Title == "" {
				res.Title = cut(collapsedText(ctx, e, 4096), 300)
			}
		case atom.Base:
			if v, ok := attrOf(e, "href"); ok && base != nil {
				if u, err := url.Parse(strings.TrimSpace(v)); err == nil {
					base = base.ResolveReference(u)
				}
			}
		}
	}
	if group != nil {
		for _, e := range elems {
			if group.Match(e) {
				root = e
				break
			}
		}
		if root == nil {
			return nil, &Error{Code: CodeRootNotFound, Message: "no element matches `root`; leave it out to read the page's main content"}
		}
		res.Root = strings.TrimSpace(opts.Root)
	} else {
		for _, want := range []atom.Atom{atom.Main, atom.Article, atom.Body} {
			for _, e := range elems {
				if e.DataAtom == want {
					root = e
					res.Root = want.String()
					break
				}
			}
			if root != nil {
				break
			}
		}
		if root == nil {
			root = doc.Root
			res.Root = "document"
		}
	}

	w := &mdWriter{
		ctx:          ctx,
		max:          lim.MaxReadBytes,
		visits:       lim.MaxTextVisits,
		base:         base,
		bodyFallback: res.Root == "body" || res.Root == "document",
	}
	w.render(root)
	if w.timedOut {
		return nil, spent()
	}
	res.Truncated = w.exhausted
	text := finalize(w.buf.String())
	res.TotalChars = utf8.RuneCountInString(text)
	parts := splitParts(text, partChars)
	res.Parts = len(parts)
	if len(parts) == 0 {
		return res, nil
	}
	part := opts.Part
	if part < 1 {
		part = 1
	}
	if opts.Last {
		part = len(parts)
	}
	if part > len(parts) {
		return res, &Error{Code: CodeNoSuchPart, Message: fmt.Sprintf("the page has %d part%s; there is no part %d", len(parts), plural(len(parts)), part)}
	}
	res.Part, res.Text = part, parts[part-1]
	res.Chars = utf8.RuneCountInString(res.Text)
	return res, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// skipRead are elements whose content a reader never sees.
var skipRead = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Template: true, atom.Noscript: true, atom.Head: true,
	atom.Title: true, atom.Meta: true, atom.Link: true, atom.Base: true,
	atom.Svg: true, atom.Math: true, atom.Iframe: true, atom.Object: true, atom.Embed: true, atom.Canvas: true,
	atom.Video: true, atom.Audio: true, atom.Map: true,
	atom.Select: true, atom.Textarea: true, atom.Input: true, atom.Button: true,
}

// navigation is never a page's content: left out wherever it is (a site's
// tool menus sit inside <main> as often as beside it). A header or a
// footer is left out only when the page marks no main content and the
// whole body is read: inside an article they hold its title and byline.
var navigation = map[atom.Atom]bool{atom.Nav: true, atom.Aside: true}

var navigationRoles = map[string]bool{"navigation": true, "complementary": true}

var furniture = map[atom.Atom]bool{atom.Header: true, atom.Footer: true}

var furnitureRoles = map[string]bool{"banner": true, "contentinfo": true}

// paragraphs are the block elements a blank line separates.
var paragraphs = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Section: true, atom.Article: true, atom.Main: true,
	atom.Header: true, atom.Footer: true, atom.Nav: true, atom.Aside: true, atom.Figure: true,
	atom.Figcaption: true, atom.Details: true, atom.Summary: true, atom.Form: true, atom.Fieldset: true,
	atom.Address: true, atom.Dl: true, atom.Body: true, atom.Html: true, atom.Center: true,
}

type listState struct {
	ordered bool
	n       int
}

// mdWriter renders a tree as Markdown. Whitespace is collapsed as a
// browser collapses it; line breaks are owed (`pending`) and settled when
// the next word arrives, so blocks never leave stray blank lines.
type mdWriter struct {
	ctx          context.Context
	buf          bytes.Buffer
	max          int
	visits       int
	pending      int    // newlines owed before the next word: 0, 1 or 2
	atStart      bool   // nothing written on this line yet
	blank        int    // empty lines at the end, the current one included
	space        bool   // one collapsed space owed before the next word
	prefix       string // written after every newline (block quotes)
	exhausted    bool   // a cap stopped the rendering
	timedOut     bool
	base         *url.URL
	bodyFallback bool
	lists        []listState
}

func (w *mdWriter) brk(n int) {
	if n > w.pending {
		w.pending = n
	}
}

func (w *mdWriter) newline() {
	if w.atStart {
		w.blank++
	} else {
		w.blank = 1
	}
	w.buf.WriteByte('\n')
	w.buf.WriteString(w.prefix)
	w.atStart = true
	w.space = false
}

// flush settles owed line breaks before something is written: as many
// newlines as are owed beyond the empty lines already there.
func (w *mdWriter) flush() {
	if w.pending == 0 {
		return
	}
	if w.buf.Len() > 0 {
		for i := w.blank; i < w.pending; i++ {
			w.newline()
		}
	} else if w.prefix != "" {
		w.buf.WriteString(w.prefix)
	}
	w.pending = 0
	w.atStart = true
	w.space = false
}

// wrote notes that the current line has content.
func (w *mdWriter) wrote() {
	w.atStart = false
	w.blank = 0
}

// text writes words with their whitespace collapsed.
func (w *mdWriter) text(s string) {
	for _, r := range s {
		if unicode.IsSpace(r) {
			w.space = true
			continue
		}
		w.flush()
		if w.space && !w.atStart {
			w.buf.WriteByte(' ')
		}
		w.space = false
		w.buf.WriteRune(r)
		w.wrote()
	}
}

// pre writes text as it is, line breaks included.
func (w *mdWriter) pre(s string) {
	w.flush()
	for _, r := range s {
		if r == '\n' {
			w.newline()
			continue
		}
		w.buf.WriteRune(r)
		w.wrote()
	}
	w.space = false
}

// open writes a mark that begins something (a heading's #, a list
// marker, "[", "**"): an owed space goes before it.
func (w *mdWriter) open(s string) {
	w.flush()
	if w.space && !w.atStart {
		w.buf.WriteByte(' ')
	}
	w.space = false
	w.buf.WriteString(s)
	w.wrote()
}

// close writes a mark that ends something ("**", "](…)"): it sits against
// the last word, and a space owed before it is owed after it instead.
func (w *mdWriter) close(s string) {
	owed := w.space
	w.space = false
	w.flush()
	w.buf.WriteString(s)
	w.wrote()
	w.space = owed
}

// wrap renders n's children between two marks, and leaves nothing when
// they rendered nothing.
func (w *mdWriter) wrap(n *html.Node, o, c string) {
	w.open(o)
	mark := w.buf.Len()
	w.children(n)
	if w.buf.Len() == mark {
		w.unwrite(mark - len(o))
		return
	}
	w.close(c)
}

// unwrite drops what was written past n, and the space before it, which
// becomes owed again.
func (w *mdWriter) unwrite(n int) {
	w.buf.Truncate(n)
	if n > 0 && w.buf.Bytes()[n-1] == ' ' {
		w.buf.Truncate(n - 1)
		w.space = true
	}
	w.atStart = n == 0
}

func (w *mdWriter) children(n *html.Node) {
	for c := n.FirstChild; c != nil && !w.exhausted; c = c.NextSibling {
		w.render(c)
	}
}

func (w *mdWriter) render(n *html.Node) {
	if w.exhausted {
		return
	}
	w.visits--
	if w.visits <= 0 || w.buf.Len() > w.max {
		w.exhausted = true
		return
	}
	if w.visits&1023 == 0 && w.ctx.Err() != nil {
		w.exhausted, w.timedOut = true, true
		return
	}
	switch n.Type {
	case html.TextNode:
		w.text(n.Data)
	case html.ElementNode:
		w.element(n)
	case html.DocumentNode:
		w.children(n)
	}
}

func (w *mdWriter) skipped(n *html.Node) bool {
	if skipRead[n.DataAtom] || navigation[n.DataAtom] {
		return true
	}
	if w.bodyFallback && furniture[n.DataAtom] {
		return true
	}
	for _, a := range n.Attr {
		switch a.Key {
		case "hidden":
			return true
		case "aria-hidden":
			if strings.EqualFold(strings.TrimSpace(a.Val), "true") {
				return true
			}
		case "role":
			role := strings.ToLower(strings.TrimSpace(a.Val))
			if navigationRoles[role] || (w.bodyFallback && furnitureRoles[role]) {
				return true
			}
		}
	}
	return false
}

func (w *mdWriter) element(n *html.Node) {
	if w.skipped(n) {
		return
	}
	a := n.DataAtom
	switch {
	case a == atom.H1 || a == atom.H2 || a == atom.H3 || a == atom.H4 || a == atom.H5 || a == atom.H6:
		level := int(n.Data[1] - '0')
		w.brk(2)
		w.open(strings.Repeat("#", level) + " ")
		mark := w.buf.Len()
		w.children(n)
		if w.buf.Len() == mark {
			w.unwrite(mark - level - 1)
		}
		w.brk(2)
	case paragraphs[a]:
		w.brk(2)
		w.children(n)
		w.brk(2)
	case a == atom.Br:
		if w.pending >= 1 {
			w.pending = 2
		} else {
			w.pending = 1
		}
	case a == atom.Hr:
		w.brk(2)
		w.open("---")
		w.brk(2)
	case a == atom.Ul || a == atom.Ol:
		gap := 2
		if len(w.lists) > 0 {
			gap = 1
		}
		st := listState{ordered: a == atom.Ol, n: 0}
		if a == atom.Ol {
			if v, ok := attrOf(n, "start"); ok {
				if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s > 0 {
					st.n = s - 1
				}
			}
		}
		w.brk(gap)
		w.lists = append(w.lists, st)
		w.children(n)
		w.lists = w.lists[:len(w.lists)-1]
		w.brk(gap)
	case a == atom.Li:
		depth := len(w.lists)
		marker := "- "
		if depth == 0 {
			depth = 1
		} else {
			st := &w.lists[depth-1]
			if st.ordered {
				st.n++
				marker = strconv.Itoa(st.n) + ". "
			}
		}
		w.brk(1)
		w.open(strings.Repeat("  ", depth-1) + marker)
		w.children(n)
		w.brk(1)
	case a == atom.Blockquote:
		// The blank line before the quote is the outer text's; the
		// quote's own lines start with its prefix.
		w.brk(2)
		w.flush()
		old := w.prefix
		w.prefix += "> "
		w.buf.WriteString("> ")
		w.atStart = true
		if w.blank == 0 {
			w.blank = 1
		}
		w.children(n)
		w.prefix = old
		w.brk(2)
	case a == atom.Pre:
		w.brk(2)
		w.open("```")
		w.brk(1)
		w.pre(rawText(n))
		w.brk(1)
		w.open("```")
		w.brk(2)
	case a == atom.Code || a == atom.Kbd || a == atom.Samp:
		w.wrap(n, "`", "`")
	case a == atom.Strong || a == atom.B:
		w.wrap(n, "**", "**")
	case a == atom.Em || a == atom.I || a == atom.Cite || a == atom.Dfn:
		w.wrap(n, "_", "_")
	case a == atom.A:
		if href := w.href(n); href != "" {
			w.wrap(n, "[", "]("+href+")")
		} else {
			w.children(n)
		}
	case a == atom.Img:
		if alt, ok := attrOf(n, "alt"); ok {
			w.text(alt)
		}
	case a == atom.Table:
		w.table(n)
	case a == atom.Dt:
		w.brk(1)
		w.children(n)
		w.brk(1)
	case a == atom.Dd:
		w.brk(1)
		w.open(": ")
		w.children(n)
		w.brk(1)
	case a == atom.Tr:
		w.brk(1)
		w.children(n)
		w.brk(1)
	case a == atom.Td || a == atom.Th:
		w.space = true
		w.children(n)
		w.space = true
	default:
		w.children(n)
	}
}

// href is the address a link keeps: resolved against the page's, http(s)
// or mailto, and safe inside Markdown's parentheses. "" drops the link
// and keeps its words.
func (w *mdWriter) href(n *html.Node) string {
	v, ok := attrOf(n, "href")
	if !ok {
		return ""
	}
	v = strings.TrimSpace(v)
	if v == "" || strings.HasPrefix(v, "#") {
		return ""
	}
	u, err := url.Parse(v)
	if err != nil {
		return ""
	}
	if w.base != nil {
		u = w.base.ResolveReference(u)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "mailto":
	default:
		return ""
	}
	s := u.String()
	if len(s) > 2048 {
		return ""
	}
	return strings.NewReplacer("(", "%28", ")", "%29", " ", "%20").Replace(s)
}

// table renders rows as `| a | b |` lines with a rule after the first.
// A cell's content is collapsed to one line, nested tables included.
func (w *mdWriter) table(n *html.Node) {
	w.brk(2)
	cols, first := 0, true
	var rows []*html.Node
	var collectRows func(*html.Node)
	collectRows = func(c *html.Node) {
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			if k.Type != html.ElementNode {
				continue
			}
			switch k.DataAtom {
			case atom.Tr:
				rows = append(rows, k)
			case atom.Thead, atom.Tbody, atom.Tfoot:
				collectRows(k)
			}
		}
	}
	collectRows(n)
	for _, tr := range rows {
		if w.exhausted {
			return
		}
		var cells []string
		for k := tr.FirstChild; k != nil; k = k.NextSibling {
			if k.Type == html.ElementNode && (k.DataAtom == atom.Td || k.DataAtom == atom.Th) && !w.skipped(k) {
				cells = append(cells, strings.ReplaceAll(w.cellText(k), "|", "\\|"))
			}
		}
		if len(cells) == 0 {
			continue
		}
		if first {
			cols = len(cells)
		}
		for len(cells) < cols {
			cells = append(cells, "")
		}
		w.open("| " + strings.Join(cells, " | ") + " |")
		w.brk(1)
		if first {
			w.open("|" + strings.Repeat(" --- |", cols))
			w.brk(1)
			first = false
		}
		if w.buf.Len() > w.max {
			w.exhausted = true
			return
		}
	}
	w.brk(2)
}

// cellText is a cell's words on one line: its text collapsed, links as
// their words, images as their alt text.
func (w *mdWriter) cellText(n *html.Node) string {
	var b strings.Builder
	space := false
	put := func(s string) {
		for _, r := range s {
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
	}
	stack := []*html.Node{n}
	for len(stack) > 0 {
		w.visits--
		if w.visits <= 0 {
			w.exhausted = true
			break
		}
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch c.Type {
		case html.TextNode:
			put(c.Data)
			continue
		case html.ElementNode:
			if c != n && w.skipped(c) {
				continue
			}
			switch c.DataAtom {
			case atom.Img:
				if alt, ok := attrOf(c, "alt"); ok {
					put(alt)
				}
				continue
			case atom.Br, atom.P, atom.Div, atom.Li, atom.Tr:
				space = true
			}
		default:
			continue
		}
		for k := c.LastChild; k != nil; k = k.PrevSibling {
			stack = append(stack, k)
		}
	}
	return strings.TrimSpace(b.String())
}

// rawText is a <pre>'s text as written, with <br> as a line break and
// script, style and template left out.
func rawText(n *html.Node) string {
	var b strings.Builder
	stack := []*html.Node{n}
	for len(stack) > 0 && b.Len() < 1<<20 {
		c := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		switch c.Type {
		case html.TextNode:
			b.WriteString(c.Data)
			continue
		case html.ElementNode:
			if c != n && skipText[c.DataAtom] {
				continue
			}
			if c.DataAtom == atom.Br {
				b.WriteByte('\n')
				continue
			}
		default:
			continue
		}
		for k := c.LastChild; k != nil; k = k.PrevSibling {
			stack = append(stack, k)
		}
	}
	return strings.Trim(b.String(), "\n")
}

// collapsedText is an element's text with whitespace collapsed, at most
// about max bytes.
func collapsedText(ctx context.Context, n *html.Node, max int) string {
	tw := &textWalker{ctx: ctx, max: max, visits: 10_000}
	return tw.text(n)
}

// finalize trims each line's end (a block quote's blank line is "> ")
// and the text's edges.
func finalize(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// splitParts cuts text into parts of at most partChars characters, each
// cut at a line's end; a line longer than a part is cut where it must be.
func splitParts(text string, partChars int) []string {
	var parts []string
	var cur strings.Builder
	curLen := 0
	flush := func() {
		if p := strings.TrimSpace(cur.String()); p != "" {
			parts = append(parts, p)
		}
		cur.Reset()
		curLen = 0
	}
	for _, line := range strings.Split(text, "\n") {
		rest := []rune(line)
		for len(rest) > partChars {
			flush()
			parts = append(parts, strings.TrimSpace(string(rest[:partChars])))
			rest = rest[partChars:]
		}
		if curLen > 0 && curLen+1+len(rest) > partChars {
			flush()
		}
		if curLen > 0 {
			cur.WriteByte('\n')
			curLen++
		}
		cur.WriteString(string(rest))
		curLen += len(rest)
	}
	flush()
	return parts
}
