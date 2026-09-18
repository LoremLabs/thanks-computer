package filestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// TestPutStreamNative: the file backend commits a stream whose hash is only
// known at EOF — spool under <root>/.spool, link into the sharded path —
// dedups identical content, enforces the limit, and never leaves a spool
// file behind (success OR failure).
func TestPutStreamNative(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	fs, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	spoolEmpty := func(when string) {
		t.Helper()
		ents, err := os.ReadDir(filepath.Join(root, spoolDir))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("%s: spool not empty: %v", when, ents)
		}
	}

	data := []byte("%PDF-1.7 a print job of unknown hash")
	hash, n, err := fs.PutStream(ctx, bytes.NewReader(data), 1<<20)
	if err != nil || hash != fsHash(data) || n != int64(len(data)) {
		t.Fatalf("PutStream: hash=%s n=%d err=%v", hash, n, err)
	}
	if got, err := fs.Get(ctx, hash); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("stored bytes differ: err=%v", err)
	}
	spoolEmpty("after commit")

	// Through the package helper the native path is discovered (no fallback).
	if h2, _, err := filecas.PutStream(ctx, fs, bytes.NewReader(data), 0); err != nil || h2 != hash {
		t.Fatalf("helper / dedup: %s %v", h2, err)
	}
	spoolEmpty("after dedup")

	over := bytes.Repeat([]byte("x"), 11)
	if _, _, err := fs.PutStream(ctx, bytes.NewReader(over), 10); !errors.Is(err, filecas.ErrTooLarge) {
		t.Fatalf("over-limit: %v", err)
	}
	if ok, _ := fs.Exists(ctx, fsHash(over)); ok {
		t.Fatal("over-limit stream became visible")
	}
	spoolEmpty("after over-limit")

	// Concurrent commits of the same content: all succeed, one blob.
	same := []byte("two clients print the same page")
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, _, err := fs.PutStream(ctx, bytes.NewReader(same), 0)
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent PutStream: %v", err)
		}
	}
	if ok, _ := fs.Exists(ctx, fsHash(same)); !ok {
		t.Fatal("concurrent content missing")
	}
	spoolEmpty("after concurrent")
}
