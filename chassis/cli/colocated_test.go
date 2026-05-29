package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveOpRefsColocated: a colocated <ruledir>/NAME.js resolves op://NAME
// to a compute ref; two scopes with DIFFERENT NAME.js get DISTINCT digests
// (proving per-rule, not global, resolution).
func TestResolveOpRefsColocated(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/hello.txcl"), `EXEC "op://hello"`)
	writeFile(t, filepath.Join(root, "OPS/site/100/hello.js"), `import { op } from "@txco/op";
export default op(({ input }) => { input.v = 1; return input; });`)
	writeFile(t, filepath.Join(root, "OPS/site/200/hello.txcl"), `EXEC "op://hello"`)
	writeFile(t, filepath.Join(root, "OPS/site/200/hello.js"), `import { op } from "@txco/op";
export default op(({ input }) => { input.v = 2; return input; });`) // different source

	ops, err := bundle.Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("walked %d ops, want 2", len(ops))
	}

	sub, built, err := resolveOpRefsColocated(ops, map[string]oprefs.Operation{}, root, io.Discard)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for _, o := range sub {
		if !strings.Contains(o.Txcl, "compute://sha256/") {
			t.Fatalf("rule not resolved to a compute ref: %q", o.Txcl)
		}
	}
	if len(built) != 2 {
		t.Fatalf("built %d computes, want 2 (distinct per scope)", len(built))
	}
	if built[0].Digest == built[1].Digest {
		t.Fatalf("two different sources produced the same digest %s", built[0].Digest)
	}
}

// TestResolveOpRefsColocatedCacheHit: a second build of the same source reads
// the .txco/compute cache (the wasm file already exists; no recompile needed).
func TestResolveOpRefsColocatedCacheHit(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/hello.txcl"), `EXEC "op://hello"`)
	writeFile(t, filepath.Join(root, "OPS/site/100/hello.js"), `import { op } from "@txco/op";
export default op(({ input }) => input);`)
	ops, _ := bundle.Walk(root)

	_, b1, err := resolveOpRefsColocated(ops, map[string]oprefs.Operation{}, root, io.Discard)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// Remove javy from PATH won't work here; instead assert the cache file
	// exists, then a second resolve still succeeds (would fail if it recompiled
	// with a broken toolchain — but at minimum it must return the same digest).
	if _, err := os.Stat(b1[0].OutPath); err != nil {
		t.Fatalf("cache wasm not written: %v", err)
	}
	_, b2, err := resolveOpRefsColocated(ops, map[string]oprefs.Operation{}, root, io.Discard)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if b1[0].Digest != b2[0].Digest {
		t.Fatalf("digest changed across builds: %s vs %s", b1[0].Digest, b2[0].Digest)
	}
}

// TestResolveOpRefsColocatedBuildsTS: a colocated NAME.ts is transpiled by
// esbuild and resolves to a compute ref (TypeScript is first-class).
func TestResolveOpRefsColocatedBuildsTS(t *testing.T) {
	if _, err := exec.LookPath("javy"); err != nil {
		t.Skip("javy not on PATH")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "OPS/site/100/calc.txcl"), `EXEC "op://calc"`)
	writeFile(t, filepath.Join(root, "OPS/site/100/calc.ts"), `import { op } from "@txco/op";
interface In { n: number }
export default op<In>(({ input }) => ({ doubled: input.n * 2 }));`)
	ops, _ := bundle.Walk(root)
	sub, built, err := resolveOpRefsColocated(ops, map[string]oprefs.Operation{}, root, io.Discard)
	if err != nil {
		t.Fatalf("resolve .ts: %v", err)
	}
	if len(built) != 1 || !strings.Contains(sub[0].Txcl, "compute://sha256/") {
		t.Fatalf("ts compute not resolved: built=%d txcl=%q", len(built), sub[0].Txcl)
	}
}
