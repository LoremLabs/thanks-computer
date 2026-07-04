package filestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

func fsHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestPutReaderStreamsAndVerifies: happy path, dedup re-put, hash mismatch
// leaves nothing behind, size mismatch refuses.
func TestPutReaderStreamsAndVerifies(t *testing.T) {
	ctx := context.Background()
	fs, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("streamed dataset artifact bytes")
	h := fsHash(data)

	if err := fs.PutReader(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := fs.Get(ctx, h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get after stream: %v", err)
	}
	// Idempotent re-put (dedup via link EEXIST).
	if err := fs.PutReader(ctx, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("re-put: %v", err)
	}

	// Hash mismatch: nothing stored.
	bad := fsHash([]byte("other"))
	if err := fs.PutReader(ctx, bad, strings.NewReader("not those bytes"), 15); !errors.Is(err, filecas.ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v", err)
	}
	if ok, _ := fs.Exists(ctx, bad); ok {
		t.Fatal("mismatched put became visible")
	}

	// Declared-size mismatch refuses even when the hash would match.
	data2 := []byte("sized")
	if err := fs.PutReader(ctx, fsHash(data2), bytes.NewReader(data2), int64(len(data2))+5); !errors.Is(err, filecas.ErrHashMismatch) {
		t.Fatalf("want size mismatch error, got %v", err)
	}
}

func TestGetReaderAndBlobPath(t *testing.T) {
	ctx := context.Background()
	fs, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("zero copy me")
	h := fsHash(data)
	if err := fs.Put(ctx, h, data); err != nil {
		t.Fatal(err)
	}

	rc, size, err := fs.GetReader(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if size != int64(len(data)) {
		t.Fatalf("size %d", size)
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatal("GetReader bytes differ")
	}

	p, ok := fs.BlobPath(h)
	if !ok || p == "" {
		t.Fatal("BlobPath missing for resident blob")
	}
	if _, ok := fs.BlobPath(fsHash([]byte("absent"))); ok {
		t.Fatal("BlobPath reported a missing blob")
	}
	if _, _, err := fs.GetReader(ctx, fsHash([]byte("absent"))); !errors.Is(err, filecas.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
