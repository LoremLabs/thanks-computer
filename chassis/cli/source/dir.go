package source

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// dirSource copies a local directory tree. Used by `dir:./path` and `file:./path`.
// Unlike githubSource it has no subpath notion — it copies the whole tree (a
// package root: `txco.package.yaml` + an `OPS/`-shaped subtree).
type dirSource struct {
	root string // absolute path to the local tree
	spec string // original spec, kept for error messages
}

func (d *dirSource) Spec() string { return d.spec }

// parseDir resolves a dir:/file: spec to an absolute local directory.
func parseDir(p, spec string) (*dirSource, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil, fmt.Errorf("source %q missing a path (try dir:./path)", spec)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", spec, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", spec, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("source %q: %s is not a directory", spec, abs)
	}
	return &dirSource{root: abs, spec: spec}, nil
}

// Fetch copies the local tree at d.root into destDir. Same safety posture as
// the tarball extractor: skips symlinks/devices, rejects path escapes via
// safeRelPath, caps per-file and total bytes. Returns the count of regular
// files written.
func (d *dirSource) Fetch(_ context.Context, destDir string) (int, error) {
	fsys := os.DirFS(d.root)
	var written int
	var totalBytes int64
	err := fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		clean, ok := safeRelPath(p)
		if !ok {
			return nil // skip hostile/odd paths
		}
		out := filepath.Join(destDir, filepath.FromSlash(clean))
		if de.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		// Only copy regular files; skip symlinks, devices, fifos.
		if !de.Type().IsRegular() {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		if info.Size() > maxFileBytes {
			return fmt.Errorf("source file %s exceeds %d-byte limit", clean, maxFileBytes)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		in, err := fsys.Open(p)
		if err != nil {
			return err
		}
		f, err := os.Create(out)
		if err != nil {
			_ = in.Close()
			return err
		}
		n, cerr := io.Copy(f, io.LimitReader(in, maxFileBytes))
		_ = in.Close()
		closeErr := f.Close()
		if cerr != nil {
			return fmt.Errorf("copy %s: %w", clean, cerr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", out, closeErr)
		}
		totalBytes += n
		if totalBytes > maxTotalBytes {
			return fmt.Errorf("source tree exceeds %d-byte total limit", maxTotalBytes)
		}
		written++
		return nil
	})
	return written, err
}
