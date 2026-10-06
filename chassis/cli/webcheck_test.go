package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/webabi"
)

func writeABIDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeTreeFiles(t, dir, files)
	return dir
}

// spaBuild is a well-formed SPA build: a fallback for navigations, a
// catch-all for everything else, a "_" asset and an immutable one.
var spaBuild = map[string]string{
	"txco-web.json":             `{"abi":1,"immutable":["app/immutable/"]}`,
	"public/index.html":         "<!doctype html>home",
	"public/assets/site.css":    "body{}",
	"public/_app/version.json":  `{"v":1}`,
	"public/app/immutable/x.js": "console.log(1)",
	"ops/900000/spa-fallback.txcl": `WHEN @src == "http" && (@web.req.method == "GET" || @web.req.method == "HEAD") && @web.req.url.path !~ /(?i)\.[a-z0-9]+$/
  EMIT @web.res.status = 200,
       @web.res.headers.content-type.0 = "text/html; charset=utf-8",
       @web.res.body = b64"<!doctype html>shell",
       @halt = true`,
	"ops/900900/not-found.txcl": `WHEN @src == "http"
  EMIT @web.res.status = 404,
       @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
       @web.res.body = b64"404 not found\n",
       @halt = true`,
}

// TestWebCheckOfflineFailureNeverBoots: a build that's wrong on disk fails
// before any chassis starts.
func TestWebCheckOfflineFailureNeverBoots(t *testing.T) {
	prev := startChassisFn
	t.Cleanup(func() { startChassisFn = prev })
	startChassisFn = func(context.Context, chassisOpts) (string, string, error) {
		t.Fatal("a chassis was started for a build that fails offline")
		return "", "", nil
	}
	for name, change := range map[string]map[string]string{
		"a bad manifest":     {"txco-web.json": `{"abi":1,"routes":{}}`},
		"a bad op":           {"ops/900000/broken.txcl": `WHEN .x =~ 5`},
		"a nested stack":     {"ops/web/100/x.txcl": `EMIT .x = 1`},
		"a server entry":     {"txco-web.json": `{"abi":1,"server":{"entry":"server/index.mjs"}}`, "server/index.mjs": "export default {}"},
		"a malformed op ref": {"ops/900000/r.txcl": `EXEC "op://ssr/render"`},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{}
			for k, v := range spaBuild {
				files[k] = v
			}
			for k, v := range change {
				files[k] = v
			}
			var out, errb bytes.Buffer
			code := runWebCheck([]string{"--json", writeABIDir(t, files)}, &out, &errb)
			if code != 1 {
				t.Fatalf("exit=%d stderr=%s", code, errb.String())
			}
			var rep webCheckReport
			if err := json.Unmarshal(out.Bytes(), &rep); err != nil || rep.OK || len(rep.Problems) == 0 {
				t.Fatalf("report: %s (%v)", out.String(), err)
			}
		})
	}
}

func TestPlanWebProbes(t *testing.T) {
	d, err := webabi.Load(writeABIDir(t, spaBuild))
	must(t, err)
	read := func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(d.Path, "public", rel)) }
	probes := planWebProbes(d, "tok", read)
	got := map[string]webProbe{}
	var names []string
	for _, p := range probes {
		got[p.name] = p
		names = append(names, p.name)
	}
	if want := "root,known-file,underscore,immutable,navigation,dotted,post,head-nav,head-file,conditional"; strings.Join(names, ",") != want {
		t.Fatalf("probes %v", names)
	}
	for name, path := range map[string]string{
		"known-file": "/assets/site.css", "underscore": "/_app/version.json", "immutable": "/app/immutable/x.js",
		"navigation": "/txco-web-check-tok", "dotted": "/txco-web-check-tok.js",
	} {
		if got[name].path != path || got[name].skip != "" {
			t.Errorf("%s: path %q skip %q, want %q", name, got[name].path, got[name].skip, path)
		}
	}

	// A build with no "_" paths and no immutable prefix skips those probes.
	d, err = webabi.Load(writeABIDir(t, map[string]string{"txco-web.json": `{"abi":1}`, "public/a.txt": "a"}))
	must(t, err)
	for _, p := range planWebProbes(d, "tok", read) {
		if (p.name == "underscore" || p.name == "immutable") && p.skip == "" {
			t.Errorf("%s should skip", p.name)
		}
	}
}

