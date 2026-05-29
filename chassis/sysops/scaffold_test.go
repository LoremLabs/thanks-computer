package sysops

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScaffoldWritesEmbeddedDefault(t *testing.T) {
	dest := t.TempDir() // workspace root
	wrote, err := Scaffold(dest, false)
	if err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	if !wrote {
		t.Fatalf("Scaffold reported nothing written into an empty workspace")
	}
	for _, rel := range []string{"OPS/_sys/boot/0/detect.txcl", "OPS/_sys/boot/0/healthz.txcl", "OPS/_sys/boot/100/route.txcl", "OPS/_sys/boot/1000/notfound.txcl"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			t.Errorf("expected scaffolded %s: %v", rel, err)
		}
	}
}

func TestScaffoldNoClobberWithoutForce(t *testing.T) {
	dest := t.TempDir() // workspace root
	if _, err := Scaffold(dest, false); err != nil {
		t.Fatalf("Scaffold #1: %v", err)
	}
	edited := filepath.Join(dest, "OPS", "_sys", "boot", "0", "detect.txcl")
	if err := os.WriteFile(edited, []byte("# my edit\n"), 0o644); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// Per-file no-clobber: a non-force scaffold must not touch the
	// existing (edited) file.
	wrote, err := Scaffold(dest, false)
	if err != nil {
		t.Fatalf("Scaffold #2: %v", err)
	}
	if wrote {
		t.Errorf("non-force Scaffold rewrote an existing file")
	}
	b, _ := os.ReadFile(edited)
	if string(b) != "# my edit\n" {
		t.Errorf("operator edit clobbered: %q", string(b))
	}

	// Force re-scaffolds, restoring the embedded content.
	if _, err := Scaffold(dest, true); err != nil {
		t.Fatalf("Scaffold force: %v", err)
	}
	b, _ = os.ReadFile(edited)
	if string(b) == "# my edit\n" {
		t.Errorf("--force did not overwrite the edited file")
	}
}
