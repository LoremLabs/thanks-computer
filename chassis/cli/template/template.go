// Package template fetches a starter tree from a remote source and copies it
// into a local directory. Used by `txco init --from <source>`.
//
// The convention: a template is a *flat tree* meant to slot directly under
// `OPS/<stack>/` in the user's workspace. So given:
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
// v1 supports the `github:` scheme only. Public repos only — fetched as a
// tar.gz from codeload.github.com. No variable substitution.
package template

import (
	"context"
	"fmt"
	"strings"
)

// Source is anything that knows how to populate destDir with template files.
// Today there's only one (githubSource); the interface is here so future
// schemes (gitlab:, file:, etc.) drop in without churning the call sites.
type Source interface {
	Spec() string
	Fetch(ctx context.Context, destDir string) (filesCopied int, err error)
}

// Parse turns a spec like `github:owner/repo[@ref][/subpath]` into a Source.
// Returns an error for unknown schemes or malformed specs so the caller can
// surface a clean message before doing any I/O.
func Parse(spec string) (Source, error) {
	switch {
	case strings.HasPrefix(spec, "github:"):
		return parseGitHub(strings.TrimPrefix(spec, "github:"))
	default:
		return nil, fmt.Errorf("unsupported template source %q (try github:owner/repo[@ref][/subpath])", spec)
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
