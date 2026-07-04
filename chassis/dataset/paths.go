// Package dataset implements txco://dataset: stack-bundled, immutable SQLite
// artifacts queried at runtime through named, apply-time-validated queries.
// A stack ships a pair of files under the reserved DATASETS/ subtree:
//
//	DATASETS/<name>.sqlite   the artifact — content-addressed in filecas,
//	                         fingerprint-only in stack_files (never inline)
//	DATASETS/<name>.yaml     the manifest — the named queries the runtime
//	                         may execute against that artifact
//
// This file (paths.go) is the leaf layer: just the reserved-path vocabulary,
// with no dependency on SQLite or the stores. It is imported by the CLI (to
// collect the pair into the bundle), the admin producer (to CAS-back artifact
// bytes like FILES/), and the control-event applier (to keep artifact bytes
// out of the in-memory runtime DB).
package dataset

import "strings"

// Reserved top-level directory (sibling to FILES/, VECTORS/, KV/) and the two
// member extensions. The pairing is strict: an artifact without a manifest
// (or vice versa) is a deploy error, not a warning — see Validate.
const (
	Dir = "DATASETS"

	ArtifactExt = ".sqlite"
	ManifestExt = ".yaml"
)

// IsDatasetPath reports whether a stack_files path lives under DATASETS/.
// Used by the producer / applier to give artifacts the same CAS-backed,
// out-of-runtime-DB treatment as FILES/ static assets.
func IsDatasetPath(p string) bool { return strings.HasPrefix(p, Dir+"/") }

// IsArtifactPath reports whether p is a dataset artifact ("DATASETS/<name>.sqlite").
func IsArtifactPath(p string) bool { return Name(p) != "" && strings.HasSuffix(p, ArtifactExt) }

// IsManifestPath reports whether p is a dataset manifest ("DATASETS/<name>.yaml").
func IsManifestPath(p string) bool { return Name(p) != "" && strings.HasSuffix(p, ManifestExt) }

// Name returns the dataset name a member path belongs to:
// "DATASETS/books.sqlite" → "books", "DATASETS/books.yaml" → "books".
// It returns "" when the path is not under DATASETS/, is nested (no slashes
// allowed in the name), or has neither member extension — the same shape
// validateStackFilePath enforces at upload.
func Name(p string) string {
	if !IsDatasetPath(p) {
		return ""
	}
	rest := p[len(Dir)+1:]
	if rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	for _, ext := range []string{ArtifactExt, ManifestExt} {
		if strings.HasSuffix(rest, ext) && len(rest) > len(ext) {
			return strings.TrimSuffix(rest, ext)
		}
	}
	return ""
}
