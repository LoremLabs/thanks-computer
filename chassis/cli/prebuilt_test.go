package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
	"github.com/loremlabs/thanks-computer/chassis/cli/oprefs"
)

// TestResolveOpRefsColocatedPrefersWasm: a prebuilt <name>.wasm sibling is used
// directly (no javy), the op://NAME ref is substituted to compute://sha256/<hex>,
// and the built list carries the exact wasm bytes. Fully hermetic — no toolchain.
func TestResolveOpRefsColocatedPrefersWasm(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "OPS", "support", "0100_TRIAGE")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A rule referencing op://classify, with a prebuilt classify.wasm sibling.
	if err := os.WriteFile(filepath.Join(dir, "classify.txcl"), []byte(`EXEC "op://classify"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wasm := []byte("\x00asm\x01\x00\x00\x00prebuilt classify")
	if err := os.WriteFile(filepath.Join(dir, "classify.wasm"), wasm, 0o644); err != nil {
		t.Fatal(err)
	}
	// A .js sibling also present — the .wasm must win (would need javy otherwise).
	if err := os.WriteFile(filepath.Join(dir, "classify.js"), []byte("export default () => ({})"), 0o644); err != nil {
		t.Fatal(err)
	}

	ops, err := bundle.WalkFS(os.DirFS(root), ".")
	if err != nil || len(ops) != 1 {
		t.Fatalf("walk: err=%v ops=%d", err, len(ops))
	}

	var errb bytes.Buffer
	out, built, err := resolveOpRefsColocated(ops, map[string]oprefs.Operation{}, root, &errb)
	if err != nil {
		t.Fatalf("resolveOpRefsColocated: %v", err)
	}

	sum := sha256.Sum256(wasm)
	wantRef := "compute://sha256/" + hex.EncodeToString(sum[:])
	if len(built) != 1 {
		t.Fatalf("want 1 built compute, got %d", len(built))
	}
	if built[0].Ref != wantRef {
		t.Errorf("built ref = %q, want %q", built[0].Ref, wantRef)
	}
	if !bytes.Equal(built[0].Wasm, wasm) {
		t.Error("built wasm bytes should be the prebuilt file's bytes")
	}
	if !bytes.Contains([]byte(out[0].Txcl), []byte(wantRef)) {
		t.Errorf("op://classify should be substituted to %s; got txcl: %q", wantRef, out[0].Txcl)
	}
}

// TestPublishPrebuildRoundTrip: publish (with the prebuild seam faked, so no
// real javy) bakes a <name>.wasm into the artifact; install materializes it next
// to the .js. Hermetic via an in-process OCI store.
func TestPublishPrebuildRoundTrip(t *testing.T) {
	fixture := absFixture(t) // support-basic: bundled classify.js
	sharedStore(t)

	// Fake the toolchain: write deterministic "wasm" bytes for each bundled op.
	prev := prebuildComputes
	prebuildComputes = func(stagingDir, srcDir string, bundled []manifest.BundledOp) (int, bool, error) {
		for _, b := range bundled {
			wasmRel := strings.TrimSuffix(b.Path, filepath.Ext(b.Path)) + ".wasm"
			dst := filepath.Join(stagingDir, wasmRel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return 0, false, err
			}
			if err := os.WriteFile(dst, []byte("\x00asm\x01\x00\x00\x00"+b.Name), 0o644); err != nil {
				return 0, false, err
			}
		}
		return len(bundled), false, nil
	}
	t.Cleanup(func() { prebuildComputes = prev })

	const ref = "oci://registry.thanks.computer/txco/support-basic:0.1.0"
	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", ref, fixture}, &out, &errb); code != 0 {
		t.Fatalf("publish: %s", errb.String())
	}
	if !strings.Contains(out.String(), "prebuilt 1 compute") {
		t.Errorf("expected prebuilt note, got: %s", out.String())
	}

	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"support-basic@0.1.0", "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("install: %s", errb.String())
	}
	// The prebuilt wasm rides along and materializes next to the .js source.
	js := filepath.Join(ws, "OPS", "support", "0100_TRIAGE", "classify.js")
	wasm := filepath.Join(ws, "OPS", "support", "0100_TRIAGE", "classify.wasm")
	if _, err := os.Stat(js); err != nil {
		t.Errorf("source .js should still ship: %v", err)
	}
	if _, err := os.Stat(wasm); err != nil {
		t.Errorf("prebuilt .wasm should materialize: %v", err)
	}
	// With a prebuilt wasm present, install must NOT warn about needing javy.
	if strings.Contains(out.String()+errb.String(), "needs `javy`") {
		t.Errorf("should not warn about javy when wasm is prebuilt; got: %s / %s", out.String(), errb.String())
	}
}

func TestWarnBundledComputesSuppressedByWasm(t *testing.T) {
	m := &manifest.Manifest{Operations: manifest.Operations{Bundled: []manifest.BundledOp{
		{Name: "classify", Path: "OPS/s/0100/classify.js"},
	}}}
	staging := t.TempDir()
	if err := os.MkdirAll(filepath.Join(staging, "OPS", "s", "0100"), 0o755); err != nil {
		t.Fatal(err)
	}

	// No wasm sibling → warns only if javy is absent. Assert the message mentions
	// the count when it fires; if javy happens to be installed it stays silent.
	var w bytes.Buffer
	warnBundledComputes(m, staging, &w)
	if strings.Contains(w.String(), "1 bundled compute") == false && w.Len() != 0 {
		t.Errorf("unexpected warning content: %q", w.String())
	}

	// With a prebuilt wasm sibling → never warns, regardless of javy.
	if err := os.WriteFile(filepath.Join(staging, "OPS", "s", "0100", "classify.wasm"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Reset()
	warnBundledComputes(m, staging, &w)
	if w.Len() != 0 {
		t.Errorf("prebuilt wasm must suppress the javy warning; got: %q", w.String())
	}
}
