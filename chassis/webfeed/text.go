package webfeed

import (
	"html"
	"net/url"
	"strings"
	"time"

	xhtml "golang.org/x/net/html"
)

// plainText makes feed text plain: HTML read for its text (scripts and
// styles dropped, a space where a tag was), entities decoded, whitespace
// collapsed. Feed text is HTML more often than not, declared or not.
func plainText(s string) string {
	if s == "" {
		return ""
	}
	if !strings.ContainsRune(s, '<') {
		if strings.ContainsRune(s, '&') {
			s = html.UnescapeString(s)
		}
		return collapse(s)
	}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	var b strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return collapse(b.String())
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if dropped(tag) {
				skip++
			}
			if blockTag(tag) {
				b.WriteByte(' ')
			}
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if dropped(tag) && skip > 0 {
				skip--
			}
			if blockTag(tag) {
				b.WriteByte(' ')
			}
		case xhtml.TextToken:
			if skip == 0 {
				b.Write(z.Text())
			}
		}
	}
}

// dropped names the elements whose content is not the item's text.
func dropped(tag string) bool {
	switch tag {
	case "script", "style", "noscript", "template", "iframe", "object", "svg":
		return true
	}
	return false
}

// blockTag names the elements that break text: a space stands where one
// starts or ends, so "<p>One</p><p>Two</p>" reads "One Two" while
// "<em>one</em>." keeps its full stop.
func blockTag(tag string) bool {
	switch tag {
	case "p", "div", "br", "hr", "li", "ul", "ol", "dl", "dt", "dd",
		"h1", "h2", "h3", "h4", "h5", "h6", "blockquote", "pre",
		"table", "thead", "tbody", "tfoot", "tr", "td", "th", "caption",
		"section", "article", "header", "footer", "nav", "aside", "main", "address",
		"figure", "figcaption", "form", "fieldset", "details", "summary", "center",
		"img", "video", "audio", "picture", "iframe", "object", "svg",
		"script", "style", "noscript", "template", "title", "body", "html", "head":
		return true
	}
	return false
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// zoneOffsets are the abbreviations feeds use that Go cannot place; a
// trailing one is rewritten as its offset before the date is parsed.
var zoneOffsets = map[string]string{
	"UT": "+0000", "UTC": "+0000", "GMT": "+0000", "Z": "+0000",
	"EST": "-0500", "EDT": "-0400", "CST": "-0600", "CDT": "-0500",
	"MST": "-0700", "MDT": "-0600", "PST": "-0800", "PDT": "-0700",
	"AKST": "-0900", "AKDT": "-0800", "HST": "-1000",
	"BST": "+0100", "CET": "+0100", "CEST": "+0200", "EET": "+0200", "EEST": "+0300",
	"IST": "+0530", "JST": "+0900", "AEST": "+1000", "AEDT": "+1100",
}

// dateLayouts are the shapes feeds write dates in: RFC 3339 (Atom, JSON
// Feed), RFC 822 and its relatives (RSS), and the bare dates some feeds
// settle for.
var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05.999999999Z0700",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04Z07:00",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05",
	"2006-01-02",
	time.RFC1123Z,
	time.RFC1123,
	time.RFC822Z,
	time.RFC822,
	"Mon, 2 Jan 2006 15:04:05 -0700",
	"Mon, 2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04 -0700",
	"Mon, 02 Jan 2006 15:04 -0700",
	"Mon, 2 Jan 2006 15:04 MST",
	"Mon, 02 Jan 2006 15:04 MST",
	"Mon, 2 Jan 2006 15:04:05",
	"Mon, 02 Jan 2006 15:04:05",
	"2 Jan 2006 15:04:05 -0700",
	"02 Jan 2006 15:04:05 -0700",
	"2 Jan 2006 15:04:05 MST",
	"02 Jan 2006 15:04:05 MST",
	"2 Jan 2006 15:04 -0700",
	"2 Jan 2006",
	"02 Jan 2006",
	"Monday, 2 January 2006 15:04:05 -0700",
	"Monday, 02 January 2006 15:04:05 -0700",
	"Mon, 2 January 2006 15:04:05 -0700",
	"Mon, 02 January 2006 15:04:05 -0700",
	"2 January 2006 15:04:05 -0700",
	"2 January 2006",
	"January 2, 2006 15:04:05 -0700",
	"January 2, 2006",
	"Jan 2, 2006",
}

// parseDate reads a feed date as RFC 3339 in UTC, or nil when it is not
// one this reader knows.
func parseDate(s string) *string {
	s = collapse(s)
	if s == "" {
		return nil
	}
	if i := strings.LastIndexByte(s, ' '); i > 0 {
		if off, ok := zoneOffsets[strings.ToUpper(s[i+1:])]; ok {
			s = s[:i+1] + off
		}
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			out := t.UTC().Format(time.RFC3339)
			return &out
		}
	}
	return nil
}

// urlResolver resolves an item's link against the feed's own address and
// accepts only http(s) with a host and no credentials.
type urlResolver struct {
	base *url.URL
}

func resolver(base string) *urlResolver {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return &urlResolver{}
	}
	return &urlResolver{base: u}
}

// resolve returns the absolute URL and its host, lower case, or "", "".
func (r *urlResolver) resolve(raw string) (string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	if !u.IsAbs() {
		if r.base == nil {
			return "", ""
		}
		u = r.base.ResolveReference(u)
	}
	if u.User != nil || u.Opaque != "" {
		return "", ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", ""
	}
	return u.String(), host
}

// Feed media types an HTML page advertises in its <head>.
var feedTypes = map[string]bool{
	"application/rss+xml":   true,
	"application/atom+xml":  true,
	"application/feed+json": true,
	"application/json":      true,
	"application/rdf+xml":   true,
	"application/xml":       true,
	"text/xml":              true,
}

// discoverMax is how many advertised feeds are reported, and
// discoverBytes how far into the page the reader looks for them: a
// <link> in the <head>, which is where they are.
const (
	discoverMax   = 5
	discoverBytes = 256 << 10
)

// Discover lists the feeds an HTML page advertises with
// <link rel="alternate" type="application/rss+xml" href="…"> and its
// kin, resolved against base, in page order, without repeats.
func Discover(page []byte, base string) []string {
	res := resolver(base)
	z := xhtml.NewTokenizer(strings.NewReader(string(head(page, discoverBytes))))
	var out []string
	seen := map[string]bool{}
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return out
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			switch string(name) {
			case "body":
				return out
			case "link":
			default:
				continue
			}
			var rel, typ, href string
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				switch string(k) {
				case "rel":
					rel = strings.ToLower(string(v))
				case "type":
					typ = strings.ToLower(strings.TrimSpace(string(v)))
				case "href":
					href = string(v)
				}
			}
			if !strings.Contains(" "+rel+" ", " alternate ") || !feedTypes[typ] {
				continue
			}
			u, _ := res.resolve(href)
			if u == "" || seen[u] {
				continue
			}
			seen[u] = true
			out = append(out, u)
			if len(out) >= discoverMax {
				return out
			}
		case xhtml.EndTagToken:
			if name, _ := z.TagName(); string(name) == "head" {
				return out
			}
		}
	}
}
