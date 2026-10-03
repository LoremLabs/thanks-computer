package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/oci"

	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// TestPackagePublishInstallRoundTrip exercises the full OCI path at the CLI
// level: `package publish` into an in-process OCI store, then `install
// <bare-ref>` (resolved via the baked default registry) pulling from the same
// store, asserting the lockfile pins the digest. No registry, no network.
func TestPackagePublishInstallRoundTrip(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "examples", "packages", "support-basic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "txco.package.yaml")); err != nil {
		t.Skipf("example package not found: %v", err)
	}

	// Keep this round trip network-free and deterministic: an isolated home
	// (no pre-cached toolchain) plus disabled auto-download means the op-build
	// path finds no javy, so publish ships the bundled computes source-only —
	// the path this test asserts — instead of fetching/using a toolchain.
	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("TXCO_JAVY_NO_DOWNLOAD", "1")

	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Both the push and pull factories point at the same in-process store.
	prevPush := source.SetPushRepositoryFactory(func(string) (oras.Target, error) { return store, nil })
	prevPull := source.SetRepositoryFactory(func(string) (oras.ReadOnlyTarget, error) { return store, nil })
	t.Cleanup(func() { source.SetPushRepositoryFactory(prevPush); source.SetRepositoryFactory(prevPull) })

	// publish
	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", "oci://registry.thanks.computer/txco/support-basic:0.1.0", fixture}, &out, &errb); code != 0 {
		t.Fatalf("publish failed: %s", errb.String())
	}
	if !strings.Contains(out.String(), "@sha256:") {
		t.Errorf("publish should print the resolved digest, got: %s", out.String())
	}

	// install via a BARE ref (baked default registry → registry.thanks.computer/txco/...)
	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"support-basic@0.1.0", "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("install from registry failed: %s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support", "0100_TRIAGE", "classify.txcl")); err != nil {
		t.Errorf("package not materialized: %v", err)
	}

	lf, err := lockfile.Read(ws)
	if err != nil || len(lf.Packages) != 1 {
		t.Fatalf("lockfile: err=%v packages=%+v", err, lf.Packages)
	}
	e := lf.Packages[0]
	if e.Ref != "support-basic@0.1.0" {
		t.Errorf("Ref = %q, want the raw user ref", e.Ref)
	}
	if e.Registry != "registry.thanks.computer" || e.Namespace != "txco" {
		t.Errorf("provenance = registry=%q namespace=%q", e.Registry, e.Namespace)
	}
	if !strings.Contains(e.Resolved, "@sha256:") {
		t.Errorf("Resolved not digest-pinned: %q", e.Resolved)
	}
}

// A program shipped in a package keeps its executable bit through publish,
// the registry and install, so it still runs from $TXCO_STACK_DIR.
func TestPackageRoundTripKeepsTheExecutableBit(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "examples", "packages", "support-basic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "txco.package.yaml")); err != nil {
		t.Skipf("example package not found: %v", err)
	}
	pkg := t.TempDir()
	if _, err := copyTree(fixture, pkg); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(pkg, "OPS", "support", "bin", "tool")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TXCO_HOME", t.TempDir())
	t.Setenv("TXCO_JAVY_NO_DOWNLOAD", "1")
	store, err := oci.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevPush := source.SetPushRepositoryFactory(func(string) (oras.Target, error) { return store, nil })
	prevPull := source.SetRepositoryFactory(func(string) (oras.ReadOnlyTarget, error) { return store, nil })
	t.Cleanup(func() { source.SetPushRepositoryFactory(prevPush); source.SetRepositoryFactory(prevPull) })

	var out, errb bytes.Buffer
	if code := runPackage([]string{"publish", "--to", "oci://registry.thanks.computer/txco/support-basic:0.1.0", pkg}, &out, &errb); code != 0 {
		t.Fatalf("publish failed: %s", errb.String())
	}
	ws := t.TempDir()
	t.Chdir(ws)
	if code := runInstall([]string{"support-basic@0.1.0", "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("install failed: %s", errb.String())
	}
	for rel, want := range map[string]os.FileMode{"bin/tool": 0o755, "0100_TRIAGE/classify.txcl": 0o644} {
		st, err := os.Stat(filepath.Join(ws, "OPS", "support", filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("installed %s mode = %o, want %o", rel, got, want)
		}
	}
}

// Re-installing over an existing file sets the new mode, both ways: WriteFile
// alone would keep the old one.
func TestCopyTreeSetsModesOnReinstall(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write := func(dir string, mode os.FileMode) {
		p := filepath.Join(dir, "tool")
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	mode := func() os.FileMode {
		st, err := os.Stat(filepath.Join(dst, "tool"))
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode().Perm()
	}
	write(src, 0o755)
	write(dst, 0o644)
	if _, err := copyTree(src, dst); err != nil || mode() != 0o755 {
		t.Fatalf("after install of an executable: %o, %v", mode(), err)
	}
	write(src, 0o644)
	if _, err := copyTree(src, dst); err != nil || mode() != 0o644 {
		t.Fatalf("after install of a plain file: %o, %v", mode(), err)
	}
}
