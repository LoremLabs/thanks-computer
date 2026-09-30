// Package sandbox is the declaration of what a program may hold: a named,
// stack-deployed bundle that says which secrets a piece of work is handed
// and under which names. A run grant (chassis/authn) names sandboxes; the
// chassis opens one when a rule starts a command with `WITH grant, sandbox`
// or when a program the work runs asks for it through `txco sandbox`. The
// program never picks a secret by name: it gets what the sandbox says.
//
// A stack declares a sandbox as one small YAML file under the reserved
// SANDBOXES/ subtree, beside OUTLETS/ and DATASETS/:
//
//	OPS/<stack>/SANDBOXES/<name>.yaml
//
// The file deploys with `txco apply` as a stack_files row (inline on a
// single node, fingerprint + CAS on the fleet — the OUTLETS/ treatment) and
// is validated at apply; nothing is released until a run grant that names
// the sandbox is opened.
//
// This file is the leaf layer: the reserved-path vocabulary, with no
// dependency on stores, so the CLI, the admin producer and the control-event
// applier can import it cheaply.
package sandbox

import (
	"regexp"
	"strings"
)

// Reserved top-level directory and the declaration extension.
const (
	Dir     = "SANDBOXES"
	DeclExt = ".yaml"
)

// nameRe pins sandbox names: they appear in a run grant's allowlist, on a
// command line (`txco sandbox github -- …`) and in error payloads, so
// lowercase identifiers only, no whitespace, no quoting surprises.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidName reports whether s is an acceptable sandbox name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// IsSandboxPath reports whether a stack_files path lives under SANDBOXES/.
func IsSandboxPath(p string) bool { return strings.HasPrefix(p, Dir+"/") }

// Name returns the sandbox a declaration path belongs to:
// "SANDBOXES/github.yaml" → "github". It returns "" when the path is not
// under SANDBOXES/, is nested, has the wrong extension, or names an invalid
// sandbox — the same shape validateStackFilePath enforces at upload.
func Name(p string) string {
	if !IsSandboxPath(p) {
		return ""
	}
	rest := p[len(Dir)+1:]
	if rest == "" || strings.Contains(rest, "/") || !strings.HasSuffix(rest, DeclExt) {
		return ""
	}
	name := strings.TrimSuffix(rest, DeclExt)
	if !ValidName(name) {
		return ""
	}
	return name
}

// DeclPath is the stack_files path of a sandbox's declaration.
func DeclPath(name string) string { return Dir + "/" + name + DeclExt }
