package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writeModes makes a package tree with one executable and one plain file.
func writeModes(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for rel, mode := range map[string]os.FileMode{"OPS/s/bin/tool": 0o755, "OPS/s/0100/a.txcl": 0o644, "OPS/s/lib/x.py": 0o640} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(rel), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil { // past the umask
			t.Fatal(err)
		}
	}
	return dir
}

func wantModes(t *testing.T, dir string) {
	t.Helper()
	for rel, want := range map[string]os.FileMode{"OPS/s/bin/tool": 0o755, "OPS/s/0100/a.txcl": 0o644, "OPS/s/lib/x.py": 0o644} {
		st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", rel, got, want)
		}
	}
}

// A package records which files are executable, and an install restores it:
// a program shipped in a stack must still run from $TXCO_STACK_DIR. Nothing
// but the executable bit travels (0640 installs as 0644).
func TestPackageKeepsTheExecutableBit(t *testing.T) {
	blob, err := tarGzDir(writeModes(t))
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := extractTar(tar.NewReader(gz), false, "", dest); err != nil {
		t.Fatal(err)
	}
	wantModes(t, dest)
}

func TestDirSourceKeepsTheExecutableBit(t *testing.T) {
	dest := t.TempDir()
	if _, err := Fetch(context.Background(), "dir:"+writeModes(t), dest); err != nil {
		t.Fatal(err)
	}
	wantModes(t, dest)
}
