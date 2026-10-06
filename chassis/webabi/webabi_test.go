package webabi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// corpus is the manifest corpus the JS conformance kit reads too, so the
// two validators can't drift apart.
var corpus = filepath.Join("..", "..", "sdk", "web-abi", "test", "fixtures", "manifests")

func TestManifestCorpus(t *testing.T) {
	valid, err := filepath.Glob(filepath.Join(corpus, "valid", "*.json"))
	if err != nil || len(valid) == 0 {
		t.Fatalf("no valid fixtures: %v", err)
	}
	for _, f := range valid {
		data, _ := os.ReadFile(f)
		if _, err := ParseManifest(data); err != nil {
			t.Errorf("%s: want valid, got %v", filepath.Base(f), err)
		}
	}

	raw, err := os.ReadFile(filepath.Join(corpus, "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Invalid map[string]string `json:"invalid"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(corpus, "invalid", "*.json"))
	if len(files) != len(cases.Invalid) {
		t.Fatalf("%d invalid fixtures but %d cases", len(files), len(cases.Invalid))
	}
	for name, ptr := range cases.Invalid {
		data, err := os.ReadFile(filepath.Join(corpus, "invalid", name))
		if err != nil {
			t.Fatal(err)
		}
		_, err = ParseManifest(data)
		var me *ManifestError
		if !errors.As(err, &me) {
			t.Errorf("%s: want a ManifestError, got %v", name, err)
			continue
		}
		found := false
		for _, p := range me.Problems {
			found = found || p.Pointer == ptr
		}
		if !found {
			t.Errorf("%s: want a problem at %q, got %+v", name, ptr, me.Problems)
		}
	}
}

func TestImmutablePrefixes(t *testing.T) {
	m, err := ParseManifest([]byte(`{"abi":1,"immutable":["b/","a/c/"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := m.ImmutablePrefixes(); !reflect.DeepEqual(got, []string{"a/c/", "b/"}) {
		t.Fatalf("got %v", got)
	}
}

func writeABI(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoad(t *testing.T) {
	dir := writeABI(t, map[string]string{
		"txco-web.json":                   `{"abi":1,"immutable":["_app/immutable/","nothing/"]}`,
		"public/index.html":               "home",
		"public/_app/immutable/x.js":      "x",
		"public/_app/version.json":        "{}",
		"public/assets/_Dk3.js":           "chunk",
		"public/__spa-fallback.html":      "shell",
		"public/.well-known/security.txt": "contact",
		"ops/900000/spa-fallback.txcl":    "EMIT .x = 1",
		"notes.md":                        "stray",
		"nitro.json":                      `{"preset":"thanks-computer"}`,
	})
	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !d.HasOps || d.HasServer {
		t.Errorf("HasOps=%v HasServer=%v", d.HasOps, d.HasServer)
	}
	wantPublic := []string{"__spa-fallback.html", "_app/immutable/x.js", "_app/version.json", "assets/_Dk3.js", "index.html"}
	if !reflect.DeepEqual(d.Public, wantPublic) {
		t.Errorf("Public = %v", d.Public)
	}
	if got := d.PublicRoots(); !reflect.DeepEqual(got, []string{"__spa-fallback.html", "_app", "assets/_Dk3.js"}) {
		t.Errorf("PublicRoots = %v", got)
	}
	wantMarkers := []string{
		"FILES/_txco/immutable/_app/immutable/_txco_mark",
		"FILES/_txco/immutable/nothing/_txco_mark",
		"FILES/_txco/public/__spa-fallback.html/_txco_mark",
		"FILES/_txco/public/_app/_txco_mark",
		"FILES/_txco/public/assets/_Dk3.js/_txco_mark",
	}
	if got := d.Markers(); !reflect.DeepEqual(got, wantMarkers) {
		t.Errorf("Markers = %v", got)
	}
	warn := strings.Join(d.Warnings, "\n")
	for _, w := range []string{"dot path", "notes.md", "nothing/ matches no file"} {
		if !strings.Contains(warn, w) {
			t.Errorf("warnings %q lack %q", warn, w)
		}
	}
	if strings.Contains(warn, "nitro.json") {
		t.Errorf("Nitro's build info is a producer's own file, not a warning: %q", warn)
	}
}

func TestLoadRefuses(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no manifest":             {"public/index.html": "x"},
		"no public/":              {"txco-web.json": `{"abi":1}`},
		"public/_txco/":           {"txco-web.json": `{"abi":1}`, "public/_txco/x": "forged"},
		"server/ without entry":   {"txco-web.json": `{"abi":1}`, "public/a": "a", "server/index.mjs": "x"},
		"entry missing":           {"txco-web.json": `{"abi":1,"server":{"entry":"server/index.mjs"}}`, "public/a": "a"},
		"a bad manifest":          {"txco-web.json": `{"abi":1,"routes":{}}`, "public/a": "a"},
		"an old adapter's FILES/": {"txco-web.json": `{"abi":1}`, "public/a": "a", "FILES/a": "a"},
		"an old adapter's ops":    {"txco-web.json": `{"abi":1}`, "public/a": "a", "900000/spa-fallback.txcl": "EMIT .x = 1"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeABI(t, files)); err == nil {
				t.Fatal("want an error")
			}
		})
	}

	d, err := Load(writeABI(t, map[string]string{
		"txco-web.json":    `{"abi":1,"server":{"entry":"server/index.mjs"}}`,
		"public/a":         "a",
		"server/index.mjs": "export default { fetch() {} }",
	}))
	if err != nil || !d.HasServer {
		t.Fatalf("a server build: d=%+v err=%v", d, err)
	}
}

// A path the admin API would refuse fails the install up front. (macOS can't
// even hold one on disk, so this checks the rule itself.)
func TestCheckStackPathLen(t *testing.T) {
	if err := checkStackPathLen("_app/x.js"); err != nil {
		t.Fatal(err)
	}
	if err := checkStackPathLen("_" + strings.Repeat("a", 1000)); err == nil {
		t.Fatal("want an error: the public marker's path is over the limit")
	}
}

func TestProvenanceRoundTrip(t *testing.T) {
	data := Provenance([]string{"FILES/b", "900000/x.txcl"})
	got, err := ParseProvenance(data)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"900000/x.txcl", "FILES/_txco/web-abi.json", "FILES/b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestCheckOverlay(t *testing.T) {
	d, err := Load(writeABI(t, map[string]string{
		"txco-web.json":              `{"abi":1,"immutable":["app/immutable/"]}`,
		"public/_app/x.js":           "x",
		"public/app/immutable/y.css": "y",
		"public/index.html":          "home",
	}))
	if err != nil {
		t.Fatal(err)
	}
	abi := append([]string{"900000/spa-fallback.txcl", "900000/mock-request.json", "FILES/_app/x.js",
		"FILES/app/immutable/y.css", "FILES/index.html", ProvenancePath}, d.Markers()...)
	author := []string{
		"100/api.txcl",                  // fine
		"FILES/_mail/welcome.txt",       // fine: _mail isn't a public root
		"900000/spa-fallback.txcl",      // same op
		"900000/mock-request.json",      // same mock
		"FILES/index.html",              // same file
		"FILES/_app/secret.json",        // under a public root
		"FILES/app/immutable/z.css",     // under an immutable prefix
		"FILES/_txco/custom/_txco_mark", // the installer's
	}
	got := CheckOverlay(author, abi, d)
	if len(got) != 6 {
		t.Fatalf("want 6 conflicts, got %d:\n%s", len(got), strings.Join(got, "\n"))
	}
	for _, p := range []string{"100/api.txcl", "_mail/welcome.txt"} {
		for _, c := range got {
			if strings.HasPrefix(c, p) || strings.HasPrefix(c, "FILES/"+p) {
				t.Errorf("%s must not conflict: %s", p, c)
			}
		}
	}
}
