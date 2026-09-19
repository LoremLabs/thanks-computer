package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// `txco draft` reads stack files directly rather than through the walker,
// so it expands &include itself; other files go up as they are.
func TestDraftExpandsIncludes(t *testing.T) {
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
	write("OPS/app/0100_RUN/run.txcl", `EMIT .s = &include("s.sh")`)
	write("OPS/app/0100_RUN/s.sh", "echo \"x\"\n")

	files, err := collectStackFiles(filepath.Join(root, "OPS", "app"))
	if err != nil {
		t.Fatal(err)
	}
	if err := expandStackIncludes(root, "app", files); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Content
	}
	if want := `EMIT .s = "echo \"x\"\n"`; got["0100_RUN/run.txcl"] != want {
		t.Errorf("run.txcl = %s, want %s", got["0100_RUN/run.txcl"], want)
	}
	if got["0100_RUN/s.sh"] != "echo \"x\"\n" {
		t.Errorf("s.sh = %q", got["0100_RUN/s.sh"])
	}

	write("OPS/app/0100_RUN/bad.txcl", `EMIT .s = &include("nope.sh")`)
	files, _ = collectStackFiles(filepath.Join(root, "OPS", "app"))
	if err := expandStackIncludes(root, "app", files); err == nil {
		t.Error("missing include: want an error")
	}
}
