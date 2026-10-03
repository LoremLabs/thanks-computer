package local

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/loremlabs/thanks-computer/chassis/stackdir"
	"github.com/loremlabs/thanks-computer/chassis/workspace"
)

// treesDir holds the stack trees under the root: <root>/.stacks/<digest>/.
// dirFor refuses a tenant or stack segment starting with ".", so no workspace
// can collide with it.
const treesDir = ".stacks"

// PlaceTree makes the stack tree t present under <root>/.stacks/<digest>/ and
// returns that path. One copy per digest serves every workspace and every
// stack whose tree hashes the same: the first exec extracts it into a temp
// directory and renames it into place, so a concurrent exec either finds it
// whole or extracts its own copy and discards it. Files are 0444 (0555 when
// the source was executable) and directories 0555 — a guard against a command
// writing into its own program, not an access boundary: this provider has
// none.
func (c *computer) PlaceTree(ctx context.Context, t workspace.Tree) (string, error) {
	if !stackdir.ValidDigest(t.Digest) {
		return "", &workspace.Error{Code: workspace.CodeStackDirUnavailable, Message: "malformed tree digest"}
	}
	final := filepath.Join(c.trees, t.Digest)
	if st, err := os.Stat(final); err == nil && st.IsDir() {
		return final, nil
	}
	if t.Open == nil {
		return "", &workspace.Error{Code: workspace.CodeStackDirUnavailable, Message: "no way to read the stack tree's bundle"}
	}
	data, err := t.Open(ctx)
	if err != nil {
		return "", &workspace.Error{Code: workspace.CodeStackDirUnavailable, Message: "read the stack tree: " + err.Error()}
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != t.Digest {
		return "", &workspace.Error{Code: workspace.CodeStackDirUnavailable, Message: "the stack tree's bundle does not match its digest"}
	}
	if err := os.MkdirAll(c.trees, 0o700); err != nil {
		return "", &workspace.Error{Code: "provider", Message: "mkdir trees: " + err.Error()}
	}
	tmp, err := os.MkdirTemp(c.trees, ".place-"+t.Digest[:12]+"-")
	if err != nil {
		return "", &workspace.Error{Code: "provider", Message: "mkdir tree: " + err.Error()}
	}
	if err := extractTree(tmp, data); err != nil {
		removeTree(tmp)
		return "", &workspace.Error{Code: workspace.CodeStackDirUnavailable, Message: "extract the stack tree: " + err.Error()}
	}
	if err := os.Rename(tmp, final); err != nil {
		removeTree(tmp)
		// Another exec placed it first: theirs is the same bytes.
		if st, serr := os.Stat(final); serr == nil && st.IsDir() {
			return final, nil
		}
		return "", &workspace.Error{Code: "provider", Message: "place tree: " + err.Error()}
	}
	// The top directory is locked last: macOS refuses to rename a directory
	// its owner cannot write.
	if err := os.Chmod(final, 0o555); err != nil {
		return "", &workspace.Error{Code: "provider", Message: "lock tree: " + err.Error()}
	}
	return final, nil
}

// extractTree writes a bundle's files beneath dir, then makes the tree below
// dir read-only, deepest directories first so each chmod still can. dir itself
// stays writable until PlaceTree has renamed it into place.
func extractTree(dir string, data []byte) error {
	var dirs []string
	err := stackdir.Unpack(data, func(f stackdir.File) error {
		target := filepath.Join(dir, filepath.FromSlash(f.Path))
		if !within(dir, target) || target == dir {
			return errors.New("bundle path escapes the tree: " + f.Path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if f.Exec {
			mode = 0o555
		}
		if err := os.WriteFile(target, f.Content, 0o600); err != nil {
			return err
		}
		return os.Chmod(target, mode)
	})
	if err != nil {
		return err
	}
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 1; i-- { // dirs[0] is dir itself
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			return err
		}
	}
	return nil
}

// removeTree deletes a tree that extractTree may have made read-only.
func removeTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	_ = os.RemoveAll(dir)
}
