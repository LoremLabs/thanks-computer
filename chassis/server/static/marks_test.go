package static

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrivateRoot(t *testing.T) {
	for rel, want := range map[string]string{
		"":                       "",
		"index.html":             "",
		"app/immutable/x.js":     "",
		"_app/immutable/x.js":    "_app",
		"assets/_Dk3.js":         "assets/_Dk3.js",
		"a/_b/_c/d":              "a/_b",
		"__spa-fallback.html":    "__spa-fallback.html",
		"_root.data":             "_root.data",
		"_txco/public/x/_txco_m": "_txco",
	} {
		if got := PrivateRoot(rel); got != want {
			t.Errorf("PrivateRoot(%q) = %q, want %q", rel, got, want)
		}
	}
}

func TestParseMarker(t *testing.T) {
	for rel, want := range map[string]string{
		PublicMarker("_app"):               "public _app",
		PublicMarker("assets/_Dk3.js"):     "public assets/_Dk3.js",
		ImmutableMarker("app/immutable/"):  "immutable app/immutable",
		ImmutableMarker("_nuxt"):           "immutable _nuxt",
		"_txco/public/_app":                "", // no leaf
		"_txco/other/_app/_txco_mark":      "", // unknown kind
		"_txco/public/_txco_mark":          "", // no target
		"_txco/public/_txco/x/_txco_mark":  "", // _txco is never public
		"_txco/immutable/_txco/_txco_mark": "", // nor immutable
		"x/_txco/public/_app/_txco_mark":   "", // only at the FILES root
		"_txco/web-abi.json":               "", // a private file, not a marker
	} {
		kind, target, ok := parseMarker(rel)
		got := ""
		if ok {
			got = kind + " " + target
		}
		if got != want {
			t.Errorf("parseMarker(%q) = %q, want %q", rel, got, want)
		}
	}
}

// TestMarkersOnTheDiskLayer: a public marker makes exactly its root
// servable, an immutable marker flags files under its prefix, and markers
// are invisible to both HTTP and read-file.
func TestMarkersOnTheDiskLayer(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, filepath.Join(ws, "OPS", "site", "FILES"), map[string]string{
		"index.html":                       "<!doctype html>home",
		"_app/immutable/x.js":              "x",
		"_mail/welcome.txt":                "private template",
		"assets/_Dk3.js":                   "chunk",
		"assets/_secret.json":              "{}",
		"app/immutable/y.css":              "y",
		"_txco/web-abi.json":               `{"abi":1}`,
		PublicMarker("_app"):               "",
		PublicMarker("assets/_Dk3.js"):     "",
		ImmutableMarker("_app/immutable/"): "",
		ImmutableMarker("app/immutable/"):  "",
	})
	ix := NewIndex(ws, zap.NewNop())

	if r := ix.Lookup("", "site", "/_app/immutable/x.js"); !r.Found || !r.Immutable {
		t.Errorf("_app/immutable/x.js: want found + immutable, got %+v", r)
	}
	if r := ix.Lookup("", "site", "/assets/_Dk3.js"); !r.Found || r.Immutable {
		t.Errorf("assets/_Dk3.js: want found, not immutable, got %+v", r)
	}
	if r := ix.Lookup("", "site", "/app/immutable/y.css"); !r.Found || !r.Immutable {
		t.Errorf("app/immutable/y.css: want found + immutable, got %+v", r)
	}
	if r := ix.Lookup("", "site", "/index.html"); !r.Found || r.Immutable {
		t.Errorf("index.html: want found, not immutable, got %+v", r)
	}

	// Unmarked private paths: a plain miss, never Owned (no existence leak).
	for _, p := range []string{"/_mail/welcome.txt", "/assets/_secret.json", "/_mail/gone.txt",
		"/_txco/web-abi.json", "/" + PublicMarker("_app")} {
		if r := ix.Lookup("", "site", p); r.Found || r.Owned {
			t.Errorf("%s: want a plain miss, got %+v", p, r)
		}
	}
	// A miss under a public root behaves like any static dir: Owned 404.
	if r := ix.Lookup("", "site", "/_app/immutable/gone.js"); r.Found || !r.Owned {
		t.Errorf("_app/immutable/gone.js: want Owned, got %+v", r)
	}

	// read-file still reads private files, and never sees a marker.
	if _, ok := ix.Asset("", "site", "_mail/welcome.txt"); !ok {
		t.Error("Asset must still read a private template")
	}
	if _, ok := ix.Asset("", "site", PublicMarker("_app")); ok {
		t.Error("Asset must not see a marker")
	}
}

// TestDotPathIsAMissNotTheRoot: safeRel refuses a dot segment with "", which
// used to read as the root and serve index.html for /.env.
func TestDotPathIsAMissNotTheRoot(t *testing.T) {
	ws := t.TempDir()
	writeTree(t, filepath.Join(ws, "OPS", "site", "FILES"), map[string]string{"index.html": "home"})
	ix := NewIndex(ws, zap.NewNop())
	for _, p := range []string{"/.env", "/.well-known/security.txt", "/a/.git/config"} {
		if r := ix.Lookup("", "site", p); r.Found || r.Owned {
			t.Errorf("%s: want a miss, got %+v", p, r)
		}
	}
	for _, p := range []string{"/", "", "/./"} {
		if r := ix.Lookup("", "site", p); !r.Found {
			t.Errorf("%q: want index.html, got %+v", p, r)
		}
	}
}

// TestMarkersOnTheTenantLayer: markers arrive as FILES rows whose bytes are
// never read; one with an empty hash (an empty file through the admin PUT)
// still counts, where any other empty-hash row is skipped.
func TestMarkersOnTheTenantLayer(t *testing.T) {
	db := tenantDB(t)
	insTenant(t, db, "tnt_a", "acme", false)
	insStack(t, db, "s_a", "tnt_a", "web", 10)
	insFile(t, db, 10, "FILES/_app/immutable/x.js", "x", hhex("x"))
	insFile(t, db, 10, "FILES/_mail/t.txt", "t", hhex("t"))
	insFile(t, db, 10, "FILES/"+PublicMarker("_app"), "", "")
	insFile(t, db, 10, "FILES/"+ImmutableMarker("_app/immutable"), "1", hhex("1"))

	ix := NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatal(err)
	}
	if r := ix.Lookup("acme", "web", "/_app/immutable/x.js"); !r.Found || !r.Immutable || r.Hash != hhex("x") {
		t.Errorf("_app/immutable/x.js: want found + immutable, got %+v", r)
	}
	if r := ix.Lookup("acme", "web", "/_mail/t.txt"); r.Found || r.Owned {
		t.Errorf("_mail/t.txt: want a plain miss, got %+v", r)
	}
	if _, ok := ix.Asset("acme", "web", PublicMarker("_app")); ok {
		t.Error("Asset must not see a marker")
	}
	// Markers are per stack: another tenant's identical path stays private.
	insTenant(t, db, "tnt_b", "beta", false)
	insStack(t, db, "s_b", "tnt_b", "web", 20)
	insFile(t, db, 20, "FILES/_app/immutable/x.js", "x", hhex("x"))
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatal(err)
	}
	if r := ix.Lookup("beta", "web", "/_app/immutable/x.js"); r.Found {
		t.Errorf("beta has no marker: want a miss, got %+v", r)
	}
}
