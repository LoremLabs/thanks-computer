package filestore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
)

func TestSanitizeSegNeutralizesDotSegments(t *testing.T) {
	for in, want := range map[string]string{
		"":             "_",
		".":            "_.",
		"..":           "_..",
		"...":          "_...",
		"/":            "_", // becomes "" after Trim("/"), but callers split first
		"a..b":         "a..b",
		"in.json":      "in.json",
		"hello-world":  "hello-world",
		"../etc":       ".._etc", // '/' -> '_', not a pure-dot segment
		"normal_seg-1": "normal_seg-1",
	} {
		if got := sanitizeSeg(in); got != want {
			t.Errorf("sanitizeSeg(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCreateCannotEscapeRoot drives the public API with a key whose
// stage segment (stack name) is a crafted traversal string and asserts
// the write stays inside the store root.
func TestCreateCannotEscapeRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	fs, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Mirrors a real continuation key: runs/<id>/stages/<stage>/...
	// where <stage> = "<stackName>/<scope>" and stackName is the
	// attacker-controlled "../../../../../../tmp/pwn".
	key := "runs/RID/stages/../../../../../../tmp/pwn/300/ops/0001-x/op-created.json"

	if _, err := fs.Create(context.Background(), key, strings.NewReader(`{"x":1}`), continuation.Meta{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Nothing may exist at /tmp/pwn or anywhere outside root.
	if _, err := os.Stat("/tmp/pwn"); err == nil {
		t.Fatalf("traversal: /tmp/pwn was created outside the store root")
	}

	// Every file actually written must live under root.
	parent := filepath.Dir(root)
	_ = filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rootAbs, _ := filepath.Abs(root)
		pAbs, _ := filepath.Abs(p)
		if !strings.HasPrefix(pAbs, rootAbs+string(os.PathSeparator)) {
			t.Errorf("file written outside root: %s", pAbs)
		}
		return nil
	})
}
