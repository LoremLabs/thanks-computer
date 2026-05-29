// Package filestore is the local-filesystem artifact backend: the built-in
// default, zero infrastructure, directly inspectable.
//
// Each ref maps to two sibling files under root: "<ref>" (data) and
// "<ref>.manifest.json" (manifest). Put writes both via temp-file+rename so
// a reader never observes a half-written pair.
package filestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/artifact"
)

var unsafeSeg = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func sanitizeSeg(s string) string {
	if s == "" {
		return "_"
	}
	s = unsafeSeg.ReplaceAllString(s, "_")
	// A segment that is only dots ('.', '..', ...) is a filesystem
	// traversal primitive the char allowlist does NOT catch — '.' is a
	// permitted name char. Left alone, filepath.Join's Clean would
	// resolve '..' and walk out of the store root. Prefix it so it can
	// never be the special '.'/'..' entry.
	if strings.Trim(s, ".") == "" {
		return "_" + s
	}
	return s
}

// init self-registers the file backend. The chassis activates it with a
// blank import (server.Start); artifact does not import filestore, so there
// is no cycle and an additional backend registers the same way.
func init() {
	artifact.Register("file", func(cfg artifact.StoreConfig) (artifact.Store, error) {
		return New(cfg.FileDir)
	})
}

// FileStore implements artifact.Store over a directory tree.
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

func (fs *FileStore) refPath(ref string) string {
	parts := strings.Split(strings.Trim(ref, "/"), "/")
	segs := make([]string, 0, len(parts)+1)
	segs = append(segs, fs.root)
	for _, p := range parts {
		segs = append(segs, sanitizeSeg(p))
	}
	return filepath.Join(segs...)
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Put writes data + manifest atomically. Manifest is written first, then
// data, then nothing else can fail — so a present data file always has a
// present manifest beside it.
func (fs *FileStore) Put(ctx context.Context, ref string, data, manifest []byte) error {
	base := fs.refPath(ref)
	if err := writeAtomic(base+".manifest.json", manifest); err != nil {
		return err
	}
	return writeAtomic(base, data)
}

func (fs *FileStore) Get(ctx context.Context, ref string) ([]byte, []byte, error) {
	base := fs.refPath(ref)
	data, err := os.ReadFile(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, artifact.ErrNotFound
		}
		return nil, nil, err
	}
	man, err := os.ReadFile(base + ".manifest.json")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, artifact.ErrNotFound
		}
		return nil, nil, err
	}
	return data, man, nil
}

func (fs *FileStore) Exists(ctx context.Context, ref string) (bool, error) {
	_, err := os.Stat(fs.refPath(ref))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Compile-time interface check.
var _ artifact.Store = (*FileStore)(nil)
