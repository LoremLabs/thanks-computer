// Package compute implements the authoring CLI for sandboxed op:// computes
// (wired as `txco op`). It scaffolds a compute, bundles + builds it to a
// content-addressed WASI module, and runs it locally on the same wazero engine
// the chassis uses — so "works locally" means "works in production".
//
// A compute is authored with the @txco/op SDK:
//
//	import { op } from "@txco/op";
//	export default op(async ({ input, log }) => ({ ok: true }));
//
// The build pipeline is esbuild (bundle + resolve @txco/op from embedded
// sources + transpile TS) → javy (QuickJS → WASI module, event loop enabled so
// async handlers work). The build seam is per-language: any toolchain that
// emits a conforming WASI stdin→stdout module slots in without runtime changes.
package op

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/cli/op/javybin"
	"github.com/loremlabs/thanks-computer/chassis/compute"
	"github.com/loremlabs/thanks-computer/chassis/compute/javyplugin"
	_ "github.com/loremlabs/thanks-computer/chassis/compute/wazero" // run locally on the real engine
)

// Dispatch routes `txco op <subcommand> …`.
func Dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stdout)
		return 0
	}
	switch args[0] {
	case "init":
		return runInit(args[1:], stdout, stderr)
	case "build":
		return runBuild(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stdout, stderr)
	case "test":
		return runTest(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "op: unknown subcommand %q\n\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `txco op — author sandboxed op:// nano-ops, colocated with their resonator

A compute is a <name>.js (or .ts) sitting next to OPS/<stack>/<scope>/<name>.txcl.
The resonator references it with EXEC "op://<name>". No manifest, no fixtures.

Usage:
  txco op init OPS/<stack>/<scope>/<name>    Scaffold <name>.js (+ <name>.txcl)
  txco op build <path.js|.ts|.txcl>          Bundle + compile; write <name>.wasm, print its compute:// ref
  txco op run   <path.js|.ts|.wasm> --input <json|@file>   Run it on the wazero engine
  txco op test  <path.js|.ts|.txcl> [--input <file>]       Run against the scope's mock-request.json
                                                            (diffs mock-response.json if present)

A compute is authored with @txco/op:
  import { op } from "@txco/op";
  export default op(async ({ input, log }) => ({ ok: true }));
`)
}

// resolveSource maps a CLI argument (a source path, a sibling .txcl, or an
// extensionless prefix) to the compute's source file, preferring an existing
// .js then .ts sibling.
func resolveSource(arg string) string {
	switch strings.ToLower(filepath.Ext(arg)) {
	case ".js", ".ts", ".mjs", ".wasm":
		return arg
	case ".txcl":
		prefix := strings.TrimSuffix(arg, filepath.Ext(arg))
		if fileExists(prefix + ".js") {
			return prefix + ".js"
		}
		if fileExists(prefix + ".ts") {
			return prefix + ".ts"
		}
		return prefix + ".js"
	default:
		if fileExists(arg + ".js") {
			return arg + ".js"
		}
		if fileExists(arg + ".ts") {
			return arg + ".ts"
		}
		return arg + ".js"
	}
}

// workspaceRootFor walks up from a path to the workspace root (the dir
// containing OPS/), where the .txco/compute build cache lives. Falls back to
// the file's own dir.
func workspaceRootFor(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	dir := filepath.Dir(abs)
	for {
		if st, serr := os.Stat(filepath.Join(dir, "OPS")); serr == nil && st.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Dir(abs)
		}
		dir = parent
	}
}

// ---- init ----

func runInit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, "op init: usage: txco op init OPS/<stack>/<scope>/<name>\n\n\t")
		return 2
	}
	// Accept a path with or without an extension; the prefix is the
	// resonator/compute base path (e.g. OPS/site/100/hello).
	prefix := strings.TrimSuffix(strings.TrimSuffix(args[0], ".js"), ".txcl")
	base := filepath.Base(prefix)
	jsPath := prefix + ".js"
	resonatorPath := prefix + ".txcl"

	if _, err := os.Stat(jsPath); err == nil {
		fmt.Fprintf(stderr, "op init: %s already exists\n\n\t", jsPath)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(prefix), 0o755); err != nil {
		fmt.Fprintf(stderr, "op init: %v\n\n\t", err)
		return 1
	}
	if err := os.WriteFile(jsPath, []byte(starterJS), 0o644); err != nil {
		fmt.Fprintf(stderr, "op init: write %s: %v\n\n\t", jsPath, err)
		return 1
	}
	created := jsPath
	if _, err := os.Stat(resonatorPath); err != nil {
		resonator := fmt.Sprintf("EXEC \"op://%s\"\n", base)
		if err := os.WriteFile(resonatorPath, []byte(resonator), 0o644); err != nil {
			fmt.Fprintf(stderr, "op init: write %s: %v\n\n\t", resonatorPath, err)
			return 1
		}
		created += " + " + resonatorPath
	}
	fmt.Fprintf(stdout, "created %s\n  edit %s, then: txco op test %s\n", created, jsPath, resonatorPath)
	return 0
}

// starterJS is the scaffolded handler — the blessed @txco/op shape. The
// stdin/stdout JSON plumbing and ctx construction are injected by the build
// step (the embedded SDK runtime), so the author only writes the handler.
const starterJS = `import { op } from "@txco/op";

// A txco compute. Receives the request envelope as ` + "`ctx.input`" + ` and returns
// the output envelope. Runs sandboxed: no filesystem, network, or ambient env.
export default op(async ({ input, log }) => {
  log.info("hello from a compute");
  input.greeting = "hello from a compute";
  return input;
});
`

// ---- build ----

func runBuild(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, "op build: usage: txco op build <path.js|.ts|.txcl>\n\n\t")
		return 2
	}
	src := resolveSource(args[0])
	if _, err := os.Stat(src); err != nil {
		fmt.Fprintf(stderr, "op build: %s not found\n\n\t", src)
		return 1
	}
	b, err := BuildFile(src, workspaceRootFor(src))
	if err != nil {
		fmt.Fprintf(stderr, "op build: %v\n\n\t", err)
		return 1
	}
	// Write a named, runnable artifact next to the source (e.g. hello.wasm),
	// so `txco op run hello.wasm` works. The hidden .txco/compute cache is
	// reserved for apply-time builds.
	wasmOut := strings.TrimSuffix(src, filepath.Ext(src)) + ".wasm"
	if err := os.WriteFile(wasmOut, b.Wasm, 0o644); err != nil {
		fmt.Fprintf(stderr, "op build: write %s: %v\n\n\t", wasmOut, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n%s\n", wasmOut, b.Ref)
	return 0
}

// Built is the result of compiling a compute: the module bytes, its
// content-addressed ref, and the engine that runs it. Consumed by `txco
// apply` to upload + resolve op://NAME.
type Built struct {
	Wasm    []byte
	Alg     string
	Digest  string
	Engine  string
	Ref     string // "compute://sha256/<digest>"
	OutPath string
	Entry   string // author's entry file (for cleaning error locations)
}

// BuildFile compiles a single colocated compute source (e.g.
// OPS/site/100/hello.js or .ts) into a WASI module: esbuild bundles it (with
// the embedded @txco/op SDK + TS transpile + tree-shaking), then javy compiles
// the bundle to wasm with the event loop enabled so async handlers run.
// Compiled artifacts are cached under <workspaceRoot>/.txco/compute keyed by the
// hash of the bundled JS, so an unchanged compute skips javy on re-apply.
func BuildFile(entryPath, workspaceRoot string) (Built, error) {
	ext := strings.ToLower(filepath.Ext(entryPath))
	switch ext {
	case ".js", ".ts", ".mjs":
	default:
		return Built{}, fmt.Errorf("unsupported compute source %q (use .js or .ts)", filepath.Base(entryPath))
	}

	bundled, _, berr := bundle(entryPath)
	if berr != nil {
		return Built{}, fmt.Errorf("bundle %s:\n%s", filepath.Base(entryPath), berr)
	}
	// Cache key folds in the bundled JS AND the plugin identity: a dynamically
	// linked module's bytecode is specific to the plugin's QuickJS build, so a
	// Javy/plugin bump must invalidate the cache even if the source is unchanged.
	bh := sha256.Sum256(append([]byte(javyplugin.JavyVersion+"\x00"+javyplugin.Namespace+"\x00"), bundled...))
	key := hex.EncodeToString(bh[:])

	cacheDir := filepath.Join(workspaceRoot, ".txco", "compute")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return Built{}, err
	}
	wasmPath := filepath.Join(cacheDir, key+".wasm")

	wasm, rerr := os.ReadFile(wasmPath)
	if rerr != nil {
		// Cache miss → compile via javy (cwd=cacheDir; relative entry + out).
		// Resolve auto-downloads + caches the pinned toolchain on first use so
		// users never install it by hand; an ErrUnavailable here means even
		// that fell through (offline, unsupported platform).
		javyBin, lerr := javybin.Resolve(context.Background(), os.Stderr)
		if lerr != nil {
			return Built{}, lerr
		}
		// Dynamic linking: emit just this op's bytecode (~1 KB) linked against the
		// shared QuickJS plugin, instead of a self-contained ~1.25 MB module. The
		// vendored plugin is the same bytes the runtime links against. The JS
		// runtime features (event loop, text-encoding, stream-io) come from the
		// plugin, so the old `-J` flags don't apply here (`-C plugin=` rejects
		// them); `source=omitted` drops the embedded JS source (smaller; the
		// authored source isn't needed to run).
		pluginPath := filepath.Join(cacheDir, "javy-plugin-"+javyplugin.JavyVersion+".wasm")
		if _, perr := os.Stat(pluginPath); perr != nil {
			if werr := os.WriteFile(pluginPath, javyplugin.Bytes(), 0o644); werr != nil {
				return Built{}, fmt.Errorf("write javy plugin: %w", werr)
			}
		}
		entryRel := key + ".entry.js"
		if werr := os.WriteFile(filepath.Join(cacheDir, entryRel), bundled, 0o644); werr != nil {
			return Built{}, werr
		}
		var jstderr bytes.Buffer
		cmd := exec.Command(javyBin, "build", entryRel,
			"-C", "dynamic", "-C", "plugin="+pluginPath, "-C", "source=omitted",
			"-o", key+".wasm")
		cmd.Dir = cacheDir
		cmd.Stderr = &jstderr
		if err := cmd.Run(); err != nil {
			return Built{}, fmt.Errorf("compile error in %s:\n%s", entryPath, CleanJSError(jstderr.String(), filepath.Base(entryPath)))
		}
		var rerr2 error
		if wasm, rerr2 = os.ReadFile(wasmPath); rerr2 != nil {
			return Built{}, rerr2
		}
	}

	sum := sha256.Sum256(wasm)
	digest := hex.EncodeToString(sum[:])
	ref := compute.Ref{Alg: "sha256", Digest: digest}
	return Built{
		Wasm: wasm, Alg: "sha256", Digest: digest, Engine: "wazero",
		Ref: ref.String(), OutPath: wasmPath, Entry: entryPath,
	}, nil
}

// CleanJSError tidies javy/QuickJS error text: it strips toolchain wrapper
// noise and points the QuickJS-internal "function.mjs" frames at the bundled
// entry so the message reads cleanly. (Precise author-line remapping via the
// esbuild sourcemap is a follow-up; the bundle is unminified so frames remain
// legible in the meantime.)
func CleanJSError(raw, entry string) string {
	s := strings.ReplaceAll(raw, "function.mjs", entry)
	s = strings.ReplaceAll(s, "Failed to compile source code: ", "")
	s = strings.ReplaceAll(s, "Error: Error: ", "Error: ")
	return strings.TrimSpace(s)
}

// ---- run ----

// runRun runs a built (.wasm) or source (.js/.ts) compute on the wazero engine
// with the provided --input (inline JSON or @file). For source inputs it builds
// first. The input is wrapped into the ABI-v2 envelope {input, meta, env}.
func runRun(args []string, stdout, stderr io.Writer) int {
	f, err := parseRunFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "op run: %v\n\n\t", err)
		return 2
	}
	if f.path == "" {
		fmt.Fprint(stderr, "op run: usage: txco op run <path.js|.ts|.wasm> --input <json|@file> [--env K=V] [--secret K=V]\n\n\t")
		return 2
	}
	src := resolveSource(f.path)
	if _, err := os.Stat(src); err != nil {
		fmt.Fprintf(stderr, "op run: %s not found\n\n\t", src)
		return 1
	}

	var wasm []byte
	entry := src
	if strings.EqualFold(filepath.Ext(src), ".wasm") {
		if wasm, err = os.ReadFile(src); err != nil {
			fmt.Fprintf(stderr, "op run: read %s: %v\n\n\t", src, err)
			return 1
		}
	} else {
		b, berr := BuildFile(src, workspaceRootFor(src))
		if berr != nil {
			fmt.Fprintf(stderr, "op run: %v\n\n\t", berr)
			return 1
		}
		wasm = b.Wasm
		entry = b.Entry
	}

	input, err := readInputSpec(f.input)
	if err != nil {
		fmt.Fprintf(stderr, "op run: %v\n\n\t", err)
		return 1
	}
	var envv any
	if f.env != nil {
		envv = f.env
	}
	out, err := runWasm(wasm, wrapInput(input, envv, f.secrets, baseName(entry)))
	if err != nil {
		fmt.Fprintf(stderr, "op run: %s\n\n\t", CleanJSError(err.Error(), filepath.Base(entry)))
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return 0
}

