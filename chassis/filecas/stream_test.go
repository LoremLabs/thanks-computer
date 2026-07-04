package filecas

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

// memStore is a minimal buffered Store with NO streaming capabilities, for
// exercising the package-helper fallbacks.
type memStore struct{ blobs map[string][]byte }

func newMemStore() *memStore { return &memStore{blobs: map[string][]byte{}} }

func (m *memStore) Name() string { return "mem" }
func (m *memStore) Put(ctx context.Context, hash string, data []byte) error {
	if err := Verify(hash, data); err != nil {
		return err
	}
	if _, ok := m.blobs[hash]; !ok {
		m.blobs[hash] = append([]byte(nil), data...)
	}
	return nil
}
func (m *memStore) Get(ctx context.Context, hash string) ([]byte, error) {
	b, ok := m.blobs[hash]
	if !ok {
		return nil, ErrNotFound
	}
	return b, nil
}
func (m *memStore) Exists(ctx context.Context, hash string) (bool, error) {
	_, ok := m.blobs[hash]
	return ok, nil
}

func streamHash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// TestStreamingFallbacks: helpers must work (buffered) against a backend
// with no streaming interfaces — including through the LRU decorator.
func TestStreamingFallbacks(t *testing.T) {
	ctx := context.Background()
	backend := newMemStore()
	wrapped := newCachedStore(backend, 1<<20, 1<<20) // the decorator every boot applies

	data := []byte("fallback bytes")
	h := streamHash(data)
	if err := PutReader(ctx, wrapped, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("PutReader fallback: %v", err)
	}
	rc, size, err := GetReader(ctx, wrapped, h)
	if err != nil {
		t.Fatalf("GetReader fallback: %v", err)
	}
	defer rc.Close()
	if size != int64(len(data)) {
		t.Fatalf("size %d != %d", size, len(data))
	}
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatalf("roundtrip mismatch")
	}
	if _, ok := BlobPath(wrapped, h); ok {
		t.Fatal("BlobPath must report false for a pathless backend")
	}
	// Fallback Put still verifies the hash.
	if err := PutReader(ctx, wrapped, h, strings.NewReader("different"), 9); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v", err)
	}
}

// TestCapabilityDiscoveryThroughDecorator: a streaming-capable backend keeps
// its capabilities when wrapped by the LRU.
func TestCapabilityDiscoveryThroughDecorator(t *testing.T) {
	backend := &streamStore{memStore: newMemStore()}
	wrapped := newCachedStore(backend, 1<<20, 1<<20)
	if _, ok := capability[ReaderPutter](wrapped); !ok {
		t.Fatal("ReaderPutter not discovered through decorator")
	}
	if _, ok := capability[ReaderGetter](wrapped); !ok {
		t.Fatal("ReaderGetter not discovered through decorator")
	}
	// And the helper actually routes to the native path.
	data := []byte("native path")
	h := streamHash(data)
	if err := PutReader(context.Background(), wrapped, h, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if !backend.putReaderUsed {
		t.Fatal("PutReader helper fell back to buffered Put despite native capability")
	}
}

// streamStore decorates memStore with marker streaming implementations.
type streamStore struct {
	*memStore
	putReaderUsed bool
}

func (s *streamStore) PutReader(ctx context.Context, hash string, r io.Reader, size int64) error {
	s.putReaderUsed = true
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	return s.memStore.Put(ctx, hash, data)
}

func (s *streamStore) GetReader(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	b, err := s.memStore.Get(ctx, hash)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}
