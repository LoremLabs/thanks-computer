package op

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// chdirTemp switches to a fresh temp dir for the duration of a test (the CLI
// works in relative paths). Restores the prior cwd on cleanup.
func chdirTemp(t *testing.T) {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })
}

// TestInitColocated scaffolds <name>.js next to a new <name>.txcl rule.
func TestInitColocated(t *testing.T) {
	chdirTemp(t)
	var out, errb bytes.Buffer
	if code := runInit([]string{"OPS/site/100/hello"}, &out, &errb); code != 0 {
		t.Fatalf("init code=%d stderr=%s", code, errb.String())
	}
	js, err := os.ReadFile("OPS/site/100/hello.js")
	if err != nil {
		t.Fatalf("hello.js: %v", err)
	}
	if !strings.Contains(string(js), `import { op } from "@txco/op"`) || !strings.Contains(string(js), "export default op(") {
		t.Fatalf("hello.js missing op() handler: %s", js)
	}
	rule, err := os.ReadFile("OPS/site/100/hello.txcl")
	if err != nil {
		t.Fatalf("hello.txcl: %v", err)
	}
	if !strings.Contains(string(rule), `EXEC "op://hello"`) {
		t.Fatalf("hello.txcl missing EXEC op://hello: %s", rule)
	}
}

