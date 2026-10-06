package webabi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// adapterGolden is the SvelteKit adapter's golden ops (its own tests pin
// them); here they must parse strictly and route the way the Web ABI says.
var adapterGolden = filepath.Join("..", "..", "sdk", "svelte-adapter-thankscomputer", "test", "golden")

func goldenOp(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(adapterGolden, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAdapterGoldenOpsParse(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(adapterGolden, "*", "*", "*.txcl"))
	if len(files) == 0 {
		t.Fatal("no adapter goldens found")
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if msgs := txcl.Validate(string(b)); len(msgs) > 0 {
			t.Errorf("%s: %v", f, msgs)
		}
	}
}

func TestAdapterGoldenOpsRoute(t *testing.T) {
	fallback, err := txcl.Resonator(goldenOp(t, "route-aware/900000/spa-fallback.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	notFound404, err := txcl.Resonator(goldenOp(t, "route-aware/900000/spa-404.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	catchAll, err := txcl.Resonator(goldenOp(t, "route-aware/900900/not-found.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	env := func(method, path string) string {
		return `{"_txc":{"src":"http","web":{"req":{"method":"` + method + `","url":{"path":"` + path + `"}}}}}`
	}
	for _, c := range []struct {
		method, path      string
		fallback, page404 bool
	}{
		{"GET", "/", true, false},
		{"GET", "/book/moby-dick", true, false},
		{"HEAD", "/login", true, false},
		{"GET", "/nope/deeper", false, true},
		{"GET", "/v1.2", false, false},            // a dot: the catch-all's
		{"GET", "/app/x.JS", false, false},        // case-insensitive
		{"POST", "/book/moby-dick", false, false}, // not a navigation
		{"DELETE", "/nope", false, false},
	} {
		e := env(c.method, c.path)
		if got := fallback.WhenMatches(e); got != c.fallback {
			t.Errorf("spa-fallback %s %s: %v, want %v", c.method, c.path, got, c.fallback)
		}
		if got := notFound404.WhenMatches(e); got != c.page404 {
			t.Errorf("spa-404 %s %s: %v, want %v", c.method, c.path, got, c.page404)
		}
		if !catchAll.WhenMatches(e) {
			t.Errorf("not-found must match every HTTP request (%s %s)", c.method, c.path)
		}
	}
	// A non-HTTP run that reaches the end of the stack is none of these.
	cron := `{"_txc":{"src":"cron"}}`
	if fallback.WhenMatches(cron) || notFound404.WhenMatches(cron) || catchAll.WhenMatches(cron) {
		t.Error("a non-HTTP run matched a web op")
	}
}
