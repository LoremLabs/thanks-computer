package cli

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/lockfile"
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// fetchPackage fetches a package source into a fresh temp dir and returns the
// dir, its provenance (blank unless the source is a Resolver, i.e. oci:), and a
// cleanup func. Works uniformly for dir:/file:/github:/oci: sources, so
// inspect/install share one fetch path.
func fetchPackage(spec string) (dir string, prov source.Provenance, cleanup func(), err error) {
	src, err := source.Parse(spec)
	if err != nil {
		return "", source.Provenance{}, func() {}, err
	}
	tmp, err := os.MkdirTemp("", "txco-pkg-*")
	if err != nil {
		return "", source.Provenance{}, func() {}, err
	}
	if _, err := src.Fetch(context.Background(), tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", source.Provenance{}, func() {}, err
	}
	if r, ok := src.(source.Resolver); ok {
		prov = r.Resolved()
	}
	return tmp, prov, func() { _ = os.RemoveAll(tmp) }, nil
}

// stackSummary describes one exported stack for inspect / dry-run output.
type stackSummary struct {
	Name   string `json:"name"`
	Scopes int    `json:"scopes"`
	Rules  int    `json:"rules"`
}

// summarizeStacks groups walked ops into per-stack scope/rule counts.
func summarizeStacks(ops []bundle.Op) []stackSummary {
	type acc struct {
		scopes map[int]bool
		rules  int
	}
	m := map[string]*acc{}
	for _, op := range ops {
		a := m[op.Stack]
		if a == nil {
			a = &acc{scopes: map[int]bool{}}
			m[op.Stack] = a
		}
		a.scopes[op.Scope] = true
		a.rules++
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]stackSummary, 0, len(names))
	for _, name := range names {
		out = append(out, stackSummary{Name: name, Scopes: len(m[name].scopes), Rules: m[name].rules})
	}
	return out
}

// exportedStackNames returns the distinct stack names in ops, sorted.
func exportedStackNames(ops []bundle.Op) []string {
	seen := map[string]bool{}
	var out []string
	for _, op := range ops {
		if !seen[op.Stack] {
			seen[op.Stack] = true
			out = append(out, op.Stack)
		}
	}
	sort.Strings(out)
	return out
}

// printRequiredOpStubs prints a txco.yaml `operations:` block the user can
// paste — one stub per required op. We PRINT (never write txco.yaml) to avoid
// clobbering the user's comments or duplicating the `operations:` key, and we
// never invent a URL (the example is a comment).
func printRequiredOpStubs(w io.Writer, reqs []manifest.RequiredOp) {
	if len(reqs) == 0 {
		return
	}
	fmt.Fprintln(w, "\nThis package needs these external operations. Add them to your")
	fmt.Fprintln(w, "txco.yaml `operations:` block and fill in real URLs:")
	fmt.Fprintln(w, "\noperations:")
	for _, r := range reqs {
		tail := ""
		if r.Description != "" {
			tail = "  (" + r.Description + ")"
		}
		if r.Example != "" {
			fmt.Fprintf(w, "  %s:\n    url: \"\"   # example: %s%s\n", r.Name, r.Example, tail)
		} else {
			fmt.Fprintf(w, "  %s:\n    url: \"\"%s\n", r.Name, tail)
		}
	}
}

// copyTree copies the regular files under src into dst (creating dirs),
// skipping symlinks/devices. The src here is an already-fetched, validated,
// size-capped staging tree, so this is a plain recursive copy.
func copyTree(src, dst string) (int, error) {
	var n int
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil // skip symlinks, devices, fifos
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

// listTreeRel returns the sorted, slash-separated relative paths of the
// regular files under dir — the exact set copyTree would write, for an
// accurate dry-run preview.
func listTreeRel(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(out)
	return out, err
}

// installedStackHash recomputes the install-time manifest hash for the stack
// materialized at OPS/<installedAs>/, using the SAME pipeline install used:
// bundle.WalkFS over the workspace OPS/ tree, filtered to this stack, then
// opsToFiles + localManifestHash. install stored exactly this value (over the
// package's exported stack — opsToFiles drops the stack name, so an `--as`
// rename re-hashes identically; copyTree is a byte copy, so an unedited stack
// reproduces the hash exactly).
//
// This is intentionally NOT loadLocalStackFiles/localStackClean (stacks_cmd.go):
// those hash literal on-disk scope dirs (`0100_TRIAGE/`) and raw mock bytes for
// the chassis-state hash — a different hash universe that would never match the
// lockfile. exists is false when OPS/<installedAs>/ is absent.
func installedStackHash(root, installedAs string) (hash string, exists bool, err error) {
	fi, statErr := os.Stat(filepath.Join(root, "OPS", installedAs))
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return "", false, nil
		}
		return "", false, statErr
	}
	if !fi.IsDir() {
		return "", false, nil
	}
	ops, err := bundle.WalkFS(os.DirFS(root), ".")
	if err != nil {
		return "", true, err
	}
	var filtered []bundle.Op
	for _, op := range ops {
		if op.Stack == installedAs {
			filtered = append(filtered, op)
		}
	}
	return localManifestHash(opsToFiles(filtered)), true, nil
}

// stackEditState reports whether the on-disk stack for an as-stack entry still
// matches what install materialized: "clean" (hash matches the lockfile),
// "edited" (differs — hand-edited since install), or "missing"
// (OPS/<installedAs>/ is gone). Vendor-only entries (no InstalledAs) own no
// OPS/ stack and always report "clean".
func stackEditState(root string, e lockfile.Entry) (string, error) {
	if e.InstalledAs == "" {
		return "clean", nil
	}
	h, exists, err := installedStackHash(root, e.InstalledAs)
	if err != nil {
		return "", err
	}
	if !exists {
		return "missing", nil
	}
	if h == e.ManifestHash {
		return "clean", nil
	}
	return "edited", nil
}

// displayPath renders p relative to root when possible, for friendly output.
func displayPath(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}
