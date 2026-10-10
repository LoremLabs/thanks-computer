package server

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"

	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
	"github.com/loremlabs/thanks-computer/chassis/server/ingress"
	"github.com/loremlabs/thanks-computer/chassis/server/static"
)

func TestPrefersMarkdown(t *testing.T) {
	for _, c := range []struct {
		accept []string
		want   bool
	}{
		{nil, false},
		{[]string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,*/*;q=0.8"}, false}, // a browser
		{[]string{"*/*"}, false},
		{[]string{"text/*"}, false},
		{[]string{"text/markdown"}, true},
		{[]string{"text/markdown, text/html;q=0.9"}, true},
		{[]string{"text/markdown, text/html"}, true}, // a tie: naming markdown is the signal
		{[]string{"text/html, text/markdown;q=0.5"}, false},
		{[]string{"text/markdown;q=0"}, false},
		{[]string{"text/markdown;q=0.5, */*;q=0.1"}, true},
		{[]string{"text/markdown;q=0.5, */*"}, false},
		{[]string{"text/markdown;q=0.5, text/*;q=0.4, */*"}, true},   // text/* is more specific than */*
		{[]string{"text/html", "text/markdown;q=0.5"}, false},        // every header line counts
		{[]string{"text/markdown;q=banana, text/html;q=0.1"}, false}, // a bad range is skipped
		{[]string{"text/markdown; charset=utf-8"}, true},
		{[]string{",,text/markdown"}, true},
	} {
		if got := prefersMarkdown(c.accept); got != c.want {
			t.Errorf("prefersMarkdown(%q) = %v, want %v", c.accept, got, c.want)
		}
	}
}

// inletStub is a resolver with InletActive.
type inletStub struct {
	stubResolver
	active map[string]bool // tenant + "|" + stack
	err    error
}

func (s *inletStub) InletActive(tenant, stack string) (bool, error) {
	return s.active[tenant+"|"+stack], s.err
}

type filesStub map[string]string // reqPath → alternate

func (f filesStub) MarkdownVariant(_, stack, reqPath string) (string, bool) {
	if stack != "web/_markdown" {
		return "", false
	}
	alt, ok := f[reqPath]
	return alt, ok
}

func mdReq(method, path string, accept ...string) []byte {
	in := `{"_txc":{"src":"http"}}`
	in, _ = sjson.Set(in, "_txc.web.req.host", "acme.example")
	in, _ = sjson.Set(in, "_txc.web.req.url.path", path)
	if method != "" {
		in, _ = sjson.Set(in, "_txc.web.req.method", method)
	}
	for i, a := range accept {
		in, _ = sjson.Set(in, "_txc.web.req.headers.Accept."+string(rune('0'+i)), a)
	}
	return []byte(in)
}

func TestNegotiateMarkdown(t *testing.T) {
	r := &inletStub{
		stubResolver: stubResolver{target: ingress.RouteTarget{Tenant: "acme", Stack: "web", Ingress: "web"}, hit: true},
		active:       map[string]bool{"acme|web/_markdown": true},
	}
	files := filesStub{"/about": "/about.md", "/café": "/café.md"}
	run := func(res any, in []byte) string {
		var rv ingress.Resolver = &r.stubResolver
		if rr, ok := res.(ingress.Resolver); ok {
			rv = rr
		}
		return negotiateMarkdown(detectTenantBody(rv, in), in, res, files)
	}

	// Prefers markdown: proposed into the inlet, marked to vary.
	out := run(r, mdReq("GET", "/about", "text/markdown"))
	if s := gjson.Get(out, "_txc.route.stack").String(); s != "web/_markdown" {
		t.Fatalf("stack = %q, want web/_markdown; %s", s, out)
	}
	if to := gjson.Get(out, "_txc.route.to").String(); to != "web/_markdown/0" {
		t.Fatalf("to = %q; %s", to, out)
	}
	if gjson.Get(out, "_txc.web.negotiated.vary").String() != "Accept" {
		t.Fatalf("no vary mark; %s", out)
	}
	if gjson.Get(out, "_txc.web.negotiated.alternate").Exists() {
		t.Fatalf("the markdown answer advertises no alternate; %s", out)
	}
	// HEAD and an internal caller (no method) negotiate too.
	for _, m := range []string{"HEAD", ""} {
		if s := gjson.Get(run(r, mdReq(m, "/", "text/markdown")), "_txc.route.stack").String(); s != "web/_markdown" {
			t.Errorf("%q: stack = %q", m, s)
		}
	}

	// A browser: stays on web, varies, and is told where the markdown is.
	out = run(r, mdReq("GET", "/about", "text/html,*/*;q=0.8"))
	if s := gjson.Get(out, "_txc.route.stack").String(); s != "web" {
		t.Fatalf("browser stack = %q; %s", s, out)
	}
	if gjson.Get(out, "_txc.web.negotiated.vary").String() != "Accept" {
		t.Fatalf("browser answer must vary too; %s", out)
	}
	if a := gjson.Get(out, "_txc.web.negotiated.alternate").String(); a != "/about.md" {
		t.Fatalf("alternate = %q; %s", a, out)
	}
	if a := gjson.Get(run(r, mdReq("GET", "/café")), "_txc.web.negotiated.alternate").String(); a != "/caf%C3%A9.md" {
		t.Fatalf("alternate is not escaped: %q", a)
	}
	// A page with no markdown file: varies, advertises nothing.
	out = run(r, mdReq("GET", "/pricing"))
	if !gjson.Get(out, "_txc.web.negotiated.vary").Exists() || gjson.Get(out, "_txc.web.negotiated.alternate").Exists() {
		t.Fatalf("/pricing: %s", out)
	}

	// Untouched: the proposal is exactly detect-tenant's.
	plain := &inletStub{stubResolver: r.stubResolver, active: map[string]bool{}}
	for name, c := range map[string]struct {
		res any
		in  []byte
	}{
		"POST":           {r, mdReq("POST", "/about", "text/markdown")},
		"asset":          {r, mdReq("GET", "/app.js", "text/markdown")},
		"md url":         {r, mdReq("GET", "/about.md", "text/markdown")},
		"no inlet":       {plain, mdReq("GET", "/about", "text/markdown")},
		"lookup error":   {&inletStub{stubResolver: r.stubResolver, active: r.active, err: errors.New("down")}, mdReq("GET", "/about", "text/markdown")},
		"no InletActive": {&r.stubResolver, mdReq("GET", "/about", "text/markdown")},
		"other tenant":   {&inletStub{stubResolver: stubResolver{target: ingress.RouteTarget{Tenant: "globex", Stack: "web"}, hit: true}, active: r.active}, mdReq("GET", "/", "text/markdown")},
		"inlet stack":    {&inletStub{stubResolver: stubResolver{target: ingress.RouteTarget{Tenant: "acme", Stack: "web/_markdown"}, hit: true}, active: map[string]bool{"acme|web/_markdown/_markdown": true}}, mdReq("GET", "/", "text/markdown")},
		"not http":       {r, []byte(`{"_txc":{"src":"tcp","web":{"req":{"url":{"path":"/"},"headers":{"Accept":["text/markdown"]}}}}}`)},
	} {
		var rv ingress.Resolver = &r.stubResolver
		if rr, ok := c.res.(ingress.Resolver); ok {
			rv = rr
		}
		want := detectTenantBody(rv, c.in)
		if got := negotiateMarkdown(want, c.in, c.res, files); got != want {
			t.Errorf("%s: changed the proposal:\n got %s\nwant %s", name, got, want)
		}
	}
	// A miss stays a miss.
	miss := &inletStub{active: r.active}
	if got := negotiateMarkdown("{}", mdReq("GET", "/", "text/markdown"), miss, files); got != "{}" {
		t.Errorf("miss: %s", got)
	}
}

// The static op: a markdown inlet serves its .md files with the page cache
// policy, and a direct .md URL the base stack lacks serves from its inlet.
func TestStaticResultBodyMarkdown(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(tenantSchemaDDL); err != nil {
		t.Fatal(err)
	}
	fcas, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO tenants VALUES('tnt_a','acme','acme','t',NULL)`)
	exec(`INSERT INTO stacks VALUES('s_w','tnt_a','web',1,'t')`)
	exec(`INSERT INTO stacks VALUES('s_m','tnt_a','web/_markdown',2,'t')`)
	put := func(version int, path, content string) {
		h := sha256Hex([]byte(content))
		exec(`INSERT INTO stack_files VALUES(?,?,?,?)`, version, path, content, h)
		if err := fcas.Put(ctx, h, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	put(1, "FILES/index.html", "<h1>Home</h1>")
	put(1, "FILES/own.md", "# web's own")
	put(2, "FILES/index.md", "# Home")
	put(2, "FILES/about.md", "# About")
	put(2, "FILES/own.md", "# the inlet's")

	ix := static.NewIndex("", zap.NewNop())
	if err := ix.RebuildTenant(db); err != nil {
		t.Fatal(err)
	}

	// Routed into the inlet: / is index.md, never web's index.html.
	env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/", "web/_markdown", "acme", ""))
	if body(t, env) != "# Home" {
		t.Fatalf("inlet /: %s", env)
	}
	if ct := gjson.Get(env, "_txc.web.res.headers.content-type.0").String(); ct != "text/markdown; charset=utf-8" {
		t.Fatalf("content-type = %q", ct)
	}
	if cc := gjson.Get(env, "_txc.web.res.headers.cache-control.0").String(); cc != "max-age=0, must-revalidate" {
		t.Fatalf("cache-control = %q", cc)
	}
	etag := gjson.Get(env, "_txc.web.res.headers.etag.0").String()
	if c := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/", "web/_markdown", "acme", etag)); gjson.Get(c, "_txc.web.res.status").Int() != 304 {
		t.Fatalf("want 304: %s", c)
	}
	// An inlet miss falls through to its ops.
	if env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/nope", "web/_markdown", "acme", "")); env != "{}" {
		t.Fatalf("inlet miss: %s", env)
	}

	// On web: /about.md (which web lacks) is the inlet's, whatever Accept says.
	if env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/about.md", "web", "acme", "")); body(t, env) != "# About" {
		t.Fatalf("direct /about.md: %s", env)
	}
	// web's own file wins.
	if env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/own.md", "web", "acme", "")); body(t, env) != "# web's own" {
		t.Fatalf("/own.md: %s", env)
	}
	// /about stays web's: a miss there (no about.html) is not the markdown.
	if env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/about", "web", "acme", "")); env != "{}" {
		t.Fatalf("/about on web: %s", env)
	}
	// Unrouted requests never reach an inlet.
	if env := staticResultBody(ctx, ix, fcas, reqInTenant(t, "/about.md", "", "acme", "")); env != "{}" {
		t.Fatalf("unrouted /about.md: %s", env)
	}
}
