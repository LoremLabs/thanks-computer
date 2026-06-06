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
	"errors"
	"fmt"
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

// Compile-time interface check.
var _ filecas.Store = (*FileStore)(nil)