// TestInitKeepsExistingRule: init adds only the .js when the rule already exists.
func TestInitKeepsExistingRule(t *testing.T) {
	chdirTemp(t)
	if err := os.MkdirAll("OPS/site/100", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("OPS/site/100/hello.txcl", []byte(`WHEN .x EXEC "op://hello"`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runInit([]string{"OPS/site/100/hello"}, &out, &errb); code != 0 {
		t.Fatalf("init: %s", errb.String())
	}
	rule, _ := os.ReadFile("OPS/site/100/hello.txcl")
	if string(rule) != `WHEN .x EXEC "op://hello"` {
		t.Fatalf("init overwrote existing rule: %s", rule)
	}
	if _, err := os.Stat("OPS/site/100/hello.js"); err != nil {
		t.Fatalf("hello.js not created: %v", err)
	}
}

func TestInitRequiresPath(t *testing.T) {
	var out, errb bytes.Buffer
	if code := runInit(nil, &out, &errb); code == 0 {
		t.Fatal("init with no path should fail")
	}
}

// --- javy-gated end-to-end (build/test on the real toolchain) ---

func setupColocated(t *testing.T, js string) {
	t.Helper()
	chdirTemp(t)
	if err := os.MkdirAll("OPS/site/100", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("OPS/site/100/hello.txcl", []byte(`EXEC "op://hello"`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("OPS/site/100/hello.js", []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildColocated(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
export default op(({ input }) => { input.ok = true; return input; });`)
	var out, errb bytes.Buffer
	if code := runBuild([]string{"OPS/site/100/hello.js"}, &out, &errb); code != 0 {
		t.Fatalf("build code=%d stderr=%s", code, errb.String())
	}
	// Output is two lines: the written <name>.wasm path, then the compute:// ref.
	if !strings.Contains(out.String(), "compute://sha256/") {
		t.Fatalf("build output missing ref = %q", out.String())
	}
	if _, err := os.Stat("OPS/site/100/hello.wasm"); err != nil {
		t.Fatalf("build did not write hello.wasm: %v", err)
	}
}

// TestDynamicLinkingShrinksModule guards the core win of dynamic linking: a
// built op is just its own QuickJS bytecode (~1 KB) linked against the shared
// plugin, NOT a self-contained ~1.25 MB module embedding the engine. A
// regression here (e.g. reverting to a static `javy build`) would balloon the
// per-op artifact ~800x.
func TestDynamicLinkingShrinksModule(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
export default op(({ input }) => { input.ok = true; return input; });`)
	entry := "OPS/site/100/hello.js"
	b, err := BuildFile(entry, workspaceRootFor(entry))
	if err != nil {
		t.Fatalf("BuildFile: %v", err)
	}
	const maxDynBytes = 64 << 10 // generous; real modules are ~1 KB, static were ~1.25 MB
	if len(b.Wasm) > maxDynBytes {
		t.Fatalf("built module is %d bytes; expected dynamically linked < %d (self-contained module regression?)", len(b.Wasm), maxDynBytes)
	}
	if b.Engine != "wazero" {
		t.Fatalf("engine = %q, want wazero", b.Engine)
	}
}

// TestTestUsesScopeMockAndDiffs: `test` feeds the scope's mock-request.json and
// diffs the output against mock-response.json (mocks-as-fixtures).
func TestTestUsesScopeMockAndDiffs(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
export default op(({ input }) => { input.greeting = "hi"; return input; });`)
	if err := os.WriteFile("OPS/site/100/mock-request.json", []byte(`{"in":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("OPS/site/100/mock-response.json", []byte(`{"in":5,"greeting":"hi"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := runTest([]string{"OPS/site/100/hello.txcl"}, &out, &errb); code != 0 {
		t.Fatalf("test code=%d stderr=%s out=%s", code, errb.String(), out.String())
	}
	if !strings.Contains(out.String(), `"in":5`) || !strings.Contains(out.String(), `"greeting":"hi"`) {
		t.Fatalf("output missing fed mock + result: %s", out.String())
	}
	if !strings.Contains(out.String(), "matches mock-response.json") {
		t.Fatalf("expected mock-response match, got: %s / %s", out.String(), errb.String())
	}
}

func TestTestMockMismatchFails(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
export default op(({ input }) => { input.greeting = "hi"; return input; });`)
	_ = os.WriteFile("OPS/site/100/mock-request.json", []byte(`{}`), 0o644)
	_ = os.WriteFile("OPS/site/100/mock-response.json", []byte(`{"greeting":"WRONG"}`), 0o644)
	var out, errb bytes.Buffer
	if code := runTest([]string{"OPS/site/100/hello.txcl"}, &out, &errb); code == 0 {
		t.Fatalf("mismatched mock-response should fail; out=%s", out.String())
	}
	if !strings.Contains(errb.String(), "differs from mock-response.json") {
		t.Fatalf("want differs error, got %s", errb.String())
	}
}

// TestConsoleStaysOffStdout: console.* must not corrupt the JSON result.
func TestConsoleStaysOffStdout(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
export default op(({ input }) => { console.log("NOISE", {a:1}); input.ok = true; return input; });`)
	var out, errb bytes.Buffer
	if code := runTest([]string{"OPS/site/100/hello.js"}, &out, &errb); code != 0 {
		t.Fatalf("test: %s", errb.String())
	}
	if strings.Contains(out.String(), "NOISE") {
		t.Fatalf("console leaked to stdout: %q", out.String())
	}
	if !gjson.Valid(strings.TrimSpace(out.String())) || !gjson.Get(out.String(), "ok").Bool() {
		t.Fatalf("stdout not clean JSON: %q", out.String())
	}
}

// TestAsyncHandlerRuns: an async handler with a real await resolves and writes
// its output (the javy event loop drains the promise).
func TestAsyncHandlerRuns(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
export default op(async ({ input }) => { await Promise.resolve(); input.async = true; return input; });`)
	var out, errb bytes.Buffer
	if code := runRun([]string{"OPS/site/100/hello.js", "--input", `{"x":1}`}, &out, &errb); code != 0 {
		t.Fatalf("run code=%d stderr=%s", code, errb.String())
	}
	if !gjson.Get(out.String(), "async").Bool() || gjson.Get(out.String(), "x").Int() != 1 {
		t.Fatalf("async handler output wrong: %q", out.String())
	}
}

// TestSecretsAndEnvReachHandler: --secret and --env land in ctx.secrets/ctx.env
// by name, and ctx.env stays distinct from ctx.secrets.
func TestSecretsAndEnvReachHandler(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
export default op(({ env, secrets }) => ({ region: env.region, key: secrets.SHH }));`)
	var out, errb bytes.Buffer
	code := runRun([]string{"OPS/site/100/hello.js", "--input", `{}`,
		"--env", "region=eu", "--secret", "SHH=top-secret"}, &out, &errb)
	if code != 0 {
		t.Fatalf("run code=%d stderr=%s", code, errb.String())
	}
	if gjson.Get(out.String(), "region").String() != "eu" {
		t.Fatalf("ctx.env.region missing: %q", out.String())
	}
	if gjson.Get(out.String(), "key").String() != "top-secret" {
		t.Fatalf("ctx.secrets.SHH missing: %q", out.String())
	}
}

// TestNonOpDefaultErrors: a default export that isn't op(handler) (or a missing
// one) fails loudly rather than emitting {}.
func TestNonOpDefaultErrors(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `export default { not: "a function" };`)
	var out, errb bytes.Buffer
	if code := runRun([]string{"OPS/site/100/hello.js", "--input", `{}`}, &out, &errb); code == 0 {
		t.Fatalf("non-op default should fail; stdout=%q", out.String())
	}
	if !strings.Contains(errb.String(), "op(handler)") {
		t.Fatalf("want op(handler) requirement error; got %q", errb.String())
	}
}

// TestCryptoHelperTreeShakes: importing @txco/op/crypto produces a correct
// sha256, and a compute that imports only `op` does NOT carry the crypto code.
func TestCryptoHelperTreeShakes(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	setupColocated(t, `import { op } from "@txco/op";
import { sha256 } from "@txco/op/crypto";
export default op(({ input }) => ({ digest: sha256(input.name) }));`)
	var out, errb bytes.Buffer
	if code := runRun([]string{"OPS/site/100/hello.js", "--input", `{"name":"Matt"}`}, &out, &errb); code != 0 {
		t.Fatalf("run code=%d stderr=%s", code, errb.String())
	}
	// sha256("Matt") — matches `printf 'Matt' | shasum -a 256`.
	const want = "84a4b19e19aa4e2a562ae0286b1e188ef4f4f9a98a92b8730d20a1e0f2882523"
	if got := gjson.Get(out.String(), "digest").String(); got != want {
		t.Fatalf("sha256 mismatch: got %q want %q", got, want)
	}

	// Tree-shaking: a compute importing only `op` must not include sha256 code.
	bundled, _, err := bundle("OPS/site/100/hello.js")
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if !bytes.Contains(bundled, []byte("sha256Bytes")) {
		t.Fatal("expected crypto constant present when crypto is imported")
	}
	if err := os.WriteFile("OPS/site/100/bare.js", []byte(`import { op } from "@txco/op";
export default op(({ input }) => input);`), 0o644); err != nil {
		t.Fatal(err)
	}
	bare, _, err := bundle("OPS/site/100/bare.js")
	if err != nil {
		t.Fatalf("bundle bare: %v", err)
	}
	if bytes.Contains(bare, []byte("sha256Bytes")) {
		t.Fatal("tree-shaking failed: crypto code present without import")
	}
}
