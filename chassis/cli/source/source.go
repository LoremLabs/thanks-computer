// Package source fetches a tree from a remote or local origin and copies it
// into a destination directory. It is the transport seam shared by
// `txco init --from <source>` and the package commands (`txco install`,
// `txco inspect`): a single `Source` interface with interchangeable
// implementations behind one `Parse`.
//
// Schemes:
//
//	github:owner/repo[@ref][/subpath]   public GitHub repo, fetched as a tar.gz
//	dir:./path                          a local directory tree
//	file:./path                         alias for dir:
//
// The interface is deliberately transport-agnostic: the Package System that
// consumes it never imports a transport's concrete types. A future `oci://`
// scheme drops in here as another Source (and is the only one that would pull
// in an OCI client) without changing any call site.
//
// The convention for `txco init`: a source is a *flat tree* meant to slot
// directly under `OPS/<stack>/` in the user's workspace. So given:
//
//	github:loremlabs/txco-templates/support-basic
//
// where the repo's `support-basic/` directory contains:
//
//	support-basic/
//	  100/resonator.txcl
//	  200/resonator.txcl
//	  triage/100/resonator.txcl
//
// running `txco init support --from github:loremlabs/txco-templates/support-basic`
// produces:
//
//	<cwd>/OPS/support/100/resonator.txcl
//	<cwd>/OPS/support/200/resonator.txcl
//	<cwd>/OPS/support/triage/100/resonator.txcl
//
// Package commands instead fetch a whole package tree (a `txco.package.yaml`
// at the root plus an `OPS/`-shaped subtree). No variable substitution.
package source

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// Source is anything that knows how to populate destDir with a fetched tree.
// Implementations: githubSource, dirSource (and, later, ociSource — the only
// one that imports an OCI client).
type Source interface {
	Spec() string
	Fetch(ctx context.Context, destDir string) (filesCopied int, err error)
}

// Parse turns a spec into a Source. Returns an error for unknown schemes or
// malformed specs so the caller can surface a clean message before any I/O.
func Parse(spec string) (Source, error) {
	switch {
	case strings.HasPrefix(spec, "github:"):
		return parseGitHub(strings.TrimPrefix(spec, "github:"))
	case strings.HasPrefix(spec, "dir:"):
		return parseDir(strings.TrimPrefix(spec, "dir:"), spec)
	case strings.HasPrefix(spec, "file:"):
		return parseDir(strings.TrimPrefix(spec, "file:"), spec)
	case strings.HasPrefix(spec, "oci:"):
		return newOCISource(spec)
	default:
		return nil, fmt.Errorf("unsupported source %q (try dir:./path, file:./path, github:owner/repo[@ref][/subpath], or oci://host/ns/name:tag)", spec)
	}
}

// Fetch is the one-shot entry point most callers want.
func Fetch(ctx context.Context, spec, destDir string) (int, error) {
	src, err := Parse(spec)
	if err != nil {
		return 0, err
	}
	return src.Fetch(ctx, destDir)
}

// safeRelPath reports whether rel is a safe relative path to join under a
// destination directory: not absolute, no ".." escape, not "."/"/"/"..".
// Returns the cleaned (forward-slash) path. Shared by the tar extractor and
// the local-dir copier so both apply one zip-slip guard.
func safeRelPath(rel string) (clean string, ok bool) {
	clean = path.Clean(rel)
	if clean == "." || clean == "/" || clean == ".." {
		return "", false
	}
	if strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", false
	}
	return clean, true
}
