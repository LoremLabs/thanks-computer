// Package filestore is the local-filesystem filecas backend: the bundled
// default, content-addressed, maildir-sharded (sha256/<ab>/<cd>/<hash>),
// zero infrastructure, directly inspectable.
//
// Put verifies sha256(data)==hash, then writes create-if-absent: bytes to a
// temp file in the destination directory, then os.Link into place. os.Link
// fails with EEXIST if the destination exists, which IS the dedup primitive
// — and because the linked inode already holds the complete bytes, a reader
// never observes a partial object. Mirrors continuation/filestore.
package filestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/loremlabs/thanks-computer/chassis/filecas"
)

// init self-registers the file backend. The chassis activates it with a
// blank import; filecas does not import filestore, so there is no cycle and
// an out-of-tree backend registers the same way.
func init() {
	filecas.Register("file", func(cfg filecas.StoreConfig) (filecas.Store, error) {
		return New(cfg.FileDir)
	})
}

// FileStore implements filecas.Store over a directory tree.
type FileStore struct {
	root string
}

// New returns a FileStore rooted at dir, creating dir (with parents) if
// absent. A failure here is a startup error, not a per-request one.
func New(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileStore{root: dir}, nil
}

func (fs *FileStore) Name() string { return "file" }

// hashPath maps a content hash to its sharded path under root, or "" if the
// hash is malformed (rejected by ShardKey — traversal defense).
func (fs *FileStore) hashPath(hash string) string {
	key, ok := filecas.ShardKey(hash)
	if !ok {
		return ""
	}
	return filepath.Join(fs.root, filepath.FromSlash(key))
}

func (fs *FileStore) Put(ctx context.Context, hash string, data []byte) error {
	if err := filecas.Verify(hash, data); err != nil {
		return err
	}
	dest := fs.hashPath(hash)
	if dest == "" {
		// Unreachable once Verify passes (a sha256 hex is always valid),
		// but guard anyway rather than join a crafted path.
		return fmt.Errorf("filecas: malformed hash %q", hash)
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		return werr
	}
	if cerr := tmp.Close(); cerr != nil {
		return cerr
	}
	if lerr := os.Link(tmpName, dest); lerr != nil {
		if errors.Is(lerr, os.ErrExist) {
			return nil // dedup: identical content already present
		}
		return lerr
	}
	return nil
}

func (fs *FileStore) Get(ctx context.Context, hash string) ([]byte, error) {
	p := fs.hashPath(hash)
	if p == "" {
		return nil, filecas.ErrNotFound
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, filecas.ErrNotFound
		}
		return nil, err
	}
	return b, nil
}

func (fs *FileStore) Exists(ctx context.Context, hash string) (bool, error) {
	p := fs.hashPath(hash)
	if p == "" {
		return false, nil
	}
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// PutReader implements filecas.ReaderPutter: stream to a temp file in the
// destination directory while hashing, verify, then os.Link into place —
// the same never-observe-a-partial-object discipline as Put, without ever
// holding the blob in memory. size is advisory here; when non-negative it
// is cross-checked like the hash.
func (fs *FileStore) PutReader(ctx context.Context, hash string, r io.Reader, size int64) error {
	dest := fs.hashPath(hash)
	if dest == "" {
		return fmt.Errorf("filecas: malformed hash %q", hash)
	}
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	h := sha256.New()
	n, werr := io.Copy(io.MultiWriter(tmp, h), r)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if size >= 0 && n != size {
		return fmt.Errorf("%w: wrote %d bytes, expected %d", filecas.ErrHashMismatch, n, size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		return fmt.Errorf("%w: have %s want %s", filecas.ErrHashMismatch, got, hash)
	}
	if lerr := os.Link(tmpName, dest); lerr != nil {
		if errors.Is(lerr, os.ErrExist) {
			return nil // dedup: identical content already present
		}
		return lerr
	}
	return nil
}

// GetReader implements filecas.ReaderGetter: an open read handle on the
// content-addressed file itself.
func (fs *FileStore) GetReader(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	p := fs.hashPath(hash)
	if p == "" {
		return nil, 0, filecas.ErrNotFound
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, filecas.ErrNotFound
		}
		return nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// BlobPath implements filecas.PathProvider: the blob IS a local file, so
// zero-copy consumers can open it in place (read-only — the path is
// content-addressed and immutable).
func (fs *FileStore) BlobPath(hash string) (string, bool) {
	p := fs.hashPath(hash)
	if p == "" {
		return "", false
	}
	if _, err := os.Stat(p); err != nil {
		return "", false
	}
	return p, true
}

// Compile-time interface checks.
var (
	_ filecas.Store        = (*FileStore)(nil)
	_ filecas.ReaderPutter = (*FileStore)(nil)
	_ filecas.ReaderGetter = (*FileStore)(nil)
	_ filecas.PathProvider = (*FileStore)(nil)
)
