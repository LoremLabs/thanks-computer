package source

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractTarNoStrip locks the OCI path: a tar with NO synthetic
// `<repo>-<sha>/` top dir must land at its literal paths (stripTopDir=false),
// not have its real top directory eaten.
func TestExtractTarNoStrip(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	entries := map[string]string{
		"txco.package.yaml":       "kind: Package",
		"OPS/support/0100/r.txcl": `EXEC "x"`,
	}
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	n, err := extractTar(tar.NewReader(&buf), false, "", dest)
	if err != nil {
		t.Fatalf("extractTar: %v", err)
	}
	if n != 2 {
		t.Errorf("wrote %d files, want 2", n)
	}
	for rel, want := range entries {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("missing %s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}