func TestWebProbeJudges(t *testing.T) {
	d, err := webabi.Load(writeABIDir(t, spaBuild))
	must(t, err)
	read := func(rel string) ([]byte, error) { return os.ReadFile(filepath.Join(d.Path, "public", rel)) }
	probes := map[string]webProbe{}
	for _, p := range planWebProbes(d, "tok", read) {
		probes[p.name] = p
	}
	unanswered := probeFacts{status: 200, ctype: "application/json", body: []byte(`{"_txc":{"web":{"req":{"method":"GET"}}}}`)}
	html404 := probeFacts{status: 404, ctype: "text/html; charset=utf-8", body: []byte("<!doctype html>")}
	for _, c := range []struct {
		probe string
		facts probeFacts
		want  string
	}{
		{"navigation", unanswered, "fail"},
		{"navigation", html404, "pass"},
		{"navigation", probeFacts{status: 200, ctype: "text/html"}, "pass"},
		{"navigation", probeFacts{status: 200, ctype: "application/json", body: []byte(`{"api":true}`)}, "warn"},
		{"dotted", unanswered, "fail"},
		{"dotted", probeFacts{status: 404, ctype: "text/plain"}, "pass"},
		{"dotted", probeFacts{status: 200, ctype: "text/html"}, "warn"},
		{"post", unanswered, "fail"},
		{"post", probeFacts{status: 405}, "pass"},
		{"post", probeFacts{status: 200, ctype: "text/html"}, "warn"},
		{"immutable", probeFacts{status: 200, cacheControl: "public, max-age=3600"}, "fail"},
		{"immutable", probeFacts{status: 200, cacheControl: "public, max-age=31536000, immutable"}, "pass"},
		{"conditional", probeFacts{status: 200}, "fail"},
		{"conditional", probeFacts{status: 304}, "pass"},
	} {
		if got, detail := probes[c.probe].judge(c.facts); got != c.want {
			t.Errorf("%s %+v: %s (%s), want %s", c.probe, c.facts, got, detail, c.want)
		}
	}
	if r, _ := judgeHeadTwin(probeFacts{status: 404, ctype: "text/html"}, probeResult{Status: 404}); r != "pass" {
		t.Errorf("head twin: %s", r)
	}
	if r, _ := judgeHeadTwin(probeFacts{status: 200, ctype: "application/json"}, probeResult{Status: 404}); r != "fail" {
		t.Errorf("an unanswered HEAD: %s", r)
	}
}

// TestWebCheckE2E runs the whole check against a real chassis, when
// TXCO_E2E_BIN names a built txco (`go build -tags sqlite_fts5 -o … ./cmd/txco`):
// a good build passes, one without a catch-all fails on the probes that
// need it.
func TestWebCheckE2E(t *testing.T) {
	bin := os.Getenv("TXCO_E2E_BIN")
	if bin == "" {
		t.Skip("TXCO_E2E_BIN not set")
	}
	t.Setenv("TXCO_WEB_CHECK_BIN", bin)
	for _, env := range []string{"TXCO_HOME", "TXCO_AUTH_MODE", "TXCO_ADMIN_USER", "TXCO_ADMIN_PASS", "TXCO_WEB_DEBUG",
		"TXCO_DEBUG_PRIVATE", "TXCO_DEBUG_BREAKPOINTS", "TXCO_TRACE_ASYNC", "TXCO_LOG_LEVEL"} {
		t.Setenv(env, os.Getenv(env)) // restored after the test; runWebCheck sets them
	}

	run := func(files map[string]string) (int, webCheckReport, string) {
		var out, errb bytes.Buffer
		code := runWebCheck([]string{"--json", writeABIDir(t, files)}, &out, &errb)
		var rep webCheckReport
		_ = json.Unmarshal(out.Bytes(), &rep)
		return code, rep, out.String() + errb.String()
	}
	code, rep, raw := run(spaBuild)
	if code != 0 || !rep.OK {
		t.Fatalf("a good build: exit=%d\n%s", code, raw)
	}

	noCatchAll := map[string]string{}
	for k, v := range spaBuild {
		if k != "ops/900900/not-found.txcl" {
			noCatchAll[k] = v
		}
	}
	code, rep, raw = run(noCatchAll)
	if code != 1 {
		t.Fatalf("no catch-all: exit=%d\n%s", code, raw)
	}
	failed := map[string]bool{}
	for _, p := range rep.Probes {
		if p.Result == "fail" {
			failed[p.Name] = true
		}
	}
	if !failed["dotted"] || !failed["post"] || failed["navigation"] {
		t.Fatalf("want dotted and post to fail, navigation to pass:\n%s", raw)
	}
}
