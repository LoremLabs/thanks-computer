package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tarGzNames gunzips+untars blob and returns the slash-separated entry names.
func tarGzNames(t *testing.T, blob []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		names = append(names, h.Name)
	}
	return names
}

// TestTarGzDirExcludesGitAndTxco pins the publish-time exclusion: `.git` (VCS
// history) and `.txco` (local build cache) never ship, at any depth, while
// regular package files — including dotfiles that aren't .git/.txco — do.
func TestTarGzDirExcludesGitAndTxco(t *testing.T) {
	dir := t.TempDir()
	writes := map[string]string{
		"txco.package.yaml":              "name: demo\n",
		"OPS/support/classify.txcl":      "rule\n",
		".env":                           "SECRET=keepme", // a dotfile that is NOT .git/.txco → ships
		".git/config":                    "[core]\n",
		".git/objects/ab/cdef":           "blob",
		".txco/compute/classify.wasm":    "\x00asm",
		"OPS/support/.txco/nested-cache": "junk", // .txco at depth is skipped too
	}
	for rel, body := range writes {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	blob, err := tarGzDir(dir)
	if err != nil {
		t.Fatalf("tarGzDir: %v", err)
	}
	got := map[string]bool{}
	for _, n := range tarGzNames(t, blob) {
		got[n] = true
	}

	wantPresent := []string{"txco.package.yaml", "OPS/support/classify.txcl", ".env"}
	for _, w := range wantPresent {
		if !got[w] {
			t.Errorf("expected %q in layer, absent", w)
		}
	}
	for n := range got {
		for _, seg := range strings.Split(n, "/") {
			if seg == ".git" || seg == ".txco" {
				t.Errorf("excluded dir content shipped: %q", n)
			}
		}
	}
}
