package filestore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/drive"
)

func TestFileStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "objs")
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if fs.Name() != "file" {
		t.Fatal("name")
	}
	key := "tnt_a/dc_1/dr_1/dv_1"
	if err := fs.Put(ctx, key, strings.NewReader("hello"), 5, "text/plain"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, info, err := fs.Get(ctx, key)
	if err != nil || info.Size != 5 || info.ModTime.IsZero() {
		t.Fatalf("Get: %+v %v", info, err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "hello" {
		t.Fatalf("bytes: %q", b)
	}
	if info, err := fs.Stat(ctx, key); err != nil || info.Size != 5 {
		t.Fatalf("Stat: %+v %v", info, err)
	}
	if _, err := fs.Stat(ctx, "tnt_a/dc_1/dr_1/nope"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("Stat missing: %v", err)
	}
	if _, _, err := fs.Get(ctx, "tnt_a/dc_1"); err == nil {
		t.Fatal("Get of a directory succeeded")
	}
	if _, err := fs.Stat(ctx, "tnt_a/dc_1"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("Stat of a directory: %v", err)
	}

	// Copy (hard link or byte copy) is independent of the source.
	if err := fs.Copy(ctx, key, "tnt_a/dc_1/dr_2/dv_2"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := fs.Copy(ctx, "tnt_a/dc_1/dr_9/dv_9", "tnt_a/dc_1/dr_3/dv_3"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("Copy missing: %v", err)
	}

	// List walks everything, skipping in-flight temp files; prefix narrows.
	if err := os.WriteFile(filepath.Join(root, "tnt_a", "dc_1", ".tmp-abc"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := fs.List(ctx, "", func(k string, info drive.ObjectInfo) error {
		keys = append(keys, k)
		if info.Size != 5 {
			t.Errorf("size of %s = %d", k, info.Size)
		}
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if strings.Join(keys, ",") != "tnt_a/dc_1/dr_1/dv_1,tnt_a/dc_1/dr_2/dv_2" {
		t.Fatalf("keys: %v", keys)
	}
	keys = nil
	_ = fs.List(ctx, "tnt_a/dc_1/dr_2", func(k string, _ drive.ObjectInfo) error { keys = append(keys, k); return nil })
	if len(keys) != 1 {
		t.Fatalf("prefixed list: %v", keys)
	}
	keys = nil
	_ = fs.List(ctx, "tnt_zzz", func(k string, _ drive.ObjectInfo) error { keys = append(keys, k); return nil })
	if len(keys) != 0 {
		t.Fatalf("missing prefix list: %v", keys)
	}
	stop := errors.New("stop")
	if err := fs.List(ctx, "", func(string, drive.ObjectInfo) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("List stop: %v", err)
	}

	// Delete is idempotent and prunes empty parents.
	if err := fs.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := fs.Delete(ctx, key); err != nil {
		t.Fatalf("Delete twice: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tnt_a", "dc_1", "dr_1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty parent not pruned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tnt_a", "dc_1", "dr_2")); err != nil {
		t.Fatalf("sibling pruned: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root pruned: %v", err)
	}
}

func TestFileStoreRejectsBadKeys(t *testing.T) {
	ctx := context.Background()
	fs, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "/", "../x", "a/../b", "a/./b", "a b/c", "a/b\x00", "é/x", "a//b"} {
		if err := fs.Put(ctx, key, strings.NewReader("x"), 1, ""); err == nil {
			t.Errorf("Put(%q) accepted", key)
		}
		if _, _, err := fs.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) accepted", key)
		}
		if err := fs.Delete(ctx, key); err == nil {
			t.Errorf("Delete(%q) accepted", key)
		}
	}
}

func TestFileStoreAtomicPut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fs, _ := New(root)
	// A reader that fails midway leaves no object and no temp file.
	r := io.MultiReader(strings.NewReader("abc"), errReader{})
	if err := fs.Put(ctx, "a/b/c", r, 6, ""); err == nil {
		t.Fatal("failed body accepted")
	}
	if _, err := fs.Stat(ctx, "a/b/c"); !errors.Is(err, drive.ErrNotFound) {
		t.Fatalf("partial object visible: %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "a", "b"))
	if len(entries) != 0 {
		t.Fatalf("temp file left: %v", entries)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }
