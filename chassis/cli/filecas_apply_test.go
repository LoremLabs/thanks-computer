package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
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

// A non-UTF-8 (binary) asset must travel base64-encoded so JSON's invalid-UTF-8 →
// U+FFFD rewrite can't mangle it; text stays raw. The hash is over the RAW bytes.
func TestCollectFileAssetsBinary(t *testing.T) {
	stackDir := t.TempDir()
	bin := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xC0} // JPEG-ish, invalid UTF-8
	jpgPath := filepath.Join(stackDir, "FILES", "covers", "x.jpg")
	if err := os.MkdirAll(filepath.Dir(jpgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jpgPath, bin, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stackDir, "FILES", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := collectFileAssets(stackDir)
	if err != nil {
		t.Fatalf("collectFileAssets: %v", err)
	}
	var jpg, txt *client.StackFile
	for i := range got {
		switch got[i].Path {
		case "FILES/covers/x.jpg":
			jpg = &got[i]
		case "FILES/a.txt":
			txt = &got[i]
		}
	}
	if jpg == nil || txt == nil {
		t.Fatalf("missing files in %+v", got)
	}
	// Binary → base64, decodes back to the EXACT bytes; ContentHash over raw bytes.
	if jpg.Encoding != "base64" {
		t.Errorf("binary Encoding = %q, want base64", jpg.Encoding)
	}
	dec, derr := base64.StdEncoding.DecodeString(jpg.Content)
	if derr != nil || !bytes.Equal(dec, bin) {
		t.Errorf("binary round-trip: decode err=%v equal=%v", derr, bytes.Equal(dec, bin))
	}
	wantHash := sha256.Sum256(bin)
	if jpg.ContentHash != hex.EncodeToString(wantHash[:]) {
		t.Errorf("ContentHash not over raw bytes: %q", jpg.ContentHash)
	}
	// Text → raw (JSON-safe as-is), no encoding flag.
	if txt.Encoding != "" || txt.Content != "hello" {
		t.Errorf("text file content/encoding = %q/%q, want hello/\"\"", txt.Content, txt.Encoding)
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
