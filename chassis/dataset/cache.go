package dataset

// The node-local artifact cache: content hash → open read-only *sql.DB.
//
// Two tiers, chosen per artifact:
//   - zero-copy: when the filecas backend has the blob as a local file
//     (the bundled file backend), open it in place — a multi-GB artifact
//     costs no second copy and no budget.
//   - materialise: otherwise (S3 overlay) stream the blob once into
//     <dir>/<hash>.sqlite via temp+rename (a reader never sees a partial
//     file) and open that. Materialised files charge the byte budget and
//     are LRU-evicted: close the handle, remove the file, re-materialise
//     on next use. Content-addressing makes invalidation a non-problem —
//     a new stack version references a new hash and the old one simply
//     ages out.
//
// Handles are shared across requests (immutable files, concurrent readers,
// no locks). Eviction while a query is in flight is safe: database/sql
// lets active connections finish, and unlinking an open SQLite file leaves
// the fd readable (unix semantics; the bytes go when the handle does).

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// handleMaxConns bounds each artifact's connection pool. Readers on an
// immutable DB never block each other; this just caps fd/memory per
// dataset.
const handleMaxConns = 8

// Cache materialises + opens dataset artifacts by content hash.
type Cache struct {
	dir      string
	fcas     filecas.Store
	maxBytes int64 // budget for MATERIALISED bytes; 0 = unbounded

	mu       sync.Mutex
	items    map[string]*list.Element // hash → element holding *entry
	ll       *list.List               // front = most-recently-used
	curBytes int64
}

type entry struct {
	hash  string
	db    *sql.DB
	path  string
	size  int64
	owned bool // true = we materialised path and may evict/remove it
}

// NewCache returns a Cache rooted at dir (created if absent), resolving
// misses from fcas. maxBytes bounds the materialised tier; zero-copy
// opens are free.
func NewCache(dir string, fcas filecas.Store, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Cache{
		dir:      dir,
		fcas:     fcas,
		maxBytes: maxBytes,
		items:    make(map[string]*list.Element),
		ll:       list.New(),
	}, nil
}

// Handle returns the shared read-only DB for the artifact with the given
// content hash, materialising it from the CAS on first use.
// filecas.ErrNotFound propagates when the blob isn't in the store.
func (c *Cache) Handle(ctx context.Context, hash string) (*sql.DB, error) {
	c.mu.Lock()
	if el, ok := c.items[hash]; ok {
		c.ll.MoveToFront(el)
		db := el.Value.(*entry).db
		c.mu.Unlock()
		return db, nil
	}
	c.mu.Unlock()

	// Resolve outside the lock — materialising can take minutes for a cold
	// multi-GB artifact and must not block hits on other datasets. A
	// concurrent double-materialise of the SAME hash is harmless (temp+
	// rename is idempotent) and the loser's handle is dropped on admit.
	ent, err := c.open(ctx, hash)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[hash]; ok {
		// Lost a materialise race; keep the incumbent.
		_ = ent.db.Close()
		c.ll.MoveToFront(el)
		return el.Value.(*entry).db, nil
	}
	el := c.ll.PushFront(ent)
	c.items[hash] = el
	if ent.owned {
		c.curBytes += ent.size
		c.evictLocked(el)
	}
	return ent.db, nil
}

// LocalPath resolves the artifact to a readable local file — the CAS blob
// itself when the backend is local (zero-copy), else the materialised cache
// copy, fetched on first use. Exposed for apply-time validation, which
// opens its own short-lived handle rather than warming the shared one.
func (c *Cache) LocalPath(ctx context.Context, hash string) (string, error) {
	p, _, err := c.resolve(ctx, hash)
	return p, err
}

// resolve maps hash → local path; owned=true means the file is the cache's
// materialised copy (budgeted, evictable) rather than the CAS's own file.
func (c *Cache) resolve(ctx context.Context, hash string) (path string, owned bool, err error) {
	if p, ok := filecas.BlobPath(c.fcas, hash); ok {
		return p, false, nil
	}
	p := filepath.Join(c.dir, hash+ArtifactExt)
	_, err = os.Stat(p)
	switch {
	case err == nil:
		// Already materialised (an earlier boot or a validation pass); trust
		// it — the name IS the content address and only complete files are
		// ever renamed in.
	case errors.Is(err, os.ErrNotExist):
		if _, err = c.materialise(ctx, hash, p); err != nil {
			return "", false, err
		}
	default:
		return "", false, err
	}
	return p, true, nil
}

// open resolves the artifact and opens it through the restricted driver.
func (c *Cache) open(ctx context.Context, hash string) (*entry, error) {
	p, owned, err := c.resolve(ctx, hash)
	if err != nil {
		return nil, err
	}
	var size int64
	if owned {
		info, serr := os.Stat(p)
		if serr != nil {
			return nil, serr
		}
		size = info.Size()
	}
	db, err := openArtifact(p)
	if err != nil {
		return nil, err
	}
	return &entry{hash: hash, db: db, path: p, size: size, owned: owned}, nil
}

// materialise streams the blob to a temp file in dir and renames it to
// dest. Returns the file info of the final file.
func (c *Cache) materialise(ctx context.Context, hash, dest string) (os.FileInfo, error) {
	rc, _, err := filecas.GetReader(ctx, c.fcas, hash)
	if err != nil {
		return nil, fmt.Errorf("dataset fetch %s: %w", hash, err)
	}
	defer rc.Close()
	tmp, err := os.CreateTemp(c.dir, ".tmp-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, rc); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("dataset materialise %s: %w", hash, err)
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return nil, err
	}
	return os.Stat(dest)
}

// evictLocked drops least-recently-used MATERIALISED entries until the
// budget holds, walking tail→front once. `keep` (the entry being admitted)
// is never evicted — the caller is about to hand its handle out, so the
// budget is best-effort with a floor of the in-use artifact. Zero-copy
// entries cost no budget and are skipped; their backing file belongs to
// the CAS and is never removed.
func (c *Cache) evictLocked(keep *list.Element) {
	if c.maxBytes <= 0 {
		return
	}
	for el := c.ll.Back(); el != nil && c.curBytes > c.maxBytes; {
		prev := el.Prev()
		ent := el.Value.(*entry)
		if el != keep && ent.owned {
			c.ll.Remove(el)
			delete(c.items, ent.hash)
			c.curBytes -= ent.size
			_ = ent.db.Close()
			_ = os.Remove(ent.path)
		}
		el = prev
	}
}

// Close closes every open handle (shutdown path). Materialised files stay
// on disk for the next boot.
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, el := range c.items {
		_ = el.Value.(*entry).db.Close()
	}
	c.items = make(map[string]*list.Element)
	c.ll = list.New()
	c.curBytes = 0
}

// openArtifact opens a local artifact file through the restricted driver.
func openArtifact(path string) (*sql.DB, error) {
	db, err := sql.Open(DriverName, DSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(handleMaxConns)
	db.SetMaxIdleConns(handleMaxConns)
	return db, nil
}
