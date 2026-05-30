package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
)

func mustMkWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writePkg lays a valid single-stack package (1 bundled + 1 required op) whose
// internal stack is `stack`, under dir.
func writePkg(t *testing.T, dir, stack string) {
	t.Helper()
	mustMkWrite(t, filepath.Join(dir, "txco.package.yaml"), `apiVersion: thanks.computer/v1alpha1
kind: Package
name: support-basic
version: 0.1.0
package:
  kind: department
operations:
  bundled:
    - name: classify
      path: OPS/`+stack+`/0100/classify.js
  required:
    - name: NOTIFY
      kind: http
      example: https://notify.example.com/op
capabilities:
  - http.fetch
`)
	mustMkWrite(t, filepath.Join(dir, "OPS", stack, "0100", "classify.txcl"), `EXEC "op://classify"`)
	mustMkWrite(t, filepath.Join(dir, "OPS", stack, "0100", "classify.js"), `export default () => ({})`)
	mustMkWrite(t, filepath.Join(dir, "OPS", stack, "0200", "notify.txcl"), `EXEC "op://NOTIFY"`)
}

func TestInstallDryRun(t *testing.T) {
	pkg := t.TempDir()
	writePkg(t, pkg, "support")
	ws := t.TempDir()
	t.Chdir(ws)

	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + pkg, "--as", "support", "--dry-run"}, &out, &errb); code != 0 {
		t.Fatalf("dry-run code=%d stderr=%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS")); !os.IsNotExist(err) {
		t.Error("dry-run created OPS/")
	}
	if _, err := os.Stat(filepath.Join(ws, lockfile.FileName)); !os.IsNotExist(err) {
		t.Error("dry-run wrote a lockfile")
	}
	s := out.String()
	if !strings.Contains(s, "dry-run") || !strings.Contains(s, "OPS/support/0100/classify.txcl") {
		t.Errorf("dry-run output unexpected:\n%s", s)
	}
	if !strings.Contains(s, "NOTIFY") {
		t.Errorf("dry-run should print the required-op stub:\n%s", s)
	}
}

func TestInstallAsRename(t *testing.T) {
	pkg := t.TempDir()
	writePkg(t, pkg, "cse") // internal stack differs from --as target
	ws := t.TempDir()
	t.Chdir(ws)

	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + pkg, "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("install code=%d stderr=%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support", "0100", "classify.txcl")); err != nil {
		t.Errorf("materialized file missing (rename cse->support failed): %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS", "cse")); !os.IsNotExist(err) {
		t.Error("OPS/cse/ should not exist after --as support")
	}
	lf, err := lockfile.Read(ws)
	if err != nil || len(lf.Packages) != 1 {
		t.Fatalf("lockfile: err=%v packages=%+v", err, lf.Packages)
	}
	e := lf.Packages[0]
	if e.ExportedStack != "cse" || e.InstalledAs != "support" || e.Mode != "as-stack" {
		t.Errorf("lock entry: %+v", e)
	}
	if e.ManifestHash == "" || e.InstalledAt == "" {
		t.Errorf("lock entry missing hash/time: %+v", e)
	}
}

func TestInstallRejectsMultiStack(t *testing.T) {
	pkg := t.TempDir()
	mustMkWrite(t, filepath.Join(pkg, "txco.package.yaml"), `apiVersion: thanks.computer/v1alpha1
kind: Package
name: multi
version: 0.1.0
`)
	mustMkWrite(t, filepath.Join(pkg, "OPS", "a", "0100", "r.txcl"), `EMIT .x = "1"`)
	mustMkWrite(t, filepath.Join(pkg, "OPS", "b", "0100", "r.txcl"), `EMIT .x = "1"`)
	ws := t.TempDir()
	t.Chdir(ws)

	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + pkg, "--as", "support"}, &out, &errb); code == 0 {
		t.Fatal("expected non-zero for multi-stack package")
	}
	if !strings.Contains(errb.String(), "single-stack") {
		t.Errorf("stderr should explain single-stack restriction: %s", errb.String())
	}
}

