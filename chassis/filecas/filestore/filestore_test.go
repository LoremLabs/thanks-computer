package filestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func newStore(t *testing.T) *FileStore {
	t.Helper()
	fs, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fs
}

func TestPutGetExistsSharded(t *testing.T) {
	ctx := context.Background()
	fs := newStore(t)
	data := []byte("hello world")
	h := hashOf(data)

	if err := fs.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Sharded path on disk: <root>/sha256/<ab>/<cd>/<hash>
	want := filepath.Join(fs.root, "sha256", h[0:2], h[2:4], h)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("expected sharded file %s: %v", want, err)
	}

	got, err := fs.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get=%q want %q", got, data)
	}
	ok, err := fs.Exists(ctx, h)
	if err != nil || !ok {
		t.Fatalf("Exists=%v,%v want true,nil", ok, err)
	}
}

func TestPutDedup(t *testing.T) {
	ctx := context.Background()
	fs := newStore(t)
	data := []byte("dedup me")
	h := hashOf(data)
	if err := fs.Put(ctx, h, data); err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(ctx, h, data); err != nil {
		t.Fatalf("second Put (dedup) should be nil, got %v", err)
	}
}

func TestPutHashMismatch(t *testing.T) {
	ctx := context.Background()
	fs := newStore(t)
	data := []byte("real content")
	wrong := hashOf([]byte("different")) // valid format, wrong digest
	if err := fs.Put(ctx, wrong, data); !errors.Is(err, filecas.ErrHashMismatch) {
		t.Fatalf("Put wrong hash err=%v want ErrHashMismatch", err)
	}
	// Nothing should have been written under the (wrong) hash.
	if ok, _ := fs.Exists(ctx, wrong); ok {
		t.Fatal("mismatched Put left a file behind")
	}
}

func TestGetExistsMissing(t *testing.T) {
	ctx := context.Background()
	fs := newStore(t)
	missing := hashOf([]byte("never stored"))
	if _, err := fs.Get(ctx, missing); !errors.Is(err, filecas.ErrNotFound) {
		t.Fatalf("Get missing err=%v want ErrNotFound", err)
	}
	if ok, err := fs.Exists(ctx, missing); ok || err != nil {
		t.Fatalf("Exists missing=%v,%v want false,nil", ok, err)
	}
	// Malformed hash → traversal-safe ErrNotFound / false, no path join.
	if _, err := fs.Get(ctx, "../../etc/passwd"); !errors.Is(err, filecas.ErrNotFound) {
		t.Fatalf("Get malformed err=%v want ErrNotFound", err)
	}
	if ok, _ := fs.Exists(ctx, "../../etc/passwd"); ok {
		t.Fatal("Exists malformed returned true")
	}
}
