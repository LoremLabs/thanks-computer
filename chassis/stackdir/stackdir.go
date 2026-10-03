// Package stackdir is a stack version's own directory, handed to a workspace
// exec as a read-only tree: `WITH cwd = "$TXCO_STACK_DIR"` starts the command
// in it and sets TXCO_STACK_DIR to its path, so an op can run a program kept
// beside it by path instead of pasting it into stdin.
//
// When one of a stack's ops names $TXCO_STACK_DIR, `txco apply` packs the
// stack's directory into one canonical tar (the bundle) and uploads it to the
// hash-addressed file store (filecas). The stack version then carries one
// fingerprint-only row:
//
//	STACKDIR/<sha256 of the tar>.tar   content "", content_hash = that sha256
//
// so an unchanged tree costs each new version one small row and its bytes are
// stored once. A run reads the row from the opstack snapshot it pinned, so an
// activation never changes the tree a running or resumed run sees.
//
// This file is the leaf layer: the reserved-path vocabulary, the caps and the
// bundle codec, with no dependency on stores, so the CLI, the admin API, the
// processor and the workspace providers can import it cheaply.
package stackdir

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// Dir is the reserved top-level directory for the tree's row.
	Dir = "STACKDIR"
	// Ext is the row's extension: the bundle is a tar.
	Ext = ".tar"
	// Env is the variable a command finds the tree by.
	Env = "TXCO_STACK_DIR"
	// Token is how an op names the tree in `cwd`; `txco apply` packs a
	// stack's tree only when one of its ops contains it.
	Token = "$" + Env

	// MaxFileBytes caps one file, as `&include` caps an included one.
	MaxFileBytes = 1 << 20
	// MaxTreeBytes caps the files of one tree, together.
	MaxTreeBytes = 32 << 20
	// MaxFiles caps how many files one tree holds.
	MaxFiles = 10000
)

var pathRe = regexp.MustCompile(`^` + Dir + `/([0-9a-f]{64})\` + Ext + `$`)

// Path is the stack_files path of the tree whose bundle hashes to digest
// (sha256, lowercase hex).
func Path(digest string) string { return Dir + "/" + digest + Ext }

// IsPath reports whether a stack_files path lives under STACKDIR/.
func IsPath(p string) bool { return strings.HasPrefix(p, Dir+"/") }

// DigestFromPath returns the digest a well-formed STACKDIR/ path names, or "".
func DigestFromPath(p string) string {
	if m := pathRe.FindStringSubmatch(p); m != nil {
		return m[1]
	}
	return ""
}

// ValidDigest reports whether d is a sha256 in lowercase hex.
func ValidDigest(d string) bool { return DigestFromPath(Path(d)) == d }

// File is one file of a tree.
type File struct {
	// Path is slash-separated and relative to the stack's directory.
	Path string
	// Exec says the source was executable: it is extracted 0555, else 0444,
	// so a program in the tree can be run directly.
	Exec    bool
	Content []byte
}

// epoch is every header's modification time, so the same files always make
// the same bundle.
var epoch = time.Unix(0, 0).UTC()

// Pack writes the canonical bundle of files: sorted by path, regular files
// only, every header fixed (mode 0444 or 0555, owner 0, no names, mtime 0), no
// directory entries. It returns the bytes and their sha256. It refuses what
// Unpack would refuse, so a bundle that packs also unpacks.
func Pack(files []File) ([]byte, string, error) {
	fs := append([]File(nil), files...)
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })
	if err := checkTree(fs); err != nil {
		return nil, "", err
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range fs {
		mode := int64(0o444)
		if f.Exec {
			mode = 0o555
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.Path,
			Size:     int64(len(f.Content)),
			Mode:     mode,
			ModTime:  epoch,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", fmt.Errorf("pack %s: %w", f.Path, err)
		}
		if _, err := tw.Write(f.Content); err != nil {
			return nil, "", fmt.Errorf("pack %s: %w", f.Path, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

// Unpack reads a bundle and hands each file to fn, in the bundle's order. It
// checks every header against the caps and the path rules before fn sees it,
// and refuses anything but a regular file.
func Unpack(data []byte, fn func(File) error) error {
	tr := tar.NewReader(bytes.NewReader(data))
	var total int64
	n := 0
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read bundle: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("bundle entry %q is not a regular file", hdr.Name)
		}
		if err := ValidPath(hdr.Name); err != nil {
			return err
		}
		if seen[hdr.Name] {
			return fmt.Errorf("bundle holds %q twice", hdr.Name)
		}
		seen[hdr.Name] = true
		n++
		if n > MaxFiles {
			return fmt.Errorf("bundle holds more than %d files", MaxFiles)
		}
		if hdr.Size < 0 || hdr.Size > MaxFileBytes {
			return fmt.Errorf("%s is %d bytes, over the %d-byte cap for one file", hdr.Name, hdr.Size, MaxFileBytes)
		}
		total += hdr.Size
		if total > MaxTreeBytes {
			return fmt.Errorf("bundle is over the %d-byte cap for a tree", MaxTreeBytes)
		}
		content, err := io.ReadAll(io.LimitReader(tr, hdr.Size))
		if err != nil {
			return fmt.Errorf("read %s: %w", hdr.Name, err)
		}
		if err := fn(File{Path: hdr.Name, Exec: hdr.Mode&0o111 != 0, Content: content}); err != nil {
			return err
		}
	}
}

// ValidPath reports why p cannot name a file in a tree: it must be a clean,
// relative, slash-separated path with no empty, ".", ".." or dot-prefixed
// segment.
func ValidPath(p string) error {
	if p == "" || len(p) > 1024 {
		return fmt.Errorf("bad tree path %q", p)
	}
	if strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return fmt.Errorf("bad tree path %q", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") {
			return fmt.Errorf("bad tree path %q", p)
		}
	}
	return nil
}

// checkTree checks sorted files against the path rules and the caps, and that
// no file sits where another needs a directory ("a" beside "a/b").
func checkTree(fs []File) error {
	if len(fs) > MaxFiles {
		return fmt.Errorf("the tree holds %d files, over the cap of %d", len(fs), MaxFiles)
	}
	var total int64
	for i, f := range fs {
		if err := ValidPath(f.Path); err != nil {
			return err
		}
		if len(f.Content) > MaxFileBytes {
			return fmt.Errorf("%s is %d bytes, over the %d-byte cap for one file", f.Path, len(f.Content), MaxFileBytes)
		}
		total += int64(len(f.Content))
		if i > 0 {
			prev := fs[i-1].Path
			if prev == f.Path {
				return fmt.Errorf("the tree holds %q twice", f.Path)
			}
		}
	}
	// "a" sorts before "a/b" but "a.x" sorts between them, so check every
	// file against the set of directory prefixes rather than its neighbour.
	files := make(map[string]bool, len(fs))
	for _, f := range fs {
		files[f.Path] = true
	}
	for _, f := range fs {
		for d := path.Dir(f.Path); d != "."; d = path.Dir(d) {
			if files[d] {
				return fmt.Errorf("%s is a file, but %s needs it to be a directory", d, f.Path)
			}
		}
	}
	if total > MaxTreeBytes {
		return fmt.Errorf("the tree is %d bytes, over the %d-byte cap", total, MaxTreeBytes)
	}
	return nil
}
