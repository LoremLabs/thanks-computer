package server

import (
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/server/static"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
)

// inletChecker is the optional resolver half markdown negotiation needs
// (ingress.DBResolver has it): is `<stack>/_markdown` active for a tenant?
// A resolver without it (YAML-only, test stubs) never negotiates.
type inletChecker interface {
	InletActive(tenant, stack string) (bool, error)
}

// markdownFiles is the static-index half: the markdown file a page has
// in a markdown stack, for an HTML answer to advertise.
type markdownFiles interface {
	MarkdownVariant(tenant, stack, reqPath string) (string, bool)
}

// negotiateMarkdown amends detect-tenant's proposal for an HTTP page
// request whose hostname stack has an active `<stack>/_markdown` inlet:
//
//   - every such request is marked `_txc.web.negotiated.vary = "Accept"`,
//     because its answer depends on Accept, whichever stack gives it;
//   - one that prefers markdown (prefersMarkdown) is proposed into the
//     inlet instead of the stack, so static serves its .md files and its
//     ops answer the rest;
//   - one that doesn't, for a page the inlet holds a file for, is marked
//     `_txc.web.negotiated.alternate` with that file's URL, which the web
//     head sends as a Link rel=alternate on an HTML answer.
//
// Anything else (another source, a non-GET, an asset path, a stack with no
// inlet, a lookup error) returns the proposal unchanged, so a site without
// a `_markdown` inlet answers exactly as before. The marks sit outside
// `_txc.web.res` (reserved, so a tenant can't forge them) and the proposal
// stays inert: boot scopes 1–99 may still override it.
func negotiateMarkdown(proposal string, in []byte, resolver any, files markdownFiles) string {
	inlets, ok := resolver.(inletChecker)
	if !ok {
		return proposal
	}
	f := gjson.GetManyBytes(in, "_txc.src", "_txc.web.req.method", "_txc.web.req.url.path")
	if f[0].String() != "http" {
		return proposal
	}
	if m := f[1].String(); m != "" && m != http.MethodGet && m != http.MethodHead {
		return proposal
	}
	reqPath := f[2].String()
	if !static.PagePath(reqPath) {
		return proposal
	}
	p := gjson.GetMany(proposal, "_txc.route.tenant", "_txc.route.stack", "_txc.route.to")
	tenant, stack := p[0].String(), p[1].String()
	// A hostname route only: the base stack, entered at its scope 0.
	if tenant == "" || p[2].String() != stack+"/0" || !tenants.HostedStack(stack) {
		return proposal
	}
	md := stack + "/" + static.MarkdownInlet
	if active, err := inlets.InletActive(tenant, md); err != nil || !active {
		return proposal
	}

	out, _ := sjson.Set(proposal, "_txc.web.negotiated.vary", "Accept")
	var accept []string
	gjson.GetBytes(in, "_txc.web.req.headers.Accept").ForEach(func(_, v gjson.Result) bool {
		accept = append(accept, v.String())
		return true
	})
	if prefersMarkdown(accept) {
		out, _ = sjson.Set(out, "_txc.route.stack", md)
		out, _ = sjson.Set(out, "_txc.route.to", md+"/0")
		return out
	}
	if files != nil {
		if alt, ok := files.MarkdownVariant(tenant, md, reqPath); ok {
			out, _ = sjson.Set(out, "_txc.web.negotiated.alternate", (&url.URL{Path: alt}).EscapedPath())
		}
	}
	return out
}

// prefersMarkdown reports whether an Accept header asks for markdown over
// HTML: it names text/markdown itself with a q above 0, at least the q
// text/html gets from its most specific match (text/html, text/*, */*). A
// tie goes to markdown, since naming text/markdown at all is the signal (no
// browser does). Every header value counts; a range that doesn't parse is
// skipped.
func prefersMarkdown(accept []string) bool {
	mdQ, mdNamed := -1.0, false
	htmlQ, htmlRank := 0.0, 0 // rank: 0 none, 1 */*, 2 text/*, 3 text/html
	for _, line := range accept {
		for _, part := range strings.Split(line, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			mt, params, err := mime.ParseMediaType(part)
			if err != nil {
				continue
			}
			q := 1.0
			if s, ok := params["q"]; ok {
				v, err := strconv.ParseFloat(s, 64)
				if err != nil || v < 0 || v > 1 {
					continue
				}
				q = v
			}
			switch mt {
			case "text/markdown":
				if !mdNamed || q > mdQ {
					mdQ, mdNamed = q, true
				}
			case "text/html":
				if htmlRank < 3 || q > htmlQ {
					htmlQ, htmlRank = q, 3
				}
			case "text/*":
				if htmlRank < 2 || (htmlRank == 2 && q > htmlQ) {
					htmlQ, htmlRank = q, 2
				}
			case "*/*":
				if htmlRank < 1 || (htmlRank == 1 && q > htmlQ) {
					htmlQ, htmlRank = q, 1
				}
			}
		}
	}
	return mdNamed && mdQ > 0 && mdQ >= htmlQ
}