func TestInstallVendorOnly(t *testing.T) {
	pkg := t.TempDir()
	writePkg(t, pkg, "support")
	ws := t.TempDir()
	t.Chdir(ws)

	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + pkg, "--vendor-only"}, &out, &errb); code != 0 {
		t.Fatalf("vendor-only code=%d stderr=%s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(ws, ".txco", "vendor", "support-basic", "0.1.0", "txco.package.yaml")); err != nil {
		t.Errorf("vendored manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "OPS")); !os.IsNotExist(err) {
		t.Error("vendor-only must not create OPS/")
	}
	lf, _ := lockfile.Read(ws)
	if len(lf.Packages) != 1 || lf.Packages[0].Mode != "vendor-only" {
		t.Errorf("lock entry: %+v", lf.Packages)
	}
}

func TestInstallEmptyDirGuard(t *testing.T) {
	pkg := t.TempDir()
	writePkg(t, pkg, "support")
	ws := t.TempDir()
	// Pre-populate OPS/support with untracked content (no lockfile entry).
	mustMkWrite(t, filepath.Join(ws, "OPS", "support", "0100", "existing.txcl"), `EMIT .x = "1"`)
	t.Chdir(ws)

	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + pkg, "--as", "support"}, &out, &errb); code == 0 {
		t.Fatal("expected install to refuse clobbering untracked OPS/support/")
	}
	// The untracked file must survive.
	if _, err := os.Stat(filepath.Join(ws, "OPS", "support", "0100", "existing.txcl")); err != nil {
		t.Errorf("untracked file was removed: %v", err)
	}
}

func TestInstallIdempotent(t *testing.T) {
	pkg := t.TempDir()
	writePkg(t, pkg, "support")
	ws := t.TempDir()
	t.Chdir(ws)

	var out, errb bytes.Buffer
	if code := runInstall([]string{"dir:" + pkg, "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("first install failed: %s", errb.String())
	}
	lockBytes1, _ := os.ReadFile(filepath.Join(ws, lockfile.FileName))

	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"dir:" + pkg, "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("re-install failed: %s", errb.String())
	}
	if !strings.Contains(out.String(), "no change") {
		t.Errorf("re-install should report no change, got:\n%s", out.String())
	}
	lockBytes2, _ := os.ReadFile(filepath.Join(ws, lockfile.FileName))
	if string(lockBytes1) != string(lockBytes2) {
		t.Error("re-install changed the lockfile despite no content change")
	}
}

// TestExamplePackageInstalls guards that the shipped example package stays
// valid and installs end-to-end (including its bundled compute riding along).
func TestExamplePackageInstalls(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "..", "examples", "packages", "support-basic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture, "txco.package.yaml")); err != nil {
		t.Skipf("example package not found at %s: %v", fixture, err)
	}

	var out, errb bytes.Buffer
	if code := runPackage([]string{"validate", fixture}, &out, &errb); code != 0 {
		t.Fatalf("example package failed validation:\n%s", errb.String())
	}

	ws := t.TempDir()
	t.Chdir(ws)
	out.Reset()
	errb.Reset()
	if code := runInstall([]string{"dir:" + fixture, "--as", "support"}, &out, &errb); code != 0 {
		t.Fatalf("install failed:\n%s", errb.String())
	}
	for _, rel := range []string{
		"OPS/support/0000_SETUP/audit.txcl",
		"OPS/support/0100_TRIAGE/classify.txcl",
		"OPS/support/0100_TRIAGE/classify.js", // bundled compute rides along
		"OPS/support/0200_NOTIFY/notify.txcl",
		lockfile.FileName,
	} {
		if _, err := os.Stat(filepath.Join(ws, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected %s after install: %v", rel, err)
		}
	}
	if !strings.Contains(out.String(), "AUDIT") || !strings.Contains(out.String(), "NOTIFY") {
		t.Errorf("install should surface required ops AUDIT/NOTIFY:\n%s", out.String())
	}
}

func TestParseRegistryConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "txco.yaml")
	mustMkWrite(t, p, `target: dev
registry:
  default: registry.thanks.computer
  defaultNamespace: txco
  aliases:
    ghcr: ghcr.io/loremlabs
`)
	cfg, err := parseConfigFile(p)
	if err != nil {
		t.Fatalf("parseConfigFile: %v", err)
	}
	if cfg.Registry.Default != "registry.thanks.computer" {
		t.Errorf("default = %q", cfg.Registry.Default)
	}
	if cfg.Registry.DefaultNamespace != "txco" {
		t.Errorf("defaultNamespace = %q", cfg.Registry.DefaultNamespace)
	}
	if cfg.Registry.Aliases["ghcr"] != "ghcr.io/loremlabs" {
		t.Errorf("alias = %q", cfg.Registry.Aliases["ghcr"])
	}
}
