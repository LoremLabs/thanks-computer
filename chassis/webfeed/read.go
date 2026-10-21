package webfeed

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"golang.org/x/net/html/charset"
)

// Document is a feed as read: its format, what it says about itself, and
// its items in document order, each already plain text with its date in
// RFC 3339 and its URL resolved and checked. Nothing is cut yet: Select
// applies the request and the caps.
type Document struct {
	Format string
	Feed   Info
	Items  []Item
	// Total is how many items the document holds, up to MaxScanItems.
	Total int
	// Truncated says the document was not read whole: more items than
	// MaxScanItems, more elements than the reader holds, or a syntax
	// error after some items were read.
	Truncated bool
}

// The reader's bounds on one document. A feed of 3 MiB cannot hold more
// elements than maxNodes; the cap is a backstop against a document that
// is all tags.
const (
	maxNodes     = 100_000
	maxDepth     = 64
	maxTextBytes = 64 << 10 // one element's own character data
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Read turns a fetched body into a Document. contentType is the
// response's header (for its charset); base is the address the body came
// from, which relative links resolve against. An HTML page is not a feed:
// the error carries the feeds the page advertises, if any.
func Read(body []byte, contentType, base string) (*Document, *Error) {
	b := bytes.TrimLeft(bytes.TrimPrefix(body, utf8BOM), " \t\r\n")
	switch {
	case len(b) == 0:
		return nil, Errorf(CodeNotAFeed, "the document is empty")
	case b[0] == '{':
		return readJSON(b, base)
	case b[0] == '<':
		return readXML(b, contentType, base)
	}
	return nil, Errorf(CodeNotAFeed, "the document is neither XML nor JSON, so not a feed")
}

// readXML reads RSS 2.0 and 0.9x (<rss>), RSS 1.0 (<rdf:RDF>) and Atom
// (<feed>). An encoding the XML declaration names is decoded by the
// decoder; one only the Content-Type names is decoded first.
func readXML(b []byte, contentType, base string) (*Document, *Error) {
	if label := declaredCharset(contentType); label != "" && !bytes.Contains(head(b, 256), []byte("encoding=")) {
		if enc, _ := charset.Lookup(label); enc != nil {
			decoded, err := enc.NewDecoder().Bytes(b)
			if err != nil {
				return nil, Errorf(CodeInvalidFeed, "the document does not decode as %s", label)
			}
			b = decoded
		}
	}
	dec := xml.NewDecoder(bytes.NewReader(b))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = charset.NewReaderLabel

	var root xml.StartElement
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, Errorf(CodeInvalidFeed, "the document is not well-formed XML")
		}
		if se, ok := tok.(xml.StartElement); ok {
			root = se
			break
		}
	}
	local := strings.ToLower(root.Name.Local)
	switch local {
	case "rss", "rdf", "feed":
	case "html":
		e := Errorf(CodeNotAFeed, "the address is an HTML page, not a feed")
		if e.Feeds = Discover(b, base); len(e.Feeds) > 0 {
			e.Message += "; the page advertises " + plural(len(e.Feeds), "feed") + " (`feeds`): read that address instead"
		}
		return nil, e
	default:
		return nil, Errorf(CodeNotAFeed, "the document's root element is <%s>, not a feed's (rss, feed or rdf:RDF)", root.Name.Local)
	}

	r := &reader{dec: dec}
	tree, err := r.element(root, 1)
	broken := err != nil && !errors.Is(err, errTooLarge)
	if tree == nil {
		return nil, Errorf(CodeInvalidFeed, "the document is not well-formed XML")
	}
	doc := &Document{Truncated: r.truncated || broken}
	res := resolver(base)
	switch local {
	case "rss":
		doc.Format = "rss"
		ch := tree.child("channel")
		if ch == nil {
			return nil, Errorf(CodeInvalidFeed, "the RSS document has no <channel>")
		}
		doc.Feed = rssInfo(ch, res)
		for _, it := range ch.children("item") {
			doc.Items = append(doc.Items, rssItem(it, res))
		}
	case "rdf":
		doc.Format = "rss"
		if ch := tree.child("channel"); ch != nil {
			doc.Feed = rssInfo(ch, res)
		}
		for _, it := range tree.children("item") {
			doc.Items = append(doc.Items, rssItem(it, res))
		}
	case "feed":
		doc.Format = "atom"
		doc.Feed = atomInfo(tree, res)
		feedAuthor := ""
		if a := tree.child("author"); a != nil {
			feedAuthor = plainText(a.childText("name"))
		}
		for _, en := range tree.children("entry") {
			it := atomEntry(en, res)
			if it.Author == "" {
				it.Author = feedAuthor
			}
			doc.Items = append(doc.Items, it)
		}
	}
	doc.Items = keep(doc.Items)
	doc.Total = len(doc.Items)
	if broken && doc.Total == 0 {
		return nil, Errorf(CodeInvalidFeed, "the document is not well-formed XML")
	}
	return doc, nil
}

