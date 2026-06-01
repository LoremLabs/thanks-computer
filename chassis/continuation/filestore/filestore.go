// Package filestore is the local-filesystem continuation backend: the
// bundled default, zero infrastructure, directly inspectable.
//
// Create-if-absent is atomic: content is written to a temp file in the
// destination directory, then os.Link'd into place. os.Link fails with
// EEXIST if the destination already exists, which IS the create-if-absent
// primitive — and because the linked inode already holds the complete
// bytes, a reader never observes a partial object.
package filestore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
)

// unsafeSeg mirrors trace.sanitizeName's allowlist: a path segment keeps
// [A-Za-z0-9._-]; everything else becomes '_'. Keys are "/"-separated;
// each segment is sanitized independently so stage strings like
// "website/100" nest naturally.
var unsafeSeg = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeSeg(s string) string {
	if s == "" {
		return "_"
	}
	s = unsafeSeg.ReplaceAllString(s, "_")
	// A segment that is only dots ('.', '..', ...) is a filesystem
	// traversal primitive the char allowlist does NOT catch — '.' is a
	// permitted name char (e.g. "in.json"). Left alone, filepath.Join's
	// Clean would resolve '..' and walk out of the store root. Prefix it
	// so it can never be the special '.'/'..' entry.
	if strings.Trim(s, ".") == "" {
		return "_" + s
	}
	return s
}

// init self-registers the file backend. The chassis activates it with a
// blank import (server.Start); continuation does not import filestore, so
// there is no cycle and an out-of-tree backend registers the same way.
func init() {
	continuation.Register("file", func(cfg continuation.StoreConfig) (continuation.Store, error) {
		return New(cfg.FileDir)
	})
}

// FileStore implements continuation.Store over a directory tree.
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

// keyPath maps a logical "/"-separated key to a sanitized filesystem path
// under root.
func (fs *FileStore) keyPath(key string) string {
	parts := strings.Split(strings.Trim(key, "/"), "/")
	segs := make([]string, 0, len(parts)+1)
	segs = append(segs, fs.root)
	for _, p := range parts {
		segs = append(segs, sanitizeSeg(p))
	}
	return filepath.Join(segs...)
}

// Create writes data at key iff key is absent (atomic; ErrExists if present).
func (fs *FileStore) Create(ctx context.Context, key string, r io.Reader, meta continuation.Meta) (continuation.Ref, error) {
	dest := fs.keyPath(key)
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return continuation.Ref{}, err
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return continuation.Ref{}, err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp inode in every path below; the
	// destination (when created) is a separate hard link to the content.
	defer func() { _ = os.Remove(tmpName) }()

	n, werr := io.Copy(tmp, r)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return continuation.Ref{}, werr
	}

	if lerr := os.Link(tmpName, dest); lerr != nil {
		if errors.Is(lerr, os.ErrExist) {
			return continuation.Ref{}, continuation.ErrExists
		}
		return continuation.Ref{}, lerr
	}
	return continuation.Ref{Store: "file", Key: key, Size: n}, nil
}

func (fs *FileStore) Get(ctx context.Context, key string) ([]byte, continuation.Meta, error) {
	b, err := os.ReadFile(fs.keyPath(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, continuation.Meta{}, continuation.ErrNotFound
		}
		return nil, continuation.Meta{}, err
	}
	return b, continuation.Meta{}, nil
}

func (fs *FileStore) Exists(ctx context.Context, key string) (bool, error) {
	_, err := os.Stat(fs.keyPath(key))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// List returns logical keys (relative to root, "/"-separated, sanitized
// form) for every regular file under prefix.
func (fs *FileStore) List(ctx context.Context, prefix string) ([]string, error) {
	base := fs.keyPath(prefix)
	var keys []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // empty prefix → empty list
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return nil // skip in-flight temp inodes
		}
		rel, rerr := filepath.Rel(fs.root, path)
		if rerr != nil {
			return rerr
		}
		keys = append(keys, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

func (fs *FileStore) Delete(ctx context.Context, key string) error {
	err := os.Remove(fs.keyPath(key))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Compile-time interface check.
var _ continuation.Store = (*FileStore)(nil)
