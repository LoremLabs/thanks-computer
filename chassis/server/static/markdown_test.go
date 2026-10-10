package static

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// markdownIndex: acme's `web` holds the HTML, its `web/_markdown` inlet
// the markdown; the chassis-wide FILES/ has an index.html and an index.md
// that a markdown stack must never serve.
func markdownIndex(t *testing.T) *Index {
	t.Helper()
	ws := t.TempDir()
	for rel, body := range map[string]string{
		"FILES/index.html": "CHASSIS HTML",
		"FILES/index.md":   "CHASSIS MD",
	} {
		p := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db := tenantDB(t)
	insTenant(t, db, "tnt_a", "acme", false)
	insStack(t, db, "s_w", "tnt_a", "web", 1)
	insFile(t, db, 1, "FILES/index.html", "HOME", hhex("HOME"))
	insFile(t, db, 1, "FILES/about.html", "ABOUT", hhex("ABOUT"))
	insStack(t, db, "s_m", "tnt_a", "web/_markdown", 2)
	insFile(t, db, 2, "FILES/index.md", "# Home", hhex("# Home"))
	insFile(t, db, 2, "FILES/about.md", "# About", hhex("# About"))
	insFile(t, db, 2, "FILES/blog/index.md", "# Blog", hhex("# Blog"))
	insFile(t, db, 2, "FILES/_drafts/x.md", "# Draft", hhex("# Draft"))

	ix := NewIndex(ws, zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestPagePath(t *testing.T) {
	for p, want := range map[string]bool{
		"/": true, "": true, "/about": true, "/blog/": true, "/a/b": true,
		"/app.js": false, "/robots.txt": false, "/about.md": false,
		"/.env": false, "/.well-known/x": false,
	} {
		if got := PagePath(p); got != want {
			t.Errorf("PagePath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestLookupMarkdown(t *testing.T) {
	ix := markdownIndex(t)
	for p, want := range map[string]string{
		"/":         hhex("# Home"),
		"/about":    hhex("# About"),
		"/about/":   hhex("# About"),
		"/blog":     hhex("# Blog"),
		"/about.md": hhex("# About"),
	} {
		r := ix.LookupMarkdown("acme", "web/_markdown", p)
		if !r.Found || r.Hash != want {
			t.Errorf("%s: %+v, want hash of its .md", p, r)
		}
		if r.Ctype != "text/markdown; charset=utf-8" {
			t.Errorf("%s: ctype %q", p, r.Ctype)
		}
	}
	// Misses fall through to the inlet's ops: never Owned, and never the
	// chassis-wide layer, an asset, a private path or another tenant's.
	for _, c := range []struct{ tenant, path string }{
		{"acme", "/nope"},
		{"acme", "/blog/gone"},
		{"acme", "/app.js"},
		{"acme", "/_drafts/x"},
		{"acme", "/.env"},
		{"other", "/"},
	} {
		if r := ix.LookupMarkdown(c.tenant, "web/_markdown", c.path); r.Found || r.Owned {
			t.Errorf("%s %s: %+v, want a plain miss", c.tenant, c.path, r)
		}
	}
	// The base stack's own Lookup is untouched by the inlet's files.
	if r := ix.Lookup("acme", "web", "/about"); !r.Found || r.Hash != hhex("ABOUT") {
		t.Errorf("web /about: %+v, want its about.html", r)
	}
}

func TestMarkdownVariantAndFile(t *testing.T) {
	ix := markdownIndex(t)
	for p, want := range map[string]string{
		"/": "/index.md", "/about": "/about.md", "/blog": "/blog/index.md",
	} {
		got, ok := ix.MarkdownVariant("acme", "web/_markdown", p)
		if !ok || got != want {
			t.Errorf("MarkdownVariant(%q) = %q, %v; want %q", p, got, ok, want)
		}
		// What it advertises serves directly.
		if r := ix.MarkdownFile("acme", "web/_markdown", got); !r.Found {
			t.Errorf("MarkdownFile(%q) missed", got)
		}
	}
	for _, p := range []string{"/nope", "/app.js", "/about.md", "/_drafts/x"} {
		if got, ok := ix.MarkdownVariant("acme", "web/_markdown", p); ok {
			t.Errorf("MarkdownVariant(%q) = %q, want none", p, got)
		}
	}
	// MarkdownFile is exact: a page path is not a .md path.
	for _, p := range []string{"/about", "/", "/_drafts/x.md"} {
		if r := ix.MarkdownFile("acme", "web/_markdown", p); r.Found {
			t.Errorf("MarkdownFile(%q) = %+v, want a miss", p, r)
		}
	}
}
