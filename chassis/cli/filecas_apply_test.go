package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectFileAssets(t *testing.T) {
	stackDir := t.TempDir()
	mk := func(rel, body string) {
		p := filepath.Join(stackDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("FILES/index.html", "<h1>hi</h1>")
	mk("FILES/assets/app.css", "body{}")
	mk("FILES/_mail/welcome.html", "tmpl") // "_" is allowed (privacy is a serve-time concern)
	mk("FILES/.hidden", "secret")          // dotfile → skipped
	mk("100/resonator.txcl", "EMIT .x=1")  // not under FILES/ → not collected
	mk("mock-request.json", "{}")          // not under FILES/ → not collected

	got, err := collectFileAssets(stackDir)
	if err != nil {
		t.Fatalf("collectFileAssets: %v", err)
	}
	byPath := map[string]string{}
	for _, f := range got {
		byPath[f.Path] = f.Content
	}

	want := map[string]string{
		"FILES/index.html":         "<h1>hi</h1>",
		"FILES/assets/app.css":     "body{}",
		"FILES/_mail/welcome.html": "tmpl",
	}
	for p, c := range want {
		if byPath[p] != c {
			t.Errorf("path %q = %q, want %q", p, byPath[p], c)
		}
	}
	if len(got) != len(want) {
		t.Errorf("collected %d files %v, want %d (dotfiles + non-FILES excluded)", len(got), keysOf(byPath), len(want))
	}
	if _, ok := byPath["FILES/.hidden"]; ok {
		t.Error("dotfile must be skipped")
	}
}

func TestCollectFileAssetsNoFilesDir(t *testing.T) {
	// A stack with no FILES/ dir → nil, no error.
	got, err := collectFileAssets(t.TempDir())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
