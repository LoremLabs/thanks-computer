// Package lockfile reads and writes txco.packages.lock.yaml — the repo-root,
// committed record of which packages produced the workspace's materialized
// files (workspace PROVENANCE). It is deliberately separate from the chassis's
// server-side manifest_hash / version lineage (runtime truth); see
// docs/txco-oci-packages.md §7.
//
// The lockfile is tool-owned (no user comments), so a yaml.Marshal round-trip
// is safe here — unlike txco.yaml, which install never rewrites.
package lockfile

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.yaml.in/yaml/v3"
)

// FileName is the lockfile name; it lives at the workspace root and is committed.
const FileName = "txco.packages.lock.yaml"

// nowFn returns the install timestamp; overridable in tests for determinism.
var nowFn = func() string { return time.Now().UTC().Format(time.RFC3339) }

// Now returns the current install timestamp (RFC3339 UTC).
func Now() string { return nowFn() }

type File struct {
	Packages []Entry `yaml:"packages"`
}

// Entry records one installed package. registry/namespace/resolved are
// PROVENANCE from the resolved ref (blank for dir:/file: sources in Phase 1,
// filled when OCI lands); name/version are the package's own identity.
type Entry struct {
	Ref           string `yaml:"ref"`                     // exactly what the user typed
	Registry      string `yaml:"registry,omitempty"`      // provenance
	Namespace     string `yaml:"namespace,omitempty"`     // provenance
	Name          string `yaml:"name"`                    // manifest identity
	Version       string `yaml:"version"`                 // manifest identity
	Resolved      string `yaml:"resolved,omitempty"`      // oci://…@sha256 (Phase 2)
	ExportedStack string `yaml:"exportedStack,omitempty"` // stack the package ships
	InstalledAs   string `yaml:"installedAs,omitempty"`   // where it was materialized
	Mode          string `yaml:"mode"`                    // as-stack | vendor-only
	ManifestHash  string `yaml:"manifestHash,omitempty"`  // localManifestHash of materialized files
	InstalledAt   string `yaml:"installedAt"`
}

// Path returns the lockfile path for a workspace root.
func Path(root string) string { return filepath.Join(root, FileName) }

// Read loads the lockfile at root. A missing file yields an empty File (nil error).
func Read(root string) (*File, error) {
	b, err := os.ReadFile(Path(root))
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, err
	}
	var f File
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Write serializes the lockfile to root, sorted for stable git diffs.
func Write(root string, f *File) error {
	f.sortEntries()
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(Path(root), b, 0o644)
}

// key identifies the install slot an entry owns: a materialized stack owns its
// stack name; a vendor-only install is keyed by name@version.
func (e Entry) key() string {
	if e.InstalledAs != "" {
		return "stack:" + e.InstalledAs
	}
	return "vendor:" + e.Name + "@" + e.Version
}

// Upsert replaces any entry occupying the same slot, else appends.
func (f *File) Upsert(e Entry) {
	for i := range f.Packages {
		if f.Packages[i].key() == e.key() {
			f.Packages[i] = e
			f.sortEntries()
			return
		}
	}
	f.Packages = append(f.Packages, e)
	f.sortEntries()
}

// FindStack returns the entry that installed the given stack, or nil. Used to
// decide whether install owns a stack dir (safe to overwrite) vs. would clobber
// untracked content.
func (f *File) FindStack(stack string) *Entry {
	for i := range f.Packages {
		if f.Packages[i].InstalledAs == stack {
			return &f.Packages[i]
		}
	}
	return nil
}

func (f *File) sortEntries() {
	sort.Slice(f.Packages, func(i, j int) bool {
		if f.Packages[i].Name != f.Packages[j].Name {
			return f.Packages[i].Name < f.Packages[j].Name
		}
		return f.Packages[i].InstalledAs < f.Packages[j].InstalledAs
	})
}
