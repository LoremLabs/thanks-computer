package cli

// Outlet collection and checking for `txco apply`, `txco dev` and
// `txco lint`: the OUTLETS/ subtree holds one small YAML declaration per
// external service a stack may call through outlet://<name>/<op>
// (chassis/outlet). Declarations are CODE — they deploy with the ops that
// use them, inline in the draft like DATASETS/ manifests — and the ops are
// checked against them here, before the server does the same on validate
// and activate: an undeclared outlet, exec on a read outlet, a statement
// that isn't a literal, more than one statement, or the wrong verb fails
// the apply. Nothing connects at apply time.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/cli/client"
	"github.com/loremlabs/thanks-computer/chassis/outlet"
	_ "github.com/loremlabs/thanks-computer/chassis/outlet/postgres" // the driver names ParseDecl accepts
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// collectOutletFiles walks <stackDir>/OUTLETS/ and returns the draft rows
// (declarations inline). An absent OUTLETS/ yields nil, nil. Files the
// server would reject at the write boundary (nesting, a foreign extension,
// a bad name) fail here first.
func collectOutletFiles(stackDir string) ([]client.StackFile, error) {
	dir := filepath.Join(stackDir, outlet.Dir)
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
		rel := outlet.Dir + "/" + name
		if e.IsDir() {
			return nil, fmt.Errorf("%s: nested directories are not allowed under %s/ (declarations are single <name>%s files)", rel, outlet.Dir, outlet.DeclExt)
		}
		if outlet.Name(rel) == "" {
			return nil, fmt.Errorf("%s: outlet declarations are <name>%s with a name matching [a-z][a-z0-9_-]*", rel, outlet.DeclExt)
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

// loadOutletDecls parses <stackDir>/OUTLETS/*.yaml into a name → declaration
// map. A declaration that doesn't parse is reported against its path.
func loadOutletDecls(stackDir string) (map[string]*outlet.Decl, []string) {
	files, err := collectOutletFiles(stackDir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	decls := map[string]*outlet.Decl{}
	var errs []string
	for _, f := range files {
		d, perr := outlet.ParseDecl([]byte(f.Content), outlet.Known)
		if perr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", filepath.Join(stackDir, filepath.FromSlash(f.Path)), perr))
			continue
		}
		decls[outlet.Name(f.Path)] = d
	}
	return decls, errs
}

// checkOutletOps runs the outlet check over every walked op whose EXEC
// names an outlet:// target, against the declarations of the op's own
// stack under <dir>/OPS/<stack>/OUTLETS/. It returns one message per
// problem, formatted like a parse error (source path, then stack/scope/
// name), and reports a broken declaration once per stack.
func checkOutletOps(ops []bundle.Op, dir string) []string {
	var msgs []string
	declsByStack := map[string]map[string]*outlet.Decl{}
	for _, op := range ops {
		if !strings.Contains(op.Txcl, outlet.SchemePrefix) {
			continue
		}
		r, err := txcl.Resonator(op.Txcl)
		if err != nil || !outlet.IsOutletExec(r.Exec) {
			continue // parse errors are reported by the parse gate
		}
		decls, ok := declsByStack[op.Stack]
		if !ok {
			var derrs []string
			decls, derrs = loadOutletDecls(filepath.Join(dir, "OPS", filepath.FromSlash(op.Stack)))
			declsByStack[op.Stack] = decls
			msgs = append(msgs, derrs...)
		}
		if cerr := outlet.CheckOp(r.Exec, r.With, decls); cerr != nil {
			msgs = append(msgs, fmt.Sprintf("%s (%s/%d/%s): %v", op.SourcePath, op.Stack, op.Scope, op.Name, cerr))
		}
	}
	return msgs
}