// keep drops an item that has neither a title nor an address: nothing to
// show and nowhere to go.
func keep(items []Item) []Item {
	out := items[:0]
	for _, it := range items {
		if it.Title == "" && it.URL == "" {
			continue
		}
		out = append(out, it)
	}
	return out
}

// node is one element of the document: its local name (lower case), its
// attributes, and its character data and children in document order.
type node struct {
	local   string
	attrs   []xml.Attr
	parts   []part
	textLen int
}

// part is one piece of an element's content: text, or a child element.
type part struct {
	text []byte
	kid  *node
}

var errTooLarge = errors.New("webfeed: too many elements")

// reader builds nodes from the decoder under the package's bounds, and
// counts the feed's items as it goes so a long document stops at
// MaxScanItems.
type reader struct {
	dec       *xml.Decoder
	nodes     int
	items     int
	truncated bool
}

// element reads one element and its subtree. On an error the element
// read so far comes back with it. A container that did not finish (the
// channel of a document that broke off) is kept, since what it holds was
// read whole; an item that did not finish is not, since it was not.
func (r *reader) element(start xml.StartElement, depth int) (*node, error) {
	if depth > maxDepth {
		return nil, errors.New("webfeed: nested too deeply")
	}
	r.nodes++
	if r.nodes > maxNodes {
		r.truncated = true
		return nil, errTooLarge
	}
	n := &node{local: strings.ToLower(start.Name.Local), attrs: start.Attr}
	for {
		tok, err := r.dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, io.ErrUnexpectedEOF
			}
			return n, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(t.Name.Local)
			if (local == "item" || local == "entry") && depth <= 2 {
				if r.items >= MaxScanItems {
					r.truncated = true
					if err := r.dec.Skip(); err != nil {
						return n, err
					}
					continue
				}
				r.items++
			}
			kid, err := r.element(t, depth+1)
			if err != nil {
				if kid != nil && local != "item" && local != "entry" {
					n.parts = append(n.parts, part{kid: kid})
				}
				return n, err
			}
			n.parts = append(n.parts, part{kid: kid})
		case xml.CharData:
			// The decoder's bytes are valid until the next token: copy.
			if n.textLen+len(t) > maxTextBytes {
				if room := maxTextBytes - n.textLen; room > 0 {
					n.parts = append(n.parts, part{text: append([]byte(nil), t[:room]...)})
					n.textLen += room
				}
				r.truncated = true
				continue
			}
			n.parts = append(n.parts, part{text: append([]byte(nil), t...)})
			n.textLen += len(t)
		case xml.EndElement:
			return n, nil
		}
	}
}

func (n *node) child(local string) *node {
	for _, p := range n.parts {
		if p.kid != nil && p.kid.local == local {
			return p.kid
		}
	}
	return nil
}

