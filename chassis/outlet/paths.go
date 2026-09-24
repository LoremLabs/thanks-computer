// Package outlet implements outlet://<name>/<op>: a stack-declared, named
// connection to an external service the tenant already runs. The chassis
// holds the credential, owns the connection pool, bounds every call and
// hands the stack JSON; the stack names an outlet and an operation and never
// sees a DSN, a password or a socket.
//
// A stack declares an outlet as one small YAML file under the reserved
// OUTLETS/ subtree, beside FILES/ and DATASETS/:
//
//	OPS/<stack>/OUTLETS/<name>.yaml
//
// The file deploys with `txco apply` as a stack_files row (inline on a
// single node, fingerprint + CAS on the fleet — the DATASETS/ manifest
// treatment) and is validated at apply; nothing connects until an op runs.
//
// This file is the leaf layer: the reserved-path vocabulary, with no
// dependency on drivers or stores, so the CLI, the admin producer and the
// control-event applier can import it cheaply.
package outlet

import (
	"regexp"
	"strings"
)

// Reserved top-level directory and the declaration extension.
const (
	Dir     = "OUTLETS"
	DeclExt = ".yaml"
)

// nameRe pins outlet names: they appear in EXEC URIs (`outlet://crm/query`)
// and error payloads, so lowercase identifiers only, no whitespace, no
// quoting surprises.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidName reports whether s is an acceptable outlet name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// IsOutletPath reports whether a stack_files path lives under OUTLETS/.
func IsOutletPath(p string) bool { return strings.HasPrefix(p, Dir+"/") }

// Name returns the outlet a declaration path belongs to:
// "OUTLETS/crm.yaml" → "crm". It returns "" when the path is not under
// OUTLETS/, is nested, has the wrong extension, or names an invalid outlet —
// the same shape validateStackFilePath enforces at upload.
func Name(p string) string {
	if !IsOutletPath(p) {
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

// DeclPath is the stack_files path of an outlet's declaration.
func DeclPath(name string) string { return Dir + "/" + name + DeclExt }
