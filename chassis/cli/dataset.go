package cli

// Dataset collection for `txco apply`: the DATASETS/ subtree pairs a SQLite
// artifact ("DATASETS/<name>.sqlite") with a named-query manifest
// ("DATASETS/<name>.yaml"). Manifests are small and ride the normal JSON
// draft body; artifacts can run to gigabytes, so they are hashed here by
// STREAMING (never read into memory) and enter the draft as fingerprint-only
// rows (Encoding "cas") after ensureDatasetBlobs streams any missing bytes to
// the chassis blob endpoint.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/dataset"
)

// datasetUpload is a local dataset artifact that must be present in the
// chassis CAS before the draft referencing it can activate.
type datasetUpload struct {
	Path      string // stack-relative, e.g. "DATASETS/books.sqlite"
	LocalPath string // absolute path on disk
	Hash      string // sha256 hex over the raw bytes (streamed)
	Size      int64
}

// collectDatasetFiles walks <stackDir>/DATASETS/ and returns the draft rows
// (.yaml manifests inline; .sqlite artifacts fingerprint-only) plus the
// upload set for ensureDatasetBlobs. An absent DATASETS/ yields nil, nil, nil.
// Pairing (<name>.sqlite ↔ <name>.yaml) and query preparation are enforced by
// the chassis at activation; here we only fail fast on files that the server
// would reject at the write boundary (nesting, foreign extensions).
func collectDatasetFiles(stackDir string) ([]client.StackFile, []datasetUpload, error) {
	dir := filepath.Join(stackDir, dataset.Dir)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil, nil // no datasets → nothing to collect
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var files []client.StackFile
	var uploads []datasetUpload
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			return nil, nil, fmt.Errorf("%s/%s: nested directories are not allowed under %s/ (members are single <name>%s + <name>%s files)",
				dataset.Dir, name, dataset.Dir, dataset.ArtifactExt, dataset.ManifestExt)
		}
		rel := dataset.Dir + "/" + name
		switch {
		case dataset.IsArtifactPath(rel):
			p := filepath.Join(dir, name)
			hash, size, herr := hashFileStreaming(p)
			if herr != nil {
				return nil, nil, fmt.Errorf("hash %s: %w", rel, herr)
			}
			files = append(files, client.StackFile{
				Path:        rel,
				ContentHash: hash,
				Encoding:    "cas",
			})
			uploads = append(uploads, datasetUpload{Path: rel, LocalPath: p, Hash: hash, Size: size})
		case dataset.IsManifestPath(rel):
			content, rerr := os.ReadFile(filepath.Join(dir, name))
			if rerr != nil {
				return nil, nil, rerr
			}
			ch := sha256.Sum256(content)
			files = append(files, client.StackFile{
				Path:        rel,
				Content:     string(content),
				ContentHash: hex.EncodeToString(ch[:]),
			})
		default:
			return nil, nil, fmt.Errorf("%s: only <name>%s and <name>%s belong under %s/",
				rel, dataset.ArtifactExt, dataset.ManifestExt, dataset.Dir)
		}
	}
	return files, uploads, nil
}

// downloadBlobToFile streams a CAS blob to dest via temp+rename, verifying
// the sha256 on the way down — a corrupt or truncated transfer never
// replaces the destination. The pull path uses it for dataset artifacts,
// whose bytes ride the blob plane rather than the JSON version detail.
func downloadBlobToFile(ctx context.Context, c *client.Client, hash, dest string) error {
	rc, _, err := c.GetBlob(ctx, hash)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".txco-blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), rc); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != hash {
		return fmt.Errorf("blob %s: downloaded bytes hash to %s", hash, got)
	}
	return os.Rename(tmpName, dest)
}

// hashFileStreaming returns the sha256 hex + size of a file without loading
// it into memory — dataset artifacts are routinely far larger than RAM allows.
func hashFileStreaming(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// ensureDatasetBlobs makes every collected artifact resident in the chassis
// CAS before the draft that references it is uploaded: HEAD per hash, then a
// streamed PUT for the misses. Mirrors uploadComputes — activation verifies
// presence and refuses the version when a blob is missing, so failing here
// aborts the stack's deploy rather than leaving a doomed draft.
func ensureDatasetBlobs(ctx context.Context, c *client.Client, uploads []datasetUpload, progress, stderr io.Writer) error {
	for _, u := range uploads {
		ok, err := c.HasBlob(ctx, u.Hash)
		if err != nil {
			return fmt.Errorf("%s: blob probe: %w", u.Path, err)
		}
		if ok {
			continue
		}
		// Only new/changed artifacts reach here, so the (potentially slow)
		// full integrity scan runs once per artifact version, client-side
		// where the file was just built — corruption is caught before the
		// bytes ship, not at activation.
		err = spin(progress, fmt.Sprintf("checking %s", u.Path), func() error {
			return dataset.IntegrityCheck(u.LocalPath)
		})
		if err != nil {
			return fmt.Errorf("%s: %w", u.Path, err)
		}
		err = spin(progress, fmt.Sprintf("uploading %s (%s bytes)", u.Path, humanBytes(u.Size)), func() error {
			f, oerr := os.Open(u.LocalPath)
			if oerr != nil {
				return oerr
			}
			defer f.Close()
			return c.PutBlob(ctx, u.Hash, f, u.Size)
		})
		if err != nil {
			return fmt.Errorf("%s: blob upload: %w", u.Path, err)
		}
	}
	return nil
}