func (n *node) children(local string) []*node {
	var out []*node
	for _, p := range n.parts {
		if p.kid != nil && p.kid.local == local {
			out = append(out, p.kid)
		}
	}
	return out
}

func (n *node) attr(local string) string {
	for _, a := range n.attrs {
		if strings.EqualFold(a.Name.Local, local) {
			return strings.TrimSpace(a.Value)
		}
	}
	return ""
}

// own is the element's own character data, trimmed: what an RSS <link>
// or <guid> holds.
func (n *node) own() string {
	var b strings.Builder
	for _, p := range n.parts {
		b.Write(p.text)
	}
	return strings.TrimSpace(b.String())
}

// all is the element's text with its descendants', in order, a space
// around each block-level child so paragraphs don't run together: what
// an Atom xhtml content holds.
func (n *node) all() string {
	if len(n.parts) == 1 && n.parts[0].kid == nil {
		return string(n.parts[0].text)
	}
	var b strings.Builder
	n.write(&b)
	return b.String()
}

func (n *node) write(b *strings.Builder) {
	for _, p := range n.parts {
		if p.kid == nil {
			b.Write(p.text)
			continue
		}
		block := blockTag(p.kid.local)
		if block {
			b.WriteByte(' ')
		}
		p.kid.write(b)
		if block {
			b.WriteByte(' ')
		}
	}
}

// childText is the first child's whole text, or "".
func (n *node) childText(local string) string {
	if k := n.child(local); k != nil {
		return k.all()
	}
	return ""
}

// first is the first non-empty of several children's texts.
func (n *node) first(locals ...string) string {
	for _, l := range locals {
		if s := strings.TrimSpace(n.childText(l)); s != "" {
			return s
		}
	}
	return ""
}

func rssInfo(ch *node, res *urlResolver) Info {
	info := Info{
		Title:       plainText(ch.childText("title")),
		Description: plainText(ch.first("description", "subtitle")),
		Updated:     parseDate(ch.first("lastbuilddate", "pubdate", "date", "updated")),
	}
	for _, l := range ch.children("link") {
		if s := l.own(); s != "" {
			info.Link, _ = res.resolve(s)
			break
		}
	}
	return info
}

func rssItem(it *node, res *urlResolver) Item {
	item := Item{
		Title:   plainText(it.childText("title")),
		ID:      strings.TrimSpace(it.childText("guid")),
		Date:    parseDate(it.first("pubdate", "date", "published", "updated")),
		Summary: plainText(it.first("description", "encoded", "summary", "content")),
		Author:  rssAuthor(it),
	}
	if item.ID == "" {
		item.ID = strings.TrimSpace(it.childText("id"))
	}
	for _, l := range it.children("link") {
		if s := l.own(); s != "" {
			item.URL, item.Source = res.resolve(s)
			break
		}
	}
	if item.URL == "" {
		if g := it.child("guid"); g != nil && !strings.EqualFold(g.attr("ispermalink"), "false") {
			item.URL, item.Source = res.resolve(g.own())
		}
	}
	if item.URL == "" {
		item.URL, item.Source = atomLink(it, res)
	}
	return item
}

// rssAuthor reads dc:creator, else RSS's author (an address, with the
// name in parentheses), else an Atom author element in an RSS item.
func rssAuthor(it *node) string {
	if s := plainText(it.childText("creator")); s != "" {
		return s
	}
	a := it.child("author")
	if a == nil {
		return ""
	}
	if name := plainText(a.childText("name")); name != "" {
		return name
	}
	s := plainText(a.all())
	if i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')'); i >= 0 && j > i {
		return strings.TrimSpace(s[i+1 : j])
	}
	return s
}

func atomInfo(f *node, res *urlResolver) Info {
	info := Info{
		Title:       plainText(f.childText("title")),
		Description: plainText(f.first("subtitle", "tagline", "description")),
		Updated:     parseDate(f.first("updated", "modified")),
	}
	info.Link, _ = atomLink(f, res)
	return info
}

