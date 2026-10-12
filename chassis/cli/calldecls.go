package cli

// Call-entry collection and checking for `txco apply`, `txco dev` and
// `txco lint`: the CALLS/ subtree holds one small YAML declaration per
// entry a call may name (chassis/calldecl). Declarations are CODE — they
// deploy with the scopes that answer the call, inline in the draft like
// CAPS/ — and every one is parsed here, before the server does the same on
// validate and activate: an unknown key, a bad field name, or an entry
// that names no scope of the stack fails the apply.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/calldecl"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/opname"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

// collectCallFiles walks <stackDir>/CALLS/ and returns the draft rows
// (declarations inline). An absent CALLS/ yields nil, nil. Files the server
// would reject at the write boundary (nesting, a foreign extension, a bad
// name) fail here first.
func collectCallFiles(stackDir string) ([]client.StackFile, error) {
	dir := filepath.Join(stackDir, calldecl.Dir)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []client.StackFile
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		rel := calldecl.Dir + "/" + name
		if e.IsDir() {
			return nil, fmt.Errorf("%s: nested directories are not allowed under %s/ (declarations are single <name>%s files)", rel, calldecl.Dir, calldecl.DeclExt)
		}
		if calldecl.Name(rel) == "" {
			return nil, fmt.Errorf("%s: call declarations are <name>%s with a name matching [a-z][a-z0-9_-]*, such as orient", rel, calldecl.DeclExt)
		}
		content, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			return nil, rerr
		}
		ch := sha256.Sum256(content)
		files = append(files, client.StackFile{
			Path:        rel,
			Content:     string(content),
			ContentHash: hex.EncodeToString(ch[:]),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// loadCallDecls parses <stackDir>/CALLS/*.yaml into a name → declaration
// map. A declaration that doesn't parse is reported against its path.
func loadCallDecls(stackDir string) (map[string]*calldecl.Decl, []string) {
	files, err := collectCallFiles(stackDir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	decls := map[string]*calldecl.Decl{}
	var errs []string
	for _, f := range files {
		d, perr := calldecl.ParseDecl([]byte(f.Content))
		if perr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", filepath.Join(stackDir, filepath.FromSlash(f.Path)), perr))
			continue
		}
		decls[calldecl.Name(f.Path)] = d
	}
	return decls, errs
}

// stackScopes is the set of scopes each walked stack's ops occupy.
func stackScopes(ops []bundle.Op) (map[string]map[int]bool, []string) {
	scopes := map[string]map[int]bool{}
	var stacks []string
	for _, op := range ops {
		if scopes[op.Stack] == nil {
			scopes[op.Stack] = map[int]bool{}
			stacks = append(stacks, op.Stack)
		}
		scopes[op.Stack][op.Scope] = true
	}
	sort.Strings(stacks)
	return scopes, stacks
}

// checkCallDecls parses every declaration of every stack the walked ops
// belong to, under <dir>/OPS/<stack>/CALLS/, and checks each entry against
// the scopes the stack's ops occupy. It returns one message per broken
// declaration.
func checkCallDecls(ops []bundle.Op, dir string) []string {
	scopes, stacks := stackScopes(ops)
	var msgs []string
	for _, stack := range stacks {
		stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(stack))
		decls, derrs := loadCallDecls(stackDir)
		msgs = append(msgs, derrs...)
		names := make([]string, 0, len(decls))
		for n := range decls {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if err := calldecl.CheckEntry(decls[n], scopes[stack]); err != nil {
				msgs = append(msgs, fmt.Sprintf("%s: %v", filepath.Join(stackDir, calldecl.Dir, n+calldecl.DeclExt), err))
			}
		}
	}
	sort.Strings(msgs)
	return msgs
}

// lintCallEntries warns about a `call://` that names, with a literal `WITH
// entry`, an entry of a callee in this apply: one the callee does not
// declare (the call would answer txco_call_no_entry), or one at or after
// whose scope the callee holds a rule a call cannot run (`WITH mode =
// "async"` or `"continuable"`; txco_call_unsupported when it fires). A
// callee outside the apply is lintCalls's warning; an entry chosen at run
// time is not knowable here. Warnings only.
func lintCallEntries(ops []bundle.Op, dir string) []string {
	var warnings []string
	_, stacks := stackScopes(ops)
	inBundle := map[string]bool{}
	for _, s := range stacks {
		inBundle[s] = true
	}
	refused := refusedRules(ops)
	declared := map[string]map[string]*calldecl.Decl{}
	for _, op := range ops {
		r, perr := txcl.Resonator(op.Txcl)
		if perr != nil || r == nil || !strings.HasPrefix(r.Exec, opname.CallScheme) {
			continue
		}
		entry, ok := literalString(r.With["entry"])
		if !ok || entry == "" {
			continue
		}
		stack, _, err := opname.CallTarget(r.Exec)
		if err != nil || !inBundle[stack] {
			continue
		}
		if _, lit := r.With["stack"].(ast.Literal); r.With["stack"] != nil && !lit {
			continue // the callee is chosen at run time
		}
		if declared[stack] == nil {
			decls, _ := loadCallDecls(filepath.Join(dir, "OPS", filepath.FromSlash(stack)))
			declared[stack] = decls
		}
		d := declared[stack][entry]
		if d == nil {
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s (%s/%d/%s) calls entry %q of stack %q, which declares no %s; the call answers txco_call_no_entry",
				op.SourcePath, op.Stack, op.Scope, op.Name, entry, stack, calldecl.DeclPath(entry)))
			continue
		}
		for _, rr := range refusedFrom(refused, stack, d.Entry) {
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s (%s/%d/%s) calls entry %q of stack %q, which enters at %d, and its rule %s cannot run inside a call; "+
					"the call ends with txco_call_unsupported when that rule fires",
				op.SourcePath, op.Stack, op.Scope, op.Name, entry, stack, d.Entry, rr.text))
		}
	}
	return warnings
}
