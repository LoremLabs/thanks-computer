package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestCollectBlobFiles(t *testing.T) {
	stackDir := t.TempDir()
	mk := func(rel string, data []byte) {
		t.Helper()
		p := filepath.Join(stackDir, "BLOBS", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}
	mk("faqs/house-01.txt", []byte("house manual"))
	mk("media/logo.png", bin)
	mk("empty.bin", nil)
	mk(".hidden/x.txt", []byte("skip"))
	mk("faqs/.DS_Store", []byte("skip"))

	files, uploads, err := collectBlobFiles(stackDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || len(uploads) != 3 {
		t.Fatalf("files=%d uploads=%d (dotfiles must be skipped): %+v", len(files), len(uploads), files)
	}
	byPath := map[string]int{}
	for i, f := range files {
		byPath[f.Path] = i
		if f.Content != "" || f.Encoding != "cas" || f.ContentHash == "" {
			t.Fatalf("row must be fingerprint-only: %+v", f)
		}
	}
	sum := sha256.Sum256(bin)
	if i, ok := byPath["BLOBS/media/logo.png"]; !ok || files[i].ContentHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("nested binary row: %+v", files)
	}
	for _, u := range uploads {
		switch u.Path {
		case "BLOBS/empty.bin":
			if u.Size != 0 || u.Hash != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
				t.Fatalf("empty file upload: %+v", u)
			}
		case "BLOBS/media/logo.png":
			if u.Size != int64(len(bin)) || u.LocalPath == "" {
				t.Fatalf("upload: %+v", u)
			}
		}
	}

	// Absent tree → nil, nil, nil.
	if f, up, err := collectBlobFiles(t.TempDir()); f != nil || up != nil || err != nil {
		t.Fatalf("absent: %v %v %v", f, up, err)
	}

	// A file whose name is not a valid blob name fails fast, at the keyboard.
	mk("faqs/ho use.doc", []byte("x"))
	if _, _, err := collectBlobFiles(stackDir); err == nil {
		t.Fatal("invalid blob name accepted")
	}
	if err := os.Remove(filepath.Join(stackDir, "BLOBS", "faqs", "ho use.doc")); err != nil {
		t.Fatal(err)
	}
	mk("_meta/x.txt", []byte("x"))
	if _, _, err := collectBlobFiles(stackDir); err == nil {
		t.Fatal("reserved '_' segment accepted")
	}
}

func TestCollectStorePacksIncludesBlobs(t *testing.T) {
	stackDir := t.TempDir()
	for _, rel := range []string{"KV/cfg.jsonl", "BLOBS/faqs/a.txt"} {
		p := filepath.Join(stackDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"key":"k","value":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, uploads, err := collectStorePacks(stackDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || len(uploads) != 1 || uploads[0].Path != "BLOBS/faqs/a.txt" {
		t.Fatalf("files=%+v uploads=%+v", files, uploads)
	}
	for _, f := range files {
		if f.Path == "KV/cfg.jsonl" && (f.Content == "" || f.Encoding != "") {
			t.Fatalf("KV pack must ride inline: %+v", f)
		}
		if f.Path == "BLOBS/faqs/a.txt" && f.Encoding != "cas" {
			t.Fatalf("blob row must be cas: %+v", f)
		}
	}
}