// baseName returns a source file's base name without extension (the op name).
func baseName(p string) string {
	return strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
}

// runFlags is the parsed result of an op run/test argument list.
type runFlags struct {
	path    string
	input   string
	env     map[string]string // --env K=V → ctx.env
	secrets map[string]string // --secret K=V → ctx.secrets
}

// parseRunFlags pulls the positional path, --input, and repeatable --env /
// --secret KEY=VAL flags out of args. env mirrors the op's WITH-clause config
// (ctx.env); secrets mirrors the per-op SecretBag (ctx.secrets) — so local runs
// match what the chassis hands a compute at runtime.
func parseRunFlags(args []string) (runFlags, error) {
	var f runFlags
	addKV := func(dst *map[string]string, flag, kv string) error {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fmt.Errorf("%s expects KEY=VALUE, got %q", flag, kv)
		}
		if *dst == nil {
			*dst = map[string]string{}
		}
		(*dst)[k] = v
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return args[i], nil
		}
		switch {
		case a == "--input" || a == "-input":
			v, e := next()
			if e != nil {
				return f, e
			}
			f.input = v
		case strings.HasPrefix(a, "--input="):
			f.input = strings.TrimPrefix(a, "--input=")
		case a == "--env" || a == "-env":
			v, e := next()
			if e != nil {
				return f, e
			}
			if e := addKV(&f.env, "--env", v); e != nil {
				return f, e
			}
		case strings.HasPrefix(a, "--env="):
			if e := addKV(&f.env, "--env", strings.TrimPrefix(a, "--env=")); e != nil {
				return f, e
			}
		case a == "--secret" || a == "-secret":
			v, e := next()
			if e != nil {
				return f, e
			}
			if e := addKV(&f.secrets, "--secret", v); e != nil {
				return f, e
			}
		case strings.HasPrefix(a, "--secret="):
			if e := addKV(&f.secrets, "--secret", strings.TrimPrefix(a, "--secret=")); e != nil {
				return f, e
			}
		case strings.HasPrefix(a, "-"):
			return f, fmt.Errorf("unknown flag %q", a)
		default:
			if f.path == "" {
				f.path = a
			}
		}
	}
	return f, nil
}

