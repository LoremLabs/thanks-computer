package webabi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// The producers' golden ops (each package's own tests pin them); here they
// must parse strictly, route the way the Web ABI says, and agree with each
// other on the guards every producer shares.
var (
	sveltekitGolden = filepath.Join("..", "..", "sdk", "svelte-adapter-thankscomputer", "test", "golden")
	nitroGolden     = filepath.Join("..", "..", "sdk", "nitro-preset", "test", "golden")
	// The ops @txco/web-abi/producer renders for every producer built on it.
	kitGolden = filepath.Join("..", "..", "sdk", "web-abi", "test", "golden")
	// The ops @txco/react-router writes for its sample app (test/routes.test.mjs).
	reactRouterGolden = filepath.Join("..", "..", "sdk", "react-router", "test", "golden")
	producerGoldens   = []string{sveltekitGolden, nitroGolden, kitGolden, reactRouterGolden}
)

func goldenOp(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func goldenFiles(t *testing.T, root string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root, "*", "*", "*.txcl"))
	if len(files) == 0 {
		t.Fatalf("no goldens under %s", root)
	}
	return files
}

func TestProducerGoldenOpsParse(t *testing.T) {
	for _, root := range producerGoldens {
		for _, f := range goldenFiles(t, root) {
			b, _ := os.ReadFile(f)
			if msgs := txcl.Validate(string(b)); len(msgs) > 0 {
				t.Errorf("%s: %v", f, msgs)
			}
			if _, err := txcl.Resonator(string(b)); err != nil {
				t.Errorf("%s: %v", f, err)
			}
		}
	}
}

func webEnv(method, path string) string {
	return `{"_txc":{"src":"http","web":{"req":{"method":"` + method + `","url":{"path":"` + path + `"}}}}}`
}

// A non-HTTP run that reaches the end of the stack is no web op's.
const cronEnv = `{"_txc":{"src":"cron"}}`

