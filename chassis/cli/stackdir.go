package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/computesrc"
	"github.com/loremlabs/thanks-computer/chassis/dataset"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
	"github.com/loremlabs/thanks-computer/chassis/stackdir"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// The stack tree (chassis/stackdir): a stack whose ops name $TXCO_STACK_DIR
// gets its own directory, packed, as one fingerprint-only STACKDIR/ row; the
// bundle goes to the chassis file store first. A stack that never names it
// carries no row, so its manifest — and whether a re-apply mints a version —
// is exactly what it was before trees existed.

// kindDirs are the top-level directories the chassis already takes by kind;
// they are never part of the tree.
var kindDirs = map[string]bool{
	"FILES":                true,
	storeseed.DirVectors:   true,
	storeseed.DirKV:        true,
	storeseed.DirCalendars: true,
	storeseed.DirContacts:  true,
	storeseed.DirBlobs:     true,
	storeseed.DirSources:   true,
	dataset.Dir:            true,
	outlet.Dir:             true,
	sandbox.Dir:            true,
	capdecl.Dir:            true,
	computesrc.Dir:         true,
	stackdir.Dir:           true,
}

// usesStackDir reports whether any of a stack's ops names the tree.
func usesStackDir(ops []bundle.Op) bool {
	for _, op := range ops {
		if strings.Contains(op.Txcl, stackdir.Token) {
			return true
		}
	}
	return false
}

// stackNames is the set of every stack the walk found, before any filter: a
// nested stack's directory belongs to that stack, not to the one around it.
func stackNames(ops []bundle.Op) map[string]bool {
	out := map[string]bool{}
	for _, op := range ops {
		out[op.Stack] = true
	}
	return out
}

// stackTreeRows packs <dir>/OPS/<stack>/ when one of ops names
// $TXCO_STACK_DIR: every regular, non-dot file, keeping its layout, except
// the kind directories at its top and any directory that is another stack's
// (allStacks). It writes the bundle to <dir>/.txco/stackdir/<hash>.tar and
// returns the one STACKDIR/ row plus its upload. A stack that does not name
// the tree returns nothing.
func stackTreeRows(dir, stack string, ops []bundle.Op, allStacks map[string]bool) ([]client.StackFile, []casUpload, error) {
	if !usesStackDir(ops) {
		return nil, nil, nil
	}
	files, err := collectStackTree(filepath.Join(dir, "OPS", filepath.FromSlash(stack)), stack, allStacks)
	if err != nil {
		return nil, nil, err
	}
	data, hash, err := stackdir.Pack(files)
	if err != nil {
		return nil, nil, err
	}
	cache := filepath.Join(dir, ".txco", "stackdir")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, nil, err
	}
	local := filepath.Join(cache, hash+stackdir.Ext)
	if _, err := os.Stat(local); err != nil {
		tmp, err := os.CreateTemp(cache, ".pack-*")
		if err != nil {
			return nil, nil, err
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if werr == nil {
			werr = cerr
		}
		if werr == nil {
			werr = os.Rename(tmp.Name(), local)
		}
		if werr != nil {
			_ = os.Remove(tmp.Name())
			return nil, nil, werr
		}
	}
	p := stackdir.Path(hash)
	row := client.StackFile{Path: p, ContentHash: hash, Encoding: "cas"}
	up := casUpload{Path: p, LocalPath: local, Hash: hash, Size: int64(len(data))}
	return []client.StackFile{row}, []casUpload{up}, nil
}

// collectStackTree reads the tree's files from stackDir, refusing any file or
// tree over the caps with the largest files named.
func collectStackTree(stackDir, stack string, allStacks map[string]bool) ([]stackdir.File, error) {
	var files []stackdir.File
	var total int64
	err := filepath.WalkDir(stackDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == stackDir {
			return nil
		}
		rel, err := filepath.Rel(stackDir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if !strings.Contains(rel, "/") && kindDirs[rel] {
				return filepath.SkipDir
			}
			if allStacks[stack+"/"+rel] {
				return filepath.SkipDir // a nested stack has its own tree
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // a symlink or device: not part of the tree, as in FILES/
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > stackdir.MaxFileBytes {
			return fmt.Errorf("%s is %d bytes, over the %d-byte cap for one file in a stack tree", rel, info.Size(), stackdir.MaxFileBytes)
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		total += int64(len(content))
		files = append(files, stackdir.File{Path: rel, Exec: info.Mode()&0o111 != 0, Content: content})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if total > stackdir.MaxTreeBytes {
		return nil, fmt.Errorf("the stack tree is %d bytes, over the %d-byte cap; largest: %s", total, stackdir.MaxTreeBytes, largestFiles(files, 5))
	}
	if len(files) > stackdir.MaxFiles {
		return nil, fmt.Errorf("the stack tree holds %d files, over the cap of %d", len(files), stackdir.MaxFiles)
	}
	return files, nil
}

// largestFiles names the n largest files, biggest first, with their sizes.
func largestFiles(files []stackdir.File, n int) string {
	fs := append([]stackdir.File(nil), files...)
	sort.Slice(fs, func(i, j int) bool { return len(fs[i].Content) > len(fs[j].Content) })
	if len(fs) > n {
		fs = fs[:n]
	}
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = fmt.Sprintf("%s (%d bytes)", f.Path, len(f.Content))
	}
	return strings.Join(parts, ", ")
}
