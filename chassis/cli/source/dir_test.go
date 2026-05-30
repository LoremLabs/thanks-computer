package source

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseDirSource(t *testing.T) {
	dir := t.TempDir()

	s, err := Parse("dir:" + dir)
	if err != nil {
		t.Fatalf("Parse dir: %v", err)
	}
	if s.Spec() != "dir:"+dir {
		t.Errorf("Spec = %q, want %q", s.Spec(), "dir:"+dir)
	}

	// file: is an alias for dir:.
	if _, err := Parse("file:" + dir); err != nil {
		t.Errorf("Parse file: %v", err)
	}
	// Empty path.
	if _, err := Parse("dir:"); err == nil {
		t.Error("expected error for empty dir path")
	}
	// Nonexistent path.
	if _, err := Parse("dir:" + filepath.Join(dir, "nope")); err == nil {
		t.Error("expected error for missing dir")
	}
	// A file, not a directory.
	f := filepath.Join(dir, "afile")
	mustWrite(t, f, "x")
	if _, err := Parse("dir:" + f); err == nil {
		t.Error("expected error for non-directory")
	}
}

func TestDirSourceFetch(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "txco.package.yaml"), "kind: Package\n")
	mustWrite(t, filepath.Join(src, "OPS", "support", "100", "r.txcl"), `EXEC "x"`)
	mustWrite(t, filepath.Join(src, "README.md"), "hi")

	dest := t.TempDir()
	n, err := Fetch(context.Background(), "dir:"+src, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 3 {
		t.Errorf("got %d files, want 3", n)
	}
	for rel, want := range map[string]string{
		"txco.package.yaml":      "kind: Package\n",
		"OPS/support/100/r.txcl": `EXEC "x"`,
		"README.md":              "hi",
	} {
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

func TestDirSourceSkipsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks differ on windows")
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "real.txcl"), "real")
	if err := os.Symlink(filepath.Join(src, "real.txcl"), filepath.Join(src, "link.txcl")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	dest := t.TempDir()
	n, err := Fetch(context.Background(), "dir:"+src, dest)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if n != 1 {
		t.Errorf("got %d files, want 1 (symlink skipped)", n)
	}
	if _, err := os.Lstat(filepath.Join(dest, "link.txcl")); err == nil {
		t.Error("symlink leaked into dest")
	}
}
