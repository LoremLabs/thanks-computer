package static

import "strings"

// MarkdownInlet is the nested stack a page request that prefers markdown
// enters: `<stack>/_markdown`, beside `<stack>/_mail` and `<stack>/_tcp`.
// Its existence (active, with ops) is the opt-in.
const MarkdownInlet = "_markdown"

// IsMarkdownStack reports whether stack is a `<stack>/_markdown` inlet.
func IsMarkdownStack(stack string) bool {
	return strings.HasSuffix(stack, "/"+MarkdownInlet)
}

// PagePath reports whether a request path names a page: the root, or a path
// whose last segment has no extension. Only pages are negotiated, so an
// agent asking for markdown still gets /app.js and /robots.txt as they are.
func PagePath(reqPath string) bool {
	rel := safeRel(reqPath)
	if rel == "" {
		return isRootPath(reqPath)
	}
	return !lastSegHasDot(rel)
}

// markdownCandidates is the markdown counterpart of indexCandidates:
//
//	root            /       → index.md
//	clean URL       /about  → about.md
//	directory index /blog   → blog/index.md
//
// A path that already ends in .md probes itself; any other extension has
// no markdown form.
func markdownCandidates(rel string) []string {
	switch {
	case rel == "":
		return []string{"index.md"}
	case strings.HasSuffix(rel, ".md"):
		return []string{rel}
	case lastSegHasDot(rel):
		return nil
	}
	return []string{rel + ".md", rel + "/index.md"}
}

// markdownLayers returns the layers a markdown stack serves from: its own
// operator layer and its tenant layer. Never the chassis-wide FILES/ or the
// embedded defaults, whose index.html (or index.md) is not this stack's.
func (ix *Index) markdownLayers(tenant, stack string) ([]layer, tenantLayer, bool) {
	ix.mu.Lock()
	ps, tn := ix.perStack, ix.tenant
	ix.mu.Unlock()

	st := safeStack(stack)
	if st == "" {
		return nil, tenantLayer{}, false
	}
	var opLayers []layer
	if l, ok := ps[st]; ok {
		opLayers = append(opLayers, l)
	}
	if ten := safeSeg(tenant); ten != "" {
		if stacks, ok := tn[ten]; ok {
			if l, ok := stacks[st]; ok {
				return opLayers, l, true
			}
		}
	}
	return opLayers, tenantLayer{}, false
}

// lookupMarkdown resolves reqPath against stack's markdown candidates,
// returning the hit and the candidate that matched.
func (ix *Index) lookupMarkdown(tenant, stack, reqPath string) (Result, string) {
	rel := safeRel(reqPath)
	if rel == "" && !isRootPath(reqPath) {
		return Result{}, "" // a dot segment is never a file
	}
	opLayers, tl, haveTenant := ix.markdownLayers(tenant, stack)
	for _, cand := range markdownCandidates(rel) {
		if r, ok := lookupServable(cand, opLayers, tl, haveTenant); ok {
			return r, cand
		}
	}
	return Result{}, ""
}

// LookupMarkdown resolves a request routed into a markdown stack: /about
// → about.md or about/index.md, / → index.md, /about.md as itself. A miss
// is never Owned: it falls through to the stack's own ops.
func (ix *Index) LookupMarkdown(tenant, stack, reqPath string) Result {
	r, _ := ix.lookupMarkdown(tenant, stack, reqPath)
	return r
}

// MarkdownVariant reports the URL path of the markdown file stack holds for
// a page, e.g. "/about.md" for /about, for an HTML answer to advertise. It
// is servable directly (MarkdownFile), whatever the request's Accept says.
func (ix *Index) MarkdownVariant(tenant, stack, reqPath string) (string, bool) {
	if !PagePath(reqPath) {
		return "", false
	}
	if _, cand := ix.lookupMarkdown(tenant, stack, reqPath); cand != "" {
		return "/" + cand, true
	}
	return "", false
}

// MarkdownFile resolves a direct request for a .md path (/about.md) in a
// markdown stack: the exact file, so the URL MarkdownVariant advertises
// serves in a browser too.
func (ix *Index) MarkdownFile(tenant, stack, reqPath string) Result {
	rel := safeRel(reqPath)
	if !strings.HasSuffix(rel, ".md") {
		return Result{}
	}
	opLayers, tl, haveTenant := ix.markdownLayers(tenant, stack)
	r, _ := lookupServable(rel, opLayers, tl, haveTenant)
	return r
}
