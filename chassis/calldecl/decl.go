// Package calldecl is the declaration of an ENTRY a call may name: a stack
// says, in one small YAML file under the reserved CALLS/ subtree, where a
// call enters it, what the call must carry and what the answer will carry.
//
//	OPS/app/_orient/CALLS/orient.yaml
//
//	description: Compile the context needed for a job.
//	entry: 100
//	timeout: 20000
//	input:
//	  job:
//	    description: the job record
//	    required: true
//	  text:
//	    description: the message to answer
//	output:
//	  prompt:
//	    description: the compiled prompt
//	    required: true
//	  sources:
//	    description: the documents it drew on
//
// A rule names the entry when it calls: `EXEC "call://app/_orient" WITH
// entry = "orient"`. The chassis enters the stack at the declared scope,
// bounds the call by the declared timeout when that is shorter, refuses a
// call that lacks a required input before the callee runs
// (txco_call_invalid_input) and an answer that lacks a required output
// after it ends (txco_call_invalid_output). Nothing else is checked: the
// shape of a field is the stacks' business. A call that names no entry is
// what it was before declarations existed.
//
// The shape is the capability declaration's (chassis/capdecl), so a stack
// that answers callers inside the tenant and callers outside it writes the
// same thing twice with different names. The file deploys with `txco apply`
// as a stack_files row, beside CAPS/, and is validated at apply.
//
// This file is the leaf layer: the reserved-path vocabulary, with no
// dependency on stores, so the CLI, the admin producer and the processor
// can import it cheaply.
package calldecl

import (
	"errors"
	"regexp"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
)

// Reserved top-level directory and the declaration extension.
const (
	Dir     = "CALLS"
	DeclExt = ".yaml"

	kind = "call declaration"
)

// Decl is one CALLS/<name>.yaml: the capability declaration's shape.
type Decl = capdecl.Decl

// Field is one input an entry takes, or one output it answers with.
type Field = capdecl.Field

// ErrNotDeclared: the stack's active version has no CALLS/<name>.yaml.
var ErrNotDeclared = errors.New("call: not declared")

// nameRe pins entry names: they appear in `WITH entry` and as a file's
// stem, so a lowercase identifier with _ and -.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// ValidName reports whether s may name an entry.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// ParseDecl strictly decodes and validates a declaration body.
func ParseDecl(data []byte) (*Decl, error) { return capdecl.ParseDeclAs(kind, data) }

// CheckEntry reports whether the declaration's entry names a scope the
// stack has.
func CheckEntry(d *Decl, scopes map[int]bool) error { return capdecl.CheckEntryAs(kind, d, scopes) }

// IsCallPath reports whether a stack_files path lives under CALLS/.
func IsCallPath(p string) bool { return strings.HasPrefix(p, Dir+"/") }

// Name returns the entry a declaration path belongs to:
// "CALLS/orient.yaml" → "orient". It returns "" when the path is not under
// CALLS/, is nested, has the wrong extension, or names an invalid entry —
// the same shape validateStackFilePath enforces at upload.
func Name(p string) string {
	if !IsCallPath(p) {
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

// DeclPath is the stack_files path of an entry's declaration.
func DeclPath(name string) string { return Dir + "/" + name + DeclExt }
