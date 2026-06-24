package cli

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestLoadLocalStackFilesMatchesPushManifest locks the cleanliness-check fix:
// loadLocalStackFiles must reconstruct a stack's file set EXACTLY as the push
// uploads it — opsToFiles over the WALKER's normalized ops (numeric scope +
// flattened name) plus the stack's own FILES/**. Reading raw disk paths instead
// made every labeled-scope-dir (`<NNNN>_LABEL/`), nested-op, or FILES-bearing
// stack falsely read "edited since pull" right after a clean push. The test uses
// a labeled dir, a nested op, FILES/, and a nested sub-stack (which must NOT
// leak into the parent's set).
func TestLoadLocalStackFilesMatchesPushManifest(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Labeled scope dir → normalizes to numeric scope 100.
	write("OPS/www/0100_REDIRECT/redirect.txcl", "WHEN @x\n  EMIT @y = 1\n")
	// Op nested below the scope dir → flattened name "sub_extra".
	write("OPS/www/0100_REDIRECT/sub/extra.txcl", "WHEN @z\n  EMIT @w = 1\n")
	// The stack's own static assets.
	write("OPS/www/FILES/index.html", "<!doctype html><title>dripl.it</title>")
	write("OPS/www/FILES/app/immutable/a.js", "export const x = 1")
	// A nested sub-stack (www/_mail): its rules AND its own FILES/ must NOT leak
	// into www's file set.
	write("OPS/www/_mail/0100_X/inner.txcl", "WHEN @m\n")
	write("OPS/www/_mail/FILES/inner.html", "nested asset")

	files, err := loadLocalStackFiles(root, "www")
	if err != nil {
		t.Fatalf("loadLocalStackFiles: %v", err)
	}
	got := make([]string, 0, len(files))
	for _, f := range files {
		got = append(got, f.Path)
	}
	sort.Strings(got)

	// Paths are the push's normalized form: numeric scope, flattened op names,
	// FILES/ included, nested sub-stack absent.
	want := []string{
		"100/redirect.txcl",
		"100/sub_extra.txcl",
		"FILES/app/immutable/a.js",
		"FILES/index.html",
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths = %v, want %v", got, want)
		}
	}
}
