package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/webabi"
)

func writeTreeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
}

// abiWorkspace is a workspace whose web stack is bound to a Web ABI build;
// extra adds or overrides files.
func abiWorkspace(t *testing.T, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"txco.yaml":                                 "stacks:\n  web:\n    abi: www/txco-web\n",
		"www/txco-web/txco-web.json":                `{"abi":1,"immutable":["_app/immutable/"]}`,
		"www/txco-web/public/index.html":            "<!doctype html>home",
		"www/txco-web/public/_app/immutable/x.js":   "console.log(1)",
		"www/txco-web/ops/900000/spa-fallback.txcl": `WHEN @src == "http" EMIT @web.res.status = 200, @halt = true`,
		"www/txco-web/ops/900900/not-found.txcl":    `WHEN @src == "http" EMIT @web.res.status = 404, @halt = true`,
	}
	for k, v := range extra {
		if v == "" {
			delete(files, k)
		} else {
			files[k] = v
		}
	}
	writeTreeFiles(t, root, files)
	return root
}

// fakeABIAdmin is a chassis admin that accepts one deploy of "web" and
// records the files it was sent.
type fakeABIAdmin struct {
	srv      *httptest.Server
	features []string
	drafts   int
	files    []client.StackFile
}

func newFakeABIAdmin(t *testing.T, features ...string) *fakeABIAdmin {
	t.Helper()
	f := &fakeABIAdmin{features: features}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "test", "features": f.features})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/default/stacks":
			http.Error(w, "no bulk list", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tenants/default/stacks/web":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/default/stacks/web/draft":
			f.drafts++
			_ = json.NewEncoder(w).Encode(map[string]any{"version_number": 1})
		case r.Method == http.MethodPut && r.URL.Path == "/v1/tenants/default/stacks/web/versions/1/files":
			var body struct {
				Files []client.StackFile `json:"files"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			f.files = body.Files
			_ = json.NewEncoder(w).Encode(map[string]any{"manifest_hash": "h"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/default/stacks/web/versions/1/validate":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tenants/default/stacks/web/activate":
			_ = json.NewEncoder(w).Encode(map[string]any{"version_number": 1})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeABIAdmin) paths() []string {
	out := make([]string, 0, len(f.files))
	for _, x := range f.files {
		out = append(out, x.Path)
	}
	sort.Strings(out)
	return out
}

func runApplyTo(t *testing.T, srvURL, root string, extra ...string) (int, string) {
	t.Helper()
	t.Setenv("TXCO_HOME", t.TempDir())
	var out, errb bytes.Buffer
	args := append([]string{"--target", srvURL, "--tenant", "default", "--yes"}, extra...)
	code := runApply(append(args, root), &out, &errb)
	return code, errb.String()
}

// TestApplyInstallsTheABIOverlay: a bound stack deploys its author tree and
// its build together — ops from both, public/ as FILES/, a marker per public
// "_" root and immutable prefix, and the provenance file.
func TestApplyInstallsTheABIOverlay(t *testing.T) {
	root := abiWorkspace(t, map[string]string{
		"OPS/web/100/api.txcl":      `WHEN @web.req.url.path == "/api" EMIT .ok = true`,
		"OPS/web/FILES/_mail/t.txt": "a private template",
	})
	f := newFakeABIAdmin(t, "web-abi-markers")
	code, stderr := runApplyTo(t, f.srv.URL, root)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	want := []string{
		"100/api.txcl",
		"900000/spa-fallback.txcl",
		"900900/not-found.txcl",
		"FILES/_app/immutable/x.js",
		"FILES/_mail/t.txt",
		"FILES/_txco/immutable/_app/immutable/_txco_mark",
		"FILES/_txco/public/_app/_txco_mark",
		"FILES/_txco/web-abi.json",
		"FILES/index.html",
	}
	if got := f.paths(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("uploaded:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, x := range f.files {
		if x.Path == webabi.ProvenancePath {
			owned, err := webabi.ParseProvenance([]byte(x.Content))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range owned {
				if p == "100/api.txcl" || p == "FILES/_mail/t.txt" {
					t.Errorf("provenance claims the author's %s", p)
				}
			}
			if len(owned) != 7 {
				t.Errorf("provenance lists %d paths, want 7: %v", len(owned), owned)
			}
		}
	}
}

// TestApplyABIOnlyStack: a bound stack with no OPS/ directory deploys too.
func TestApplyABIOnlyStack(t *testing.T) {
	root := abiWorkspace(t, nil)
	f := newFakeABIAdmin(t, "web-abi-markers")
	if code, stderr := runApplyTo(t, f.srv.URL, root); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}
	if f.drafts != 1 || len(f.files) != 7 {
		t.Fatalf("drafts=%d files=%v", f.drafts, f.paths())
	}

	// A static build with no ops/ at all still deploys its files.
	root = abiWorkspace(t, map[string]string{
		"www/txco-web/ops/900000/spa-fallback.txcl": "",
		"www/txco-web/ops/900900/not-found.txcl":    "",
	})
	f = newFakeABIAdmin(t, "web-abi-markers")
	if code, stderr := runApplyTo(t, f.srv.URL, root); code != 0 || f.drafts != 1 {
		t.Fatalf("no ops: exit=%d drafts=%d stderr=%s", code, f.drafts, stderr)
	}
}

// TestApplyRefusesAChassisWithoutMarkers: markers sent to an older chassis
// would 404 every "_" asset, so apply stops before any draft.
func TestApplyRefusesAChassisWithoutMarkers(t *testing.T) {
	f := newFakeABIAdmin(t) // no features
	code, stderr := runApplyTo(t, f.srv.URL, abiWorkspace(t, nil))
	if code == 0 || !strings.Contains(stderr, "doesn't read Web ABI markers") || f.drafts != 0 {
		t.Fatalf("exit=%d drafts=%d stderr=%s", code, f.drafts, stderr)
	}
}

// TestApplyServerEntryNeedsStaticOnly: no chassis runs server/ yet.
func TestApplyServerEntryNeedsStaticOnly(t *testing.T) {
	root := abiWorkspace(t, map[string]string{
		"www/txco-web/txco-web.json":    `{"abi":1,"server":{"entry":"server/index.mjs"}}`,
		"www/txco-web/server/index.mjs": "export default { fetch() {} }",
	})
	t.Setenv("TXCO_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := runApply([]string{"--dry-run", root}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "--static-only") {
		t.Fatalf("exit=%d stderr=%s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := runApply([]string{"--dry-run", "--static-only", root}, &out, &errb); code != 0 || !strings.Contains(errb.String(), "static half only") {
		t.Fatalf("--static-only: exit=%d stderr=%s", code, errb.String())
	}
}

// TestApplyABICollisions: the author's tree and the build can't both own a
// path, or put a private file under a root the build makes public.
func TestApplyABICollisions(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"the same file":          {"OPS/web/FILES/index.html": "the author's home"},
		"the same op":            {"OPS/web/900000/spa-fallback.txcl": "EMIT .x = 1"},
		"under a public root":    {"OPS/web/100/a.txcl": "EMIT .a = 1", "OPS/web/FILES/_app/secret.json": "{}"},
		"the installer's _txco/": {"OPS/web/100/a.txcl": "EMIT .a = 1", "OPS/web/FILES/_txco/x": "forged"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeABIAdmin(t, "web-abi-markers")
			code, stderr := runApplyTo(t, f.srv.URL, abiWorkspace(t, extra))
			if code == 0 || !strings.Contains(stderr, "collides") || f.drafts != 0 {
				t.Fatalf("exit=%d drafts=%d stderr=%s", code, f.drafts, stderr)
			}
		})
	}
}

// TestApplyUnbuiltBinding: a bound stack whose build is missing fails the
// apply that deploys it, but not one that skips it, and not a push of
// another stack.
func TestApplyUnbuiltBinding(t *testing.T) {
	root := t.TempDir()
	writeTreeFiles(t, root, map[string]string{
		"txco.yaml":            "stacks:\n  web:\n    abi: www/txco-web\n",
		"OPS/other/100/a.txcl": "EMIT .a = 1",
	})
	t.Setenv("TXCO_HOME", t.TempDir())
	var out, errb bytes.Buffer
	if code := runApply([]string{"--dry-run", root}, &out, &errb); code == 0 || !strings.Contains(errb.String(), "can't be installed") {
		t.Fatalf("apply: exit=%d stderr=%s", code, errb.String())
	}
	errb.Reset()
	if code := runApply([]string{"--dry-run", "--skip", "web", root}, &out, &errb); code != 0 {
		t.Fatalf("apply --skip web: exit=%d stderr=%s", code, errb.String())
	}
	errb.Reset()
	if code := runPush([]string{"--dry-run", "other", root}, &out, &errb); code != 0 {
		t.Fatalf("push other: exit=%d stderr=%s", code, errb.String())
	}
}

// TestBuildStackFilesHashTracksTheBuild: a changed byte in public/ is a
// changed stack, so the skip-if-unchanged checks redeploy it.
func TestBuildStackFilesHashTracksTheBuild(t *testing.T) {
	root := abiWorkspace(t, nil)
	hash := func() string {
		ws, err := readWorkspace(root)
		must(t, err)
		b, err := buildStackFiles(root, "web", opsForStack(ws.Ops, "web"), ws.withABI("web", collectOpts{}))
		must(t, err)
		return b.Hash()
	}
	before := hash()
	must(t, os.WriteFile(filepath.Join(root, "www/txco-web/public/index.html"), []byte("<!doctype html>new"), 0o644))
	if hash() == before {
		t.Fatal("the hash didn't change with public/")
	}
}

// TestLintCoversBoundBuilds: lint reports a build that collides with its
// stack's tree, one that can't be installed, and an author op in the
// producer band; a clean bound workspace lints clean.
func TestLintCoversBoundBuilds(t *testing.T) {
	lint := func(root string) (int, string) {
		var out, errb bytes.Buffer
		code := runLint([]string{root}, &out, &errb)
		return code, out.String() + errb.String()
	}
	if code, out := lint(abiWorkspace(t, nil)); code != 0 || !strings.Contains(out, "no issues") {
		t.Fatalf("clean: exit=%d\n%s", code, out)
	}
	if code, out := lint(abiWorkspace(t, map[string]string{"OPS/web/FILES/index.html": "mine"})); code == 0 || !strings.Contains(out, "collides") {
		t.Fatalf("collision: exit=%d\n%s", code, out)
	}
	if code, out := lint(abiWorkspace(t, map[string]string{"www/txco-web/txco-web.json": `{"abi":2}`})); code == 0 || !strings.Contains(out, "can't be installed") {
		t.Fatalf("broken build: exit=%d\n%s", code, out)
	}
	if code, out := lint(abiWorkspace(t, map[string]string{"OPS/web/950000/late.txcl": "EMIT .x = 1"})); code != 0 || !strings.Contains(out, "producer band") {
		t.Fatalf("band warning: exit=%d\n%s", code, out)
	}
}

// TestPullLeavesABIPathsToTheBuild: a pulled version's Web ABI install (its
// ops, public/ as FILES/, markers, provenance) stays out of OPS/<stack>/; the
// author's own files are written as before.
func TestPullLeavesABIPathsToTheBuild(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := abiWorkspace(t, map[string]string{"OPS/web/100/api.txcl": "EMIT .old = 1"})
	owned := []string{"900000/spa-fallback.txcl", "FILES/index.html", "FILES/_txco/public/_app/_txco_mark"}
	prov, _ := json.Marshal(string(webabi.Provenance(owned)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/versions/4"):
			_, _ = w.Write([]byte(`{"version_number":4,"manifest_hash":"h","files":[` +
				`{"path":"100/api.txcl","content":"EMIT .new = 1"},` +
				`{"path":"FILES/_mail/t.txt","content":"template"},` +
				`{"path":"900000/spa-fallback.txcl","content":"EMIT .gen = 1"},` +
				`{"path":"FILES/index.html","content":"home"},` +
				`{"path":"FILES/_txco/public/_app/_txco_mark","content":"m"},` +
				`{"path":"FILES/_txco/web-abi.json","content":` + string(prov) + `}]}`))
		case strings.HasSuffix(r.URL.Path, "/stacks/web"):
			_, _ = w.Write([]byte(`{"name":"web","active_version":4}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var o, e bytes.Buffer
	if code := runPull([]string{"web", root, "--addr", srv.URL, "--force"}, &o, &e); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, e.String())
	}
	var got []string
	_ = filepath.WalkDir(filepath.Join(root, "OPS", "web"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(root, "OPS", "web"), p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(got)
	if want := []string{"100/api.txcl", "FILES/_mail/t.txt"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("OPS/web holds %v, want %v\nstdout=%s", got, want, o.String())
	}
	if !strings.Contains(o.String(), "4 left to its Web ABI build") {
		t.Errorf("stdout=%s", o.String())
	}
}

// TestDraftRefusesABoundStack: draft uploads OPS/<stack>/ raw, which would
// drop the stack's web half.
func TestDraftRefusesABoundStack(t *testing.T) {
	t.Setenv("TXCO_HOME", t.TempDir())
	root := abiWorkspace(t, map[string]string{"OPS/web/100/api.txcl": "EMIT .x = 1"})
	var o, e bytes.Buffer
	if code := runDraft([]string{"web", root}, &o, &e); code == 0 || !strings.Contains(e.String(), "txco push web") {
		t.Fatalf("code=%d stderr=%s", code, e.String())
	}
}
