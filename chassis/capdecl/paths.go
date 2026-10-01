// Package capdecl is the declaration of what a stack can do for a run: a
// named capability, the scope of the stack that answers it, and the input
// it takes. A program a run grant covers calls a capability by name
// (`cap://<name>`, chassis/server/capgw); the run's `_grant` stack decides
// the call; the stack that DECLARES the name runs it, from the scope the
// declaration names.
//
// A stack declares a capability as one small YAML file under the reserved
// CAPS/ subtree, beside SANDBOXES/ and OUTLETS/:
//
//	OPS/<stack>/CAPS/<name>.yaml
//
// The file deploys with `txco apply` as a stack_files row (inline on a
// single node, fingerprint + CAS on the fleet — the SANDBOXES/ treatment)
// and is validated at apply. A SANDBOXES/ declaration says what a run MAY
// call; a CAPS/ declaration says who answers. A tenant's catalogue is the
// declarations of its active stacks: `txco://caps/list`, `txco caps list`.
//
// This file is the leaf layer: the reserved-path vocabulary, with no
// dependency on stores, so the CLI, the admin producer and the control-event
// applier can import it cheaply.
package capdecl

import (
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// Reserved top-level directory and the declaration extension.
const (
	Dir     = "CAPS"
	DeclExt = ".yaml"
)

// IsCapPath reports whether a stack_files path lives under CAPS/.
func IsCapPath(p string) bool { return strings.HasPrefix(p, Dir+"/") }

// Name returns the capability a declaration path belongs to:
// "CAPS/local.web.fetch.yaml" → "local.web.fetch". It returns "" when the
// path is not under CAPS/, is nested, has the wrong extension, or names an
// invalid capability — the same shape validateStackFilePath enforces at
// upload. A capability name holds dots, so the extension is trimmed off the
// end rather than cut at the first dot.
func Name(p string) string {
	if !IsCapPath(p) {
		return ""
	}
	rest := p[len(Dir)+1:]
	if rest == "" || strings.Contains(rest, "/") || !strings.HasSuffix(rest, DeclExt) {
		return ""
	}
	name := strings.TrimSuffix(rest, DeclExt)
	if !sandbox.ValidCapability(name) {
		return ""
	}
	return name
}

// DeclPath is the stack_files path of a capability's declaration.
func DeclPath(name string) string { return Dir + "/" + name + DeclExt }
