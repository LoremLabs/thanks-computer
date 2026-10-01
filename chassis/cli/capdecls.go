package cli

// Capability collection and checking for `txco apply`, `txco dev` and
// `txco lint`: the CAPS/ subtree holds one small YAML declaration per
// capability the stack answers (chassis/capdecl). Declarations are CODE —
// they deploy with the scopes that answer the call, inline in the draft
// like SANDBOXES/ — and every one is parsed here, before the server does
// the same on validate and activate: an unknown key, a bad input name, or
// an entry that names no scope of the stack fails the apply.
// Whether another stack of the tenant already declares the name is the
// server's check: only it knows what is active.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/capdecl"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
)

// collectCapFiles walks <stackDir>/CAPS/ and returns the draft rows
// (declarations inline). An absent CAPS/ yields nil, nil. Files the server
// would reject at the write boundary (nesting, a foreign extension, a bad
// name) fail here first.
func collectCapFiles(stackDir string) ([]client.StackFile, error) {
	dir := filepath.Join(stackDir, capdecl.Dir)
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
		rel := capdecl.Dir + "/" + name
		if e.IsDir() {
			return nil, fmt.Errorf("%s: nested directories are not allowed under %s/ (declarations are single <name>%s files)", rel, capdecl.Dir, capdecl.DeclExt)
		}
		if capdecl.Name(rel) == "" {
			return nil, fmt.Errorf("%s: capability declarations are <name>%s with a name of lowercase words joined by dots, such as crm.lookup", rel, capdecl.DeclExt)
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

// loadCapDecls parses <stackDir>/CAPS/*.yaml into a name → declaration
// map. A declaration that doesn't parse is reported against its path.
func loadCapDecls(stackDir string) (map[string]*capdecl.Decl, []string) {
	files, err := collectCapFiles(stackDir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	decls := map[string]*capdecl.Decl{}
	var errs []string
	for _, f := range files {
		d, perr := capdecl.ParseDecl([]byte(f.Content))
		if perr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", filepath.Join(stackDir, filepath.FromSlash(f.Path)), perr))
			continue
		}
		decls[capdecl.Name(f.Path)] = d
	}
	return decls, errs
}

// checkCapDecls parses every declaration of every stack the walked ops
// belong to, under <dir>/OPS/<stack>/CAPS/, and checks each entry against
// the scopes the stack's ops occupy. Two stacks of the workspace declaring
// one name is reported here too: it would fail the second activation. It
// returns one message per broken declaration.
func checkCapDecls(ops []bundle.Op, dir string) []string {
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
	var msgs []string
	owner := map[string]string{}
	for _, stack := range stacks {
		stackDir := filepath.Join(dir, "OPS", filepath.FromSlash(stack))
		decls, derrs := loadCapDecls(stackDir)
		msgs = append(msgs, derrs...)
		names := make([]string, 0, len(decls))
		for n := range decls {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			path := filepath.Join(stackDir, capdecl.Dir, n+capdecl.DeclExt)
			if err := capdecl.CheckEntry(decls[n], scopes[stack]); err != nil {
				msgs = append(msgs, fmt.Sprintf("%s: %v", path, err))
			}
			if other, dup := owner[n]; dup {
				msgs = append(msgs, fmt.Sprintf("%s: capability %q is already declared by stack %q (one stack answers a capability)", path, n, other))
				continue
			}
			owner[n] = stack
		}
	}
	sort.Strings(msgs)
	return msgs
}
