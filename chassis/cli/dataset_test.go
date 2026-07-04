package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectDatasetFiles(t *testing.T) {
	stackDir := t.TempDir()
	dsDir := filepath.Join(stackDir, "DATASETS")
	if err := os.MkdirAll(dsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Binary-ish artifact bytes (never valid UTF-8 in real life; the
	// collector must not care — it hashes streaming, no content).
	artifact := []byte{0x53, 0x51, 0x4c, 0x69, 0x74, 0x65, 0x00, 0xff, 0xfe}
	if err := os.WriteFile(filepath.Join(dsDir, "books.sqlite"), artifact, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := "queries:\n  q:\n    sql: SELECT 1\n"
	if err := os.WriteFile(filepath.Join(dsDir, "books.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	files, uploads, err := collectDatasetFiles(stackDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || len(uploads) != 1 {
		t.Fatalf("files=%d uploads=%d", len(files), len(uploads))
	}
	sum := sha256.Sum256(artifact)
	wantHash := hex.EncodeToString(sum[:])
	var sawArtifact, sawManifest bool
	for _, f := range files {
		switch f.Path {
		case "DATASETS/books.sqlite":
			sawArtifact = true
			if f.Content != "" || f.Encoding != "cas" || f.ContentHash != wantHash {
				t.Fatalf("artifact row: %+v", f)
			}
		case "DATASETS/books.yaml":
			sawManifest = true
			if f.Content != manifest || f.Encoding != "" || f.ContentHash == "" {
				t.Fatalf("manifest row: %+v", f)
			}
		}
	}
	if !sawArtifact || !sawManifest {
		t.Fatalf("missing rows: %+v", files)
	}
	u := uploads[0]
	if u.Hash != wantHash || u.Size != int64(len(artifact)) || u.Path != "DATASETS/books.sqlite" {
		t.Fatalf("upload: %+v", u)
	}

	// Absent DATASETS/ → all nil, no error.
	if f, up, err := collectDatasetFiles(t.TempDir()); f != nil || up != nil || err != nil {
		t.Fatalf("absent dir: %v %v %v", f, up, err)
	}

	// Foreign extensions + nesting are fail-fast (the server would reject
	// them at the write boundary anyway).
	if err := os.WriteFile(filepath.Join(dsDir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectDatasetFiles(stackDir); err == nil {
		t.Fatal("foreign extension accepted")
	}
	_ = os.Remove(filepath.Join(dsDir, "notes.txt"))
	if err := os.MkdirAll(filepath.Join(dsDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectDatasetFiles(stackDir); err == nil {
		t.Fatal("nested dir accepted")
	}
}