// readInputSpec reads the --input value: "@file" reads from a file, otherwise
// the value is treated as inline JSON. Empty → {}.
func readInputSpec(spec string) ([]byte, error) {
	if spec == "" {
		return []byte("{}"), nil
	}
	if strings.HasPrefix(spec, "@") {
		b, err := os.ReadFile(spec[1:])
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", spec[1:], err)
		}
		return b, nil
	}
	return []byte(spec), nil
}

// wrapInput builds the ABI-v2 stdin envelope {input, meta, env, secrets} the SDK
// runtime reads. env may be a map[string]string (from --env) or arbitrary JSON
// (from mock-env.json); secrets is a name→value map (from --secret / mock-
// secrets.json); nil renders as {}. meta is a local stand-in for the trace
// identity the chassis stamps at runtime.
func wrapInput(input []byte, env any, secrets map[string]string, name string) []byte {
	var in any
	if len(bytes.TrimSpace(input)) == 0 {
		in = map[string]any{}
	} else if err := json.Unmarshal(input, &in); err != nil {
		// Not JSON — pass through as a raw string input.
		in = string(input)
	}
	if env == nil {
		env = map[string]any{}
	}
	if secrets == nil {
		secrets = map[string]string{}
	}
	meta := map[string]any{"rid": "local", "op": name, "stack": "", "scope": 0, "name": name}
	b, _ := json.Marshal(map[string]any{"input": in, "meta": meta, "env": env, "secrets": secrets})
	return b
}

