package dataset

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
	"github.com/loremlabs/thanks-computer/chassis/filecas/filestore"
)

// bufStore is a buffered filecas.Store with NO BlobPath, forcing the cache
// down the materialise tier (the S3-overlay shape).
type bufStore struct{ blobs map[string][]byte }

func (b *bufStore) Name() string { return "buf" }
func (b *bufStore) Put(ctx context.Context, hash string, data []byte) error {
	if err := filecas.Verify(hash, data); err != nil {
		return err
	}
	b.blobs[hash] = append([]byte(nil), data...)
	return nil
}
func (b *bufStore) Get(ctx context.Context, hash string) ([]byte, error) {
	d, ok := b.blobs[hash]
	if !ok {
		return nil, filecas.ErrNotFound
	}
	return d, nil
}
func (b *bufStore) Exists(ctx context.Context, hash string) (bool, error) {
	_, ok := b.blobs[hash]
	return ok, nil
}

// fixtureBytes builds a valid one-table SQLite artifact and returns its
// bytes + hash. salt makes distinct artifacts (distinct hashes) on demand.
func fixtureBytes(t *testing.T, salt string) ([]byte, string) {
	t.Helper()
	p := buildFixture(t)
	if salt != "" {
		db, err := sql.Open("sqlite3", "file:"+p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO books VALUES (?, ?, 'salt', 'WS')`, "979"+salt, salt); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:])
}

func TestCacheMaterialiseAndReuse(t *testing.T) {
	ctx := context.Background()
	data, hash := fixtureBytes(t, "")
	store := &bufStore{blobs: map[string][]byte{hash: data}}
	dir := t.TempDir()
	c, err := NewCache(dir, store, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	db, err := c.Handle(ctx, hash)
	if err != nil {
		t.Fatalf("cold handle: %v", err)
	}
	var title string
	if err := db.QueryRow(`SELECT title FROM books WHERE isbn13 = ?`, "9780000000001").Scan(&title); err != nil {
		t.Fatalf("query through cache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, hash+ArtifactExt)); err != nil {
		t.Fatalf("materialised file missing: %v", err)
	}
	// Second call: same shared handle, no re-materialise.
	db2, err := c.Handle(ctx, hash)
	if err != nil || db2 != db {
		t.Fatalf("handle not shared: %v", err)
	}
	// Absent hash propagates ErrNotFound.
	if _, err := c.Handle(ctx, strings.Repeat("ab", 32)); !errors.Is(err, filecas.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestCacheZeroCopyViaBlobPath(t *testing.T) {
	ctx := context.Background()
	data, hash := fixtureBytes(t, "")
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(ctx, hash, data); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	c, err := NewCache(dir, fs, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	db, err := c.Handle(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM books`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("zero-copy query: n=%d err=%v", n, err)
	}
	// Nothing materialised — the CAS file was opened in place.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ArtifactExt) {
			t.Fatalf("unexpected materialised copy %s for a local-backend blob", e.Name())
		}
	}
}

func TestCacheEviction(t *testing.T) {
	ctx := context.Background()
	dataA, hashA := fixtureBytes(t, "a")
	dataB, hashB := fixtureBytes(t, "b")
	if hashA == hashB {
		t.Fatal("fixtures not distinct")
	}
	store := &bufStore{blobs: map[string][]byte{hashA: dataA, hashB: dataB}}
	dir := t.TempDir()
	// Budget fits ONE artifact: admitting B must evict A (close + remove),
	// but never the entry being admitted (pin semantics — a handle just
	// handed out is never closed under the caller).
	c, err := NewCache(dir, store, int64(len(dataA))+1)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.Handle(ctx, hashA); err != nil {
		t.Fatal(err)
	}
	dbB, err := c.Handle(ctx, hashB)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := dbB.QueryRow(`SELECT count(*) FROM books`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("B unusable after admit: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, hashA+ArtifactExt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("A not evicted from disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, hashB+ArtifactExt)); err != nil {
		t.Fatalf("B (pinned) unexpectedly evicted: %v", err)
	}
	// A re-request re-materialises A and works again (and in turn evicts B).
	dbA, err := c.Handle(ctx, hashA)
	if err != nil {
		t.Fatalf("re-handle after eviction: %v", err)
	}
	if err := dbA.QueryRow(`SELECT count(*) FROM books`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("query after re-materialise: n=%d err=%v", n, err)
	}
}