func atomEntry(en *node, res *urlResolver) Item {
	item := Item{
		Title:   plainText(en.childText("title")),
		ID:      strings.TrimSpace(en.childText("id")),
		Date:    parseDate(en.first("published", "issued", "updated", "modified", "created")),
		Summary: plainText(en.first("summary", "content")),
	}
	if a := en.child("author"); a != nil {
		item.Author = plainText(a.childText("name"))
	}
	item.URL, item.Source = atomLink(en, res)
	return item
}

// atomLink picks the element's page link: rel="alternate" (or no rel)
// of type text/html first, then any alternate, then the first link with
// an href that is not the feed itself.
func atomLink(n *node, res *urlResolver) (string, string) {
	var alternate, any string
	for _, l := range n.children("link") {
		href := l.attr("href")
		if href == "" {
			continue
		}
		rel := strings.ToLower(l.attr("rel"))
		typ := strings.ToLower(l.attr("type"))
		switch {
		case (rel == "" || rel == "alternate") && (typ == "" || typ == "text/html"):
			return res.resolve(href)
		case rel == "" || rel == "alternate":
			if alternate == "" {
				alternate = href
			}
		case rel != "self" && rel != "hub" && rel != "replies" && rel != "edit":
			if any == "" {
				any = href
			}
		}
	}
	if alternate != "" {
		return res.resolve(alternate)
	}
	if any != "" {
		return res.resolve(any)
	}
	return "", ""
}

// readJSON reads a JSON Feed (jsonfeed.org): a title and an items array.
func readJSON(b []byte, base string) (*Document, *Error) {
	if !gjson.ValidBytes(b) {
		return nil, Errorf(CodeInvalidFeed, "the document is not well-formed JSON")
	}
	root := gjson.ParseBytes(b)
	items := root.Get("items")
	if !items.IsArray() || !strings.HasPrefix(root.Get("version").String(), "https://jsonfeed.org/") && !root.Get("title").Exists() {
		return nil, Errorf(CodeNotAFeed, "the JSON document is not a JSON Feed (no version and title, or no items)")
	}
	res := resolver(base)
	doc := &Document{Format: "json"}
	doc.Feed.Title = plainText(root.Get("title").String())
	doc.Feed.Description = plainText(root.Get("description").String())
	doc.Feed.Link, _ = res.resolve(root.Get("home_page_url").String())
	feedAuthor := plainText(firstOf(root, "authors.0.name", "author.name"))
	items.ForEach(func(_, it gjson.Result) bool {
		if doc.Total >= MaxScanItems {
			doc.Truncated = true
			return false
		}
		doc.Total++
		item := Item{
			ID:      strings.TrimSpace(it.Get("id").String()),
			Title:   plainText(it.Get("title").String()),
			Date:    parseDate(firstOf(it, "date_published", "date_modified")),
			Summary: plainText(firstOf(it, "summary", "content_text", "content_html")),
			Author:  plainText(firstOf(it, "authors.0.name", "author.name")),
		}
		if item.Author == "" {
			item.Author = feedAuthor
		}
		item.URL, item.Source = res.resolve(firstOf(it, "url", "external_url"))
		doc.Items = append(doc.Items, item)
		return true
	})
	doc.Items = keep(doc.Items)
	doc.Total = len(doc.Items)
	return doc, nil
}

func firstOf(v gjson.Result, paths ...string) string {
	for _, p := range paths {
		if s := strings.TrimSpace(v.Get(p).String()); s != "" {
			return s
		}
	}
	return ""
}

// declaredCharset is the Content-Type's charset parameter when it names
// something other than UTF-8, lower case, else "".
func declaredCharset(contentType string) string {
	if contentType == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return ""
	}
	label := strings.ToLower(strings.TrimSpace(params["charset"]))
	switch label {
	case "", "utf-8", "utf8", "us-ascii", "ascii":
		return ""
	}
	return label
}

func head(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}
