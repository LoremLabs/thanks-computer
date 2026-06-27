package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitDirTarget(t *testing.T) {
	// An existing directory for the "bare name that is a dir" case.
	tmp := t.TempDir()
	sub := filepath.Join(tmp, "existingdir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		args       []string
		wantDir    string
		wantTarget string
	}{
		{"empty", nil, "", ""},
		{"bare name → target", []string{"staging"}, "", "staging"},
		{"dot → dir", []string{"."}, ".", ""},
		{"relative path → dir", []string{"./sub"}, "./sub", ""},
		{"parent path → dir", []string{"../repo"}, "../repo", ""},
		{"abs path → dir", []string{"/abs/path"}, "/abs/path", ""},
		{"contains slash → dir", []string{"a/b"}, "a/b", ""},
		{"dir then target", []string{"./sub", "staging"}, "./sub", "staging"},
		{"target then dir", []string{"staging", "./sub"}, "./sub", "staging"},
		{"existing dir (bare) → dir", []string{sub}, sub, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, tgt := splitDirTarget(c.args)
			if d != c.wantDir || tgt != c.wantTarget {
				t.Errorf("splitDirTarget(%v) = (%q, %q), want (%q, %q)", c.args, d, tgt, c.wantDir, c.wantTarget)
			}
		})
	}
}
