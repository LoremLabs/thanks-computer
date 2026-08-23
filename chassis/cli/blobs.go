package cli

// BLOBS/ collection for `txco data apply` + `txco dev`: the BLOBS/** tree
// under a stack is the runtime blob store's seed pack — one file per blob,
// the tree IS the hierarchy ("BLOBS/faqs/house-01.doc" seeds the blob name
// "faqs/house-01.doc"). Files can be any size, so like DATASETS/ artifacts
// they are hashed by STREAMING and enter the draft as fingerprint-only rows
// (Encoding "cas") after ensureBlobsResident streams any missing bytes to
// the chassis blob endpoint. Names are validated here so a developer gets
// the error at the keyboard, not in a post-activation reconcile log.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/blob"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/storeseed"
)

// collectBlobFiles walks <stackDir>/BLOBS/** and returns one fingerprint-only
// draft row per regular file plus the upload set for ensureBlobsResident.
// Dotfiles/dotdirs and irregular files (symlinks) are skipped, like
// collectTreeAssets. An absent BLOBS/ yields nil, nil, nil. A file whose
// stack-relative name is not a valid blob name is a hard error.
func collectBlobFiles(stackDir string) ([]client.StackFile, []casUpload, error) {
	treeDir := filepath.Join(stackDir, storeseed.DirBlobs)
	info, err := os.Stat(treeDir)
	if err != nil || !info.IsDir() {
		return nil, nil, nil
	}
	var files []client.StackFile
	var uploads []casUpload
	walkErr := filepath.WalkDir(treeDir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if p != treeDir && strings.HasPrefix(filepath.Base(p), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(filepath.Base(p), ".") {
			return nil
		}
		rel, rerr := filepath.Rel(stackDir, p) // "BLOBS/<...>"
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		name := storeseed.PackName(rel)
		if err := blob.ValidName(name); err != nil {
			return fmt.Errorf("%s: %w (blob names are '/'-separated segments of [A-Za-z0-9._-], no leading '_')", rel, err)
		}
		hash, size, herr := hashFileStreaming(p)
		if herr != nil {
			return fmt.Errorf("hash %s: %w", rel, herr)
		}
		files = append(files, client.StackFile{Path: rel, ContentHash: hash, Encoding: "cas"})
		uploads = append(uploads, casUpload{Path: rel, LocalPath: p, Hash: hash, Size: size})
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	return files, uploads, nil
}