// readMockEnv loads a scope's mock-env.json (arbitrary JSON config) if present,
// for local ctx.env parity. Missing file → nil (renders as {}).
func readMockEnv(scopeDir string) any {
	p := filepath.Join(scopeDir, "mock-env.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

// readMockSecrets loads a scope's mock-secrets.json (a name→string map) if
// present, for local ctx.secrets parity. Missing/invalid → nil.
func readMockSecrets(scopeDir string) map[string]string {
	b, err := os.ReadFile(filepath.Join(scopeDir, "mock-secrets.json"))
	if err != nil {
		return nil
	}
	var v map[string]string
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}

// ---- test ----

// runTest builds the colocated compute and runs it on the wazero engine with
// the scope's mock-request.json as input (or an explicit --input file). If the
// scope mock is used and a mock-response.json exists, the output is diffed
// against it — mocks double as the compute's test fixtures.
func runTest(args []string, stdout, stderr io.Writer) int {
	f, err := parseRunFlags(args)
	if err != nil {
		fmt.Fprintf(stderr, "op test: %v\n\n\t", err)
		return 2
	}
	if f.path == "" {
		fmt.Fprint(stderr, "op test: usage: txco op test <path.js|.ts|.txcl> [--input <file>] [--env K=V] [--secret K=V]\n\n\t")
		return 2
	}
	src := resolveSource(f.path)
	if _, err := os.Stat(src); err != nil {
		fmt.Fprintf(stderr, "op test: %s not found\n\n\t", src)
		return 1
	}
	b, err := BuildFile(src, workspaceRootFor(src))
	if err != nil {
		fmt.Fprintf(stderr, "op test: %v\n\n\t", err)
		return 1
	}

	scopeDir := filepath.Dir(src)
	var input []byte
	usingMock := false
	switch {
	case f.input != "":
		spec := f.input
		if !strings.HasPrefix(spec, "@") {
			spec = "@" + spec // test --input takes a file path
		}
		if input, err = readInputSpec(spec); err != nil {
			fmt.Fprintf(stderr, "op test: %v\n\n\t", err)
			return 1
		}
	case fileExists(filepath.Join(scopeDir, "mock-request.json")):
		input, _ = os.ReadFile(filepath.Join(scopeDir, "mock-request.json"))
		usingMock = true
	default:
		input = []byte("{}")
	}

	// ctx.env / ctx.secrets parity: flags win; else a scope mock-env.json /
	// mock-secrets.json if present.
	var envv any
	if f.env != nil {
		envv = f.env
	} else if me := readMockEnv(scopeDir); me != nil {
		envv = me
	}
	secrets := f.secrets
	if secrets == nil {
		secrets = readMockSecrets(scopeDir)
	}
	out, err := runWasm(b.Wasm, wrapInput(input, envv, secrets, baseName(src)))
	if err != nil {
		fmt.Fprintf(stderr, "op test: %s\n\n\t", CleanJSError(err.Error(), filepath.Base(b.Entry)))
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", out)

	// Mocks-as-fixtures: when fed the scope mock, diff against the expected
	// response if one exists.
	if usingMock {
		if exp := filepath.Join(scopeDir, "mock-response.json"); fileExists(exp) {
			want, _ := os.ReadFile(exp)
			if jsonEqual(out, want) {
				fmt.Fprintln(stdout, "✓ matches mock-response.json")
			} else {
				fmt.Fprintf(stderr, "✗ differs from mock-response.json\n  want: %s\n  got:  %s\n",
					compactJSON(want), compactJSON(out))
				return 1
			}
		}
	}
	return 0
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func jsonEqual(a, b []byte) bool { return string(compactJSON(a)) == string(compactJSON(b)) }

// compactJSON normalizes JSON for stable comparison; on parse failure it falls
// back to trimmed raw bytes.
func compactJSON(b []byte) []byte {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return bytes.TrimSpace(b)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return bytes.TrimSpace(b)
	}
	return out
}

// runWasm executes a built module on the wazero engine exactly as the chassis
// would, so local results match production.
func runWasm(wasm, input []byte) ([]byte, error) {
	eng, err := compute.OpenEngine("wazero", compute.EngineConfig{MaxMemoryMB: 64})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(wasm)
	art := compute.Artifact{Alg: "sha256", Digest: hex.EncodeToString(sum[:]), Engine: "wazero", Wasm: wasm}
	ctx := context.Background()
	inst, err := eng.Load(ctx, art)
	if err != nil {
		return nil, err
	}
	defer func() { _ = inst.Close(ctx) }()
	// Capture the guest's console.* output. On success we flush it to the CLI's
	// stderr (visible, off the JSON stdout); on error we don't dump it raw —
	// the caller prints a single cleaned error message instead (the error +
	// stack is already in err).
	var con bytes.Buffer
	out, err := inst.Invoke(ctx, input, compute.Limits{
		MaxMemoryMB: 64, MaxWall: 5 * time.Second, Now: time.Now(), Stderr: &con,
	})
	if err == nil && con.Len() > 0 {
		_, _ = os.Stderr.Write(con.Bytes())
	}
	return out, err
}
