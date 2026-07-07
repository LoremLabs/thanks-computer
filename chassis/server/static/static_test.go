package static

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func workspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mk("FILES/robots.txt", "CHASSIS-ROBOTS")
	mk("FILES/assets/app.css", "body{}")
	mk("OPS/hello-world/FILES/robots.txt", "STACK-ROBOTS")
	return root
}

func newIdx(t *testing.T, root string) *Index {
	t.Helper()
	return NewIndex(root, zap.NewNop())
}

// Embedded favicon resolves with no workspace, with a strong ETag.
func TestIndexEmbeddedFavicon(t *testing.T) {
	r := newIdx(t, "").Lookup("", "","/favicon.ico")
	if !r.Found || len(r.Body) == 0 {
		t.Fatalf("embedded favicon must resolve; %+v", r)
	}
	if r.Ctype != "image/x-icon" {
		t.Fatalf("content-type=%q", r.Ctype)
	}
	if len(r.ETag) < 3 || r.ETag[0] != '"' {
		t.Fatalf("expected a strong ETag, got %q", r.ETag)
	}
}

// Per-stack FILES overrides chassis-wide when routed to that stack;
// otherwise chassis-wide wins.
func TestIndexLayerPrecedence(t *testing.T) {
	ix := newIdx(t, workspace(t))
	if r := ix.Lookup("", "hello-world", "/robots.txt"); !r.Found || string(r.Body) != "STACK-ROBOTS" {
		t.Fatalf("routed stack robots; %+v", r)
	}
	if r := ix.Lookup("", "","/robots.txt"); !r.Found || string(r.Body) != "CHASSIS-ROBOTS" {
		t.Fatalf("unrouted → chassis robots; %+v", r)
	}
	if r := ix.Lookup("", "other", "/robots.txt"); !r.Found || string(r.Body) != "CHASSIS-ROBOTS" {
		t.Fatalf("stack w/o override → chassis; %+v", r)
	}
}

// A directory in FILES owns its whole prefix: an exact file serves; a
// miss UNDER that dir is Owned (→ 404), not pass-through.
func TestIndexDirectoryOwnership(t *testing.T) {
	ix := newIdx(t, workspace(t))

	if r := ix.Lookup("", "","/assets/app.css"); !r.Found || string(r.Body) != "body{}" {
		t.Fatalf("assets file must serve; %+v", r)
	}
	if r := ix.Lookup("", "","/assets/missing.js"); r.Found || !r.Owned {
		t.Fatalf("miss under owned dir must be Owned (404), got %+v", r)
	}
	if r := ix.Lookup("", "","/assets/deep/nope.png"); r.Found || !r.Owned {
		t.Fatalf("deep miss under owned dir must be Owned; %+v", r)
	}
	// Top-level (no directory) is NEVER prefix-owned — needs an explicit
	// file. A missing top-level path passes through.
	if r := ix.Lookup("", "","/nope.txt"); r.Found || r.Owned {
		t.Fatalf("missing top-level must pass through; %+v", r)
	}
	if r := ix.Lookup("", "","/"); r.Found || r.Owned {
		t.Fatalf("root must pass through; %+v", r)
	}
	// Ownership is FILE-LIKE misses only: an extension-less path under an
	// owned dir is a PAGE route (the adapter's route-aware fallbacks and
	// data-404 ops handle it) and must pass through — the
	// /publications/<ns>/<slug> case, which shares its prefix with the
	// prerendered /publications/<slug>.html catalog pages.
	if r := ix.Lookup("", "", "/assets/some-page"); r.Found || r.Owned {
		t.Fatalf("extension-less miss under owned dir must pass through; %+v", r)
	}
	if r := ix.Lookup("", "", "/assets/deep/nested-page"); r.Found || r.Owned {
		t.Fatalf("deep extension-less miss must pass through; %+v", r)
	}
}

