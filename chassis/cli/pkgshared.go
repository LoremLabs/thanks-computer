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
	"github.com/loremlabs/thanks-computer/chassis/cli/manifest"
	"github.com/loremlabs/thanks-computer/chassis/cli/source"
)

// fetchPackage fetches a package source into a fresh temp dir and returns the
// dir plus a cleanup func. Works uniformly for dir:/file:/github: sources, so
// inspect/install share one fetch path.
func fetchPackage(spec string) (dir string, cleanup func(), err error) {
	tmp, err := os.MkdirTemp("", "txco-pkg-*")
	if err != nil {
		return "", func() {}, err
	}
	if _, err := source.Fetch(context.Background(), spec, tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", func() {}, err
	}
	return tmp, func() { _ = os.RemoveAll(tmp) }, nil
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

// displayPath renders p relative to root when possible, for friendly output.
func displayPath(root, p string) string {
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return p
}
