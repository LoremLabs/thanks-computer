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

// errAfter yields its bytes, then fails — a client that drops mid-upload.
type errAfter struct {
	data []byte
	err  error
}

func (e *errAfter) Read(p []byte) (int, error) {
	if len(e.data) == 0 {
		return 0, e.err
	}
	n := copy(p, e.data)
	e.data = e.data[n:]
	return n, nil
}

// TestPutStreamFallback: a backend with no StreamPutter still commits an
// unknown-hash stream (spool → hash → PutReader), through the LRU decorator;
// the limit is inclusive; and every failure leaves NOTHING behind.
func TestPutStreamFallback(t *testing.T) {
	ctx := context.Background()
	backend := newMemStore()
	wrapped := newCachedStore(backend, 1<<20, 1<<20)

	data := []byte("a document whose hash nobody knew up front")
	hash, n, err := PutStream(ctx, wrapped, bytes.NewReader(data), 0)
	if err != nil || hash != streamHash(data) || n != int64(len(data)) {
		t.Fatalf("PutStream: hash=%s n=%d err=%v", hash, n, err)
	}
	if got, err := wrapped.Get(ctx, hash); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("stored bytes differ: %q err=%v", got, err)
	}
	// Re-put of identical content is a dedup success.
	if h2, _, err := PutStream(ctx, wrapped, bytes.NewReader(data), 0); err != nil || h2 != hash {
		t.Fatalf("dedup re-put: %s %v", h2, err)
	}

	// Exactly the limit is accepted; one byte more is refused and stores nothing.
	exact := []byte("0123456789")
	if _, _, err := PutStream(ctx, wrapped, bytes.NewReader(exact), int64(len(exact))); err != nil {
		t.Fatalf("stream of exactly limit bytes refused: %v", err)
	}
	over := []byte("0123456789X")
	if _, _, err := PutStream(ctx, wrapped, bytes.NewReader(over), 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-limit: err=%v, want ErrTooLarge", err)
	}
	if ok, _ := backend.Exists(ctx, streamHash(over)); ok {
		t.Fatal("over-limit stream became visible")
	}

	// A reader that fails mid-stream stores nothing (not even the prefix).
	boom := errors.New("client went away")
	prefix := []byte("partial upload")
	if _, _, err := PutStream(ctx, wrapped, &errAfter{data: append([]byte(nil), prefix...), err: boom}, 0); !errors.Is(err, boom) {
		t.Fatalf("reader error: %v", err)
	}
	if ok, _ := backend.Exists(ctx, streamHash(prefix)); ok {
		t.Fatal("partial stream became visible")
	}

	// A cancelled context aborts before anything lands.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	gone := []byte("never stored")
	if _, _, err := PutStream(cctx, wrapped, bytes.NewReader(gone), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: %v", err)
	}
	if ok, _ := backend.Exists(ctx, streamHash(gone)); ok {
		t.Fatal("cancelled stream became visible")
	}
}