// try_files: prerendered routes serve their own HTML for the
// extension-less URL the browser requests, root resolves index.html, and
// genuine misses still pass through / 404-under-owned-dir as before.
func TestIndexTryFiles(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	mk("FILES/index.html", "<root>")
	mk("FILES/about.html", "<about>")             // clean URL
	mk("FILES/blog/index.html", "<blog>")         // directory index
	mk("FILES/docs.html", "<docs-clean>")         // precedence: .html wins…
	mk("FILES/docs/index.html", "<docs-dir>")     // …over /index.html
	mk("FILES/app/immutable/x.js", "JS")          // hashed asset (has ext)
	ix := newIdx(t, root)

	served := []struct{ path, body string }{
		{"/", "<root>"},                      // root → index.html
		{"/about", "<about>"},                // clean URL → about.html
		{"/about/", "<about>"},               // trailing slash normalizes the same
		{"/blog", "<blog>"},                  // → blog/index.html
		{"/blog/", "<blog>"},                 // → blog/index.html
		{"/docs", "<docs-clean>"},            // .html beats /index.html
		{"/app/immutable/x.js", "JS"},        // exact asset
		{"/index.html", "<root>"},            // explicit file still serves
		{"/about.html", "<about>"},           // explicit .html still serves
	}
	for _, c := range served {
		if r := ix.Lookup("", "", c.path); !r.Found || string(r.Body) != c.body {
			t.Errorf("%s: want %q, got Found=%v body=%q owned=%v", c.path, c.body, r.Found, string(r.Body), r.Owned)
		}
	}

	// A resolved clean-URL serves with the target file's content-type.
	if r := ix.Lookup("", "", "/about"); r.Ctype != "text/html; charset=utf-8" {
		t.Errorf("/about content-type=%q, want text/html", r.Ctype)
	}

	// Misses: an extension-less top-level miss passes through (SPA-fallback
	// territory); a miss under an owned dir is still Owned (404).
	if r := ix.Lookup("", "", "/no-such-route"); r.Found || r.Owned {
		t.Errorf("extension-less miss must pass through; %+v", r)
	}
	if r := ix.Lookup("", "", "/app/immutable/missing.js"); r.Found || !r.Owned {
		t.Errorf("missing asset under owned dir must be Owned (404); %+v", r)
	}
}

func TestIndexNestedContentType(t *testing.T) {
	if r := newIdx(t, workspace(t)).Lookup("", "","/assets/app.css"); r.Ctype != "text/css; charset=utf-8" {
		t.Fatalf("content-type=%q", r.Ctype)
	}
}

// Content-types are pinned and deterministic regardless of the OS mime
// database (the .md → octet-stream regression). Case-insensitive.
func TestContentTypePinned(t *testing.T) {
	cases := map[string]string{
		"readme.md":  "text/markdown; charset=utf-8",
		"a.css":      "text/css; charset=utf-8",
		"a.JS":       "text/javascript; charset=utf-8",
		"a.cjs":      "text/javascript; charset=utf-8",
		"a.html":     "text/html; charset=utf-8",
		"app.js.map": "application/json",
		"f.woff2":    "font/woff2",
		"p.PNG":      "image/png",
	}
	for name, want := range cases {
		if got := contentType(name, nil); got != want {
			t.Errorf("contentType(%q)=%q want %q", name, got, want)
		}
	}
	// Unknown/extension-less: fall back to content sniffing, then
	// octet-stream when there are no bytes to sniff.
	if got := contentType("x.unknownxz", nil); got != "application/octet-stream" {
		t.Errorf("no-bytes unknown = %q want application/octet-stream", got)
	}
	if got := contentType("noext", []byte("<!DOCTYPE html><title>hi</title>")); got != "text/html; charset=utf-8" {
		t.Errorf("sniff html = %q want text/html; charset=utf-8", got)
	}
	if got := contentType("blob", []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}); got != "image/png" {
		t.Errorf("sniff png = %q want image/png", got)
	}
}

func TestIndexRejectsTraversal(t *testing.T) {
	ix := newIdx(t, workspace(t))
	// These cannot escape the FILES root and have no in-root target.
	for _, p := range []string{
		"", "/..", "/../FILES/robots.txt", "/.hidden", "//etc/passwd",
	} {
		if r := ix.Lookup("", "",p); r.Found || r.Owned {
			t.Fatalf("unsafe %q must not resolve; %+v", p, r)
		}
	}
	// `..` is normalized (rooted path.Clean) — it can't escape, so this
	// resolves to the in-root /robots.txt, which is correct HTTP path
	// normalization, not a traversal.
	if r := ix.Lookup("", "","/assets/../../robots.txt"); !r.Found || string(r.Body) != "CHASSIS-ROBOTS" {
		t.Fatalf("normalized path should resolve to /robots.txt; %+v", r)
	}
}

func TestIndexOversizeSkipped(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "FILES"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "FILES", "big.bin"),
		make([]byte, MaxFileBytes+1), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "FILES", "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ix := newIdx(t, root)
	if r := ix.Lookup("", "","/big.bin"); r.Found {
		t.Fatal("over-cap file must be skipped")
	}
	if r := ix.Lookup("", "","/ok.txt"); !r.Found || string(r.Body) != "ok" {
		t.Fatalf("within-cap sibling must serve; %+v", r)
	}
}

func TestIndexRebuild(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "FILES"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ix := newIdx(t, root)
	if r := ix.Lookup("", "","/late.txt"); r.Found {
		t.Fatal("not created yet")
	}
	if err := os.WriteFile(filepath.Join(root, "FILES", "late.txt"), []byte("late"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ix.Rebuild()
	if r := ix.Lookup("", "","/late.txt"); !r.Found || string(r.Body) != "late" {
		t.Fatalf("Rebuild must pick up new file; %+v", r)
	}
}