func TestSvelteKitGoldenOpsRoute(t *testing.T) {
	fallback, err := txcl.Resonator(goldenOp(t, sveltekitGolden, "route-aware/900000/spa-fallback.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	notFound404, err := txcl.Resonator(goldenOp(t, sveltekitGolden, "route-aware/900000/spa-404.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	catchAll, err := txcl.Resonator(goldenOp(t, sveltekitGolden, "route-aware/900900/not-found.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		method, path      string
		fallback, page404 bool
	}{
		{"GET", "/", true, false},
		{"GET", "/book/moby-dick", true, false},
		{"GET", "/book/moby.dick", true, false}, // a param may hold a dot: the route table decides
		{"HEAD", "/login", true, false},
		{"GET", "/nope/deeper", false, true},
		{"GET", "/v1.2", false, false},            // a dot on no known route: the catch-all's
		{"GET", "/app/x.JS", false, false},        // case-insensitive
		{"POST", "/book/moby-dick", false, false}, // not a navigation
		{"DELETE", "/nope", false, false},
	} {
		e := webEnv(c.method, c.path)
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
	if fallback.WhenMatches(cronEnv) || notFound404.WhenMatches(cronEnv) || catchAll.WhenMatches(cronEnv) {
		t.Error("a non-HTTP run matched a web op")
	}
}

// TestNavigationGoldenOpsRoute: the producers without a route table (the
// Nitro preset, and the kit every later producer uses) answer every
// navigation with one op, and everything else with the catch-all.
func TestNavigationGoldenOpsRoute(t *testing.T) {
	for _, g := range []struct{ root, rel string }{
		{nitroGolden, "static-404/900000/page-404.txcl"},
		{nitroGolden, "static-builtin-404/900000/page-404.txcl"},
		{nitroGolden, "spa/900000/spa-fallback.txcl"},
		{nitroGolden, "server/900000/page-404.txcl"},
		{kitGolden, "spa/900000/spa-fallback.txcl"},
		{kitGolden, "page-404/900000/page-404.txcl"},
		{kitGolden, "builtin-404/900000/page-404.txcl"},
	} {
		rel := filepath.Join(filepath.Base(filepath.Dir(filepath.Dir(g.root))), g.rel)
		nav, err := txcl.Resonator(goldenOp(t, g.root, g.rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			method, path string
			nav          bool
		}{
			{"GET", "/", true},
			{"GET", "/blog/nope", true},
			{"HEAD", "/about/", true},
			{"GET", "/v1.2", false},       // a dot: the catch-all's
			{"GET", "/_nuxt/x.JS", false}, // case-insensitive
			{"POST", "/about", false},     // not a navigation
			{"DELETE", "/nope", false},
		} {
			if got := nav.WhenMatches(webEnv(c.method, c.path)); got != c.nav {
				t.Errorf("%s %s %s: %v, want %v", rel, c.method, c.path, got, c.nav)
			}
		}
		if nav.WhenMatches(cronEnv) {
			t.Errorf("%s matched a non-HTTP run", rel)
		}
	}
	for _, g := range []struct{ root, mode string }{
		{nitroGolden, "static-404"}, {nitroGolden, "static-builtin-404"}, {nitroGolden, "spa"}, {nitroGolden, "server"},
		{kitGolden, "spa"}, {kitGolden, "page-404"}, {kitGolden, "builtin-404"},
	} {
		mode := g.mode
		catchAll, err := txcl.Resonator(goldenOp(t, g.root, mode+"/900900/not-found.txcl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range []string{"GET", "HEAD", "POST", "DELETE"} {
			if !catchAll.WhenMatches(webEnv(m, "/x.js")) {
				t.Errorf("%s not-found must match every HTTP request (%s)", mode, m)
			}
		}
		if catchAll.WhenMatches(cronEnv) {
			t.Errorf("%s not-found matched a non-HTTP run", mode)
		}
	}
}

// opBody is an op's text without its comment lines: the rule itself.
func opBody(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

var navWhen = regexp.MustCompile(`(?s)WHEN .*?\n(?:\s+&&[^\n]*\n)*`)

// TestProducerGoldensShareGuards: every producer's catch-all is the same
// rule, and its navigation ops open with the same guard (a GET or HEAD of an
// extensionless path), so a fix to one reaches the others. The one
// exception is deliberate: a routes-mode 200 (the op that matches the route
// table) carries no extension guard — a path a page route knows is a page
// whatever dots it carries — while its 404 keeps it.
func TestProducerGoldensShareGuards(t *testing.T) {
	const method = `WHEN @src == "http" && (@web.req.method == "GET" || @web.req.method == "HEAD")`
	const ext = `     && @web.req.url.path !~ /(?i)\.[a-z0-9]+$/`
	const guard = method + "\n" + ext
	var catchAll string
	for _, root := range producerGoldens {
		for _, f := range goldenFiles(t, root) {
			b, _ := os.ReadFile(f)
			body := opBody(string(b))
			if filepath.Base(f) == "not-found.txcl" {
				if catchAll == "" {
					catchAll = body
				} else if body != catchAll {
					t.Errorf("%s: the catch-all differs from the others:\n%s\nwant:\n%s", f, body, catchAll)
				}
				continue
			}
			when := navWhen.FindString(body)
			if strings.Contains(when, "@web.req.url.path =~ /") {
				// A routes-mode 200: the method guard, then the route table, and no extension guard.
				if !strings.HasPrefix(when, method) || strings.Contains(when, ext) {
					t.Errorf("%s: a routes-mode fallback must open with the method guard and carry no extension guard:\n%s", f, when)
				}
				continue
			}
			if !strings.HasPrefix(when, guard) {
				t.Errorf("%s: the navigation op doesn't open with the shared guard:\n%s", f, when)
			}
		}
	}
}

// TestReactRouterGoldenOpsRoute runs the React Router matcher through RE2:
// React Router's path syntax (params, an optional param, an optional static
// segment, a splat), case-insensitive except a caseSensitive route.
func TestReactRouterGoldenOpsRoute(t *testing.T) {
	fallback, err := txcl.Resonator(goldenOp(t, reactRouterGolden, "routes/900000/spa-fallback.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	page404, err := txcl.Resonator(goldenOp(t, reactRouterGolden, "routes/900000/spa-404.txcl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path  string
		known bool
	}{
		{"/", true}, {"/about", true}, {"/ABOUT/", true}, {"/users", true}, {"/users/42", true}, {"/users/42/", true},
		{"/users/42.json", true}, // a param may hold a dot: the route table decides
		{"/docs", true}, {"/en/docs", true}, {"/pricing", true}, {"/beta/pricing", true},
		{"/files", true}, {"/files/a/b/c", true}, {"/Exact", true},
		{"/exact", false}, {"/users/42/edit", false}, {"/en/fr/docs", false}, {"/nope", false}, {"/aboutx", false},
	} {
		e := webEnv("GET", c.path)
		if got := fallback.WhenMatches(e); got != c.known {
			t.Errorf("spa-fallback GET %s: %v, want %v", c.path, got, c.known)
		}
		if got := page404.WhenMatches(e); got != !c.known {
			t.Errorf("spa-404 GET %s: %v, want %v", c.path, got, !c.known)
		}
	}
	for _, e := range []string{webEnv("POST", "/about"), webEnv("GET", "/nope.json"), cronEnv} {
		if fallback.WhenMatches(e) || page404.WhenMatches(e) {
			t.Errorf("a navigation op matched a non-navigation: %s", e)
		}
	}
}
