package op

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/computesrc"
)

// writeWorkspace lays out a stack whose compute imports a sibling module from
// another directory, plus a TS compute, under root.
func writeWorkspace(t *testing.T, root string) {
	t.Helper()
	files := map[string]string{
		"OPS/site/100/plan.txcl": `EXEC "op://plan"`,
		"OPS/site/100/plan.js": `import { op } from "@txco/op";
import { twice } from "../lib/twice.js";
export default op(({ input }) => { input.n = twice(input.n ?? 1); return input; });`,
		"OPS/site/lib/twice.js":   `export const twice = (n) => n * 2;`,
		"OPS/site/200/typed.txcl": `EXEC "op://typed"`,
		"OPS/site/200/typed.ts": `import { op } from "@txco/op";
const label: string = "typed";
export default op(({ input }) => { input.label = label; return input; });`,
	}
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func filesByPath(b *computesrc.Bundle) map[string]string {
	out := map[string]string{}
	for _, f := range b.Files {
		out[f.Path] = f.Content
	}
	return out
}

// The bundle holds exactly the authored files — entry plus its local import,
// paths relative to the entry's directory — and never the embedded SDK or the
// generated entry stub.
func TestSourceBundleIsTheAuthoredFiles(t *testing.T) {
	chdirTemp(t)
	writeWorkspace(t, ".")
	_, sm, err := bundle("OPS/site/100/plan.js")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sourceBundle("OPS/site/100/plan.js", sm)
	if err != nil {
		t.Fatal(err)
	}
	got := filesByPath(b)
	if b.Entry != "plan.js" || len(got) != 2 {
		t.Fatalf("bundle = entry %q files %v", b.Entry, got)
	}
	if got["../lib/twice.js"] != `export const twice = (n) => n * 2;` {
		t.Fatalf("import missing or altered: %q", got["../lib/twice.js"])
	}
	if _, _, err := b.Encode(); err != nil {
		t.Fatalf("bundle does not encode: %v", err)
	}
}

// A TypeScript compute keeps its TypeScript, not the transpiled output.
func TestSourceBundleKeepsTypeScript(t *testing.T) {
	chdirTemp(t)
	writeWorkspace(t, ".")
	_, sm, err := bundle("OPS/site/200/typed.ts")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sourceBundle("OPS/site/200/typed.ts", sm)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := os.ReadFile("OPS/site/200/typed.ts")
	if got := filesByPath(b)["typed.ts"]; got != string(src) {
		t.Fatalf("typed.ts = %q, want the authored TS", got)
	}
}

// Building the same source from two different working directories gives the
// same wasm digest AND the same source hash — the bundle is cwd-independent,
// so an unchanged compute is stored once.
func TestBuildSourceIsCwdIndependent(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	chdirTemp(t)
	writeWorkspace(t, ".")
	root, _ := os.Getwd()

	fromRoot, err := BuildFile("OPS/site/100/plan.js", root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("OPS/site"); err != nil {
		t.Fatal(err)
	}
	fromStack, err := BuildFile("100/plan.js", root)
	if err != nil {
		t.Fatal(err)
	}
	if fromRoot.Digest != fromStack.Digest {
		t.Fatalf("digest varies with cwd: %s vs %s", fromRoot.Digest, fromStack.Digest)
	}
	if fromRoot.Source == nil || fromRoot.SourceHash == "" || fromRoot.SourceHash != fromStack.SourceHash {
		t.Fatalf("source hash varies with cwd: %q vs %q", fromRoot.SourceHash, fromStack.SourceHash)
	}
	data, err := os.ReadFile(fromRoot.SourceFile)
	if err != nil {
		t.Fatalf("source copy not written: %v", err)
	}
	if _, hash, err := mustDecode(t, data).Encode(); err != nil || hash != fromRoot.SourceHash {
		t.Fatalf("stored copy hash %s, want %s (%v)", hash, fromRoot.SourceHash, err)
	}
}

func mustDecode(t *testing.T, data []byte) computesrc.Bundle {
	t.Helper()
	b, err := computesrc.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A prebuilt wasm has no source to keep.
func TestBuiltFromWasmHasNoSource(t *testing.T) {
	if b := BuiltFromWasm([]byte("\x00asm")); b.Source != nil || b.SourceFile != "" {
		t.Fatalf("prebuilt wasm carries a source: %+v", b)
	}
}
