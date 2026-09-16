// Package filestore is the local-filesystem drive object backend: the
// bundled default, zero infrastructure, directly inspectable.
//
// A put is atomic: content is written to a temp file in the destination
// directory, then renamed into place, so a reader never observes a partial
// object and a failed put leaves nothing at the key. Keys are the ids the
// drive store mints — every segment must already be `[A-Za-z0-9._-]` and
// not only dots; this backend REJECTS anything else rather than
// sanitizing, because a key it did not expect is a bug, not input.
package filestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/drive"
)

// init self-registers the file backend. The chassis activates it with a
// blank import; drive does not import filestore, so there is no cycle and
// an out-of-tree backend registers the same way.
func init() {
	drive.RegisterObjects("file", func(cfg drive.ObjectsConfig) (drive.ObjectStore, error) {
		return New(cfg.FileDir)
	})
}

// FileStore implements drive.ObjectStore over a directory tree.
type FileStore struct {
	root string
}

// New returns a FileStore rooted at dir, creating dir (with parents) if
// absent. A failure here is a startup error, not a per-request one.
func New(dir string) (*FileStore, error) {
	if dir == "" {
		dir = "./chassis/data/drive"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileStore{root: dir}, nil
}

// Name implements drive.ObjectStore.
func (fs *FileStore) Name() string { return "file" }

// keyPath maps a logical "/"-separated key to a filesystem path under
// root, refusing any segment that could leave it.
func (fs *FileStore) keyPath(key string) (string, error) {
	key = strings.Trim(key, "/")
	if key == "" {
		return "", errors.New("drive filestore: empty key")
	}
	parts := strings.Split(key, "/")
	segs := make([]string, 0, len(parts)+1)
	segs = append(segs, fs.root)
	for _, p := range parts {
		if !drive.ValidKeySegment(p) {
			return "", fmt.Errorf("drive filestore: invalid key segment %q", p)
		}
		segs = append(segs, p)
	}
	return filepath.Join(segs...), nil
}

// Put implements drive.ObjectStore: temp file in the destination directory,
// then rename (atomic on POSIX). No fsync, per the chassis posture.
func (fs *FileStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	dest, err := fs.keyPath(key)
	if err != nil {
		return err
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
	_, werr := io.Copy(tmp, r)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmpName, dest)
}

// Get implements drive.ObjectStore.
func (fs *FileStore) Get(ctx context.Context, key string) (io.ReadCloser, drive.ObjectInfo, error) {
	p, err := fs.keyPath(key)
	if err != nil {
		return nil, drive.ObjectInfo{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, drive.ObjectInfo{}, drive.ErrNotFound
		}
		return nil, drive.ObjectInfo{}, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, drive.ObjectInfo{}, err
	}
	if st.IsDir() {
		_ = f.Close()
		return nil, drive.ObjectInfo{}, drive.ErrNotFound
	}
	return f, drive.ObjectInfo{Size: st.Size(), ModTime: st.ModTime()}, nil
}

// Stat implements drive.ObjectStore.
func (fs *FileStore) Stat(ctx context.Context, key string) (drive.ObjectInfo, error) {
	p, err := fs.keyPath(key)
	if err != nil {
		return drive.ObjectInfo{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return drive.ObjectInfo{}, drive.ErrNotFound
		}
		return drive.ObjectInfo{}, err
	}
	if st.IsDir() {
		return drive.ObjectInfo{}, drive.ErrNotFound
	}
	return drive.ObjectInfo{Size: st.Size(), ModTime: st.ModTime()}, nil
}

// List implements drive.ObjectStore: every regular file under prefix (a
// key prefix on segment boundaries; "" for all), skipping in-flight temp
// files.
func (fs *FileStore) List(ctx context.Context, prefix string, fn func(key string, info drive.ObjectInfo) error) error {
	base := fs.root
	if prefix = strings.Trim(prefix, "/"); prefix != "" {
		var err error
		if base, err = fs.keyPath(prefix); err != nil {
			return err
		}
	}
	return filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // empty prefix → empty list
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".tmp-") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			if errors.Is(ierr, os.ErrNotExist) {
				return nil // deleted under us
			}
			return ierr
		}
		rel, rerr := filepath.Rel(fs.root, p)
		if rerr != nil {
			return rerr
		}
		return fn(filepath.ToSlash(rel), drive.ObjectInfo{Size: info.Size(), ModTime: info.ModTime()})
	})
}

// Delete implements drive.ObjectStore (idempotent). Empty parent
// directories are removed up to the root so a deleted resource leaves no
// trace.
func (fs *FileStore) Delete(ctx context.Context, key string) error {
	p, err := fs.keyPath(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for dir := filepath.Dir(p); dir != fs.root && strings.HasPrefix(dir, fs.root); dir = filepath.Dir(dir) {
		if err := os.Remove(dir); err != nil {
			break // not empty, or gone
		}
	}
	return nil
}

// Copy implements drive.ObjectCopier: a hard link when the filesystem
// allows it (the object is immutable, so sharing the inode is safe), else a
// byte copy.
func (fs *FileStore) Copy(ctx context.Context, from, to string) error {
	src, err := fs.keyPath(from)
	if err != nil {
		return err
	}
	dst, err := fs.keyPath(to)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	} else if errors.Is(err, os.ErrNotExist) {
		return drive.ErrNotFound
	}
	rc, info, err := fs.Get(ctx, from)
	if err != nil {
		return err
	}
	defer rc.Close()
	return fs.Put(ctx, to, rc, info.Size, "")
}

// Compile-time interface checks.
var (
	_ drive.ObjectStore  = (*FileStore)(nil)
	_ drive.ObjectCopier = (*FileStore)(nil)
)
