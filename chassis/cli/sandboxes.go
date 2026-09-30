package cli

// Sandbox collection and checking for `txco apply`, `txco dev` and
// `txco lint`: the SANDBOXES/ subtree holds one small YAML declaration per
// named bundle of what a program may hold (chassis/sandbox). Declarations
// are CODE — they deploy with the rules that open them, inline in the draft
// like OUTLETS/ — and every one is parsed here, before the server does the
// same on validate and activate: an unknown key, a missing env, a bad
// variable name or a bad reference fails the apply. Nothing is released at
// apply time.

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
	"github.com/loremlabs/thanks-computer/chassis/sandbox"
)

// collectSandboxFiles walks <stackDir>/SANDBOXES/ and returns the draft
// rows (declarations inline). An absent SANDBOXES/ yields nil, nil. Files
// the server would reject at the write boundary (nesting, a foreign
// extension, a bad name) fail here first.
func collectSandboxFiles(stackDir string) ([]client.StackFile, error) {
	dir := filepath.Join(stackDir, sandbox.Dir)
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
		rel := sandbox.Dir + "/" + name
		if e.IsDir() {
			return nil, fmt.Errorf("%s: nested directories are not allowed under %s/ (declarations are single <name>%s files)", rel, sandbox.Dir, sandbox.DeclExt)
		}
		if sandbox.Name(rel) == "" {
			return nil, fmt.Errorf("%s: sandbox declarations are <name>%s with a name matching [a-z][a-z0-9_-]*", rel, sandbox.DeclExt)
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

// loadSandboxDecls parses <stackDir>/SANDBOXES/*.yaml into a name →
// declaration map. A declaration that doesn't parse is reported against
// its path.
func loadSandboxDecls(stackDir string) (map[string]*sandbox.Decl, []string) {
	files, err := collectSandboxFiles(stackDir)
	if err != nil {
		return nil, []string{err.Error()}
	}
	decls := map[string]*sandbox.Decl{}
	var errs []string
	for _, f := range files {
		d, perr := sandbox.ParseDecl([]byte(f.Content))
		if perr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", filepath.Join(stackDir, filepath.FromSlash(f.Path)), perr))
			continue
		}
		decls[sandbox.Name(f.Path)] = d
	}
	return decls, errs
}

// checkSandboxDecls parses every declaration of every stack the walked ops
// belong to, under <dir>/OPS/<stack>/SANDBOXES/. Unlike the outlet check it
// does not wait for an op to name one: a sandbox is named at run time, by
// txco://delegate/mint, so nothing in the op text says which stack uses
// which. It returns one message per broken declaration.
func checkSandboxDecls(ops []bundle.Op, dir string) []string {
	var msgs []string
	seen := map[string]bool{}
	for _, op := range ops {
		if seen[op.Stack] {
			continue
		}
		seen[op.Stack] = true
		_, derrs := loadSandboxDecls(filepath.Join(dir, "OPS", filepath.FromSlash(op.Stack)))
		msgs = append(msgs, derrs...)
	}
	sort.Strings(msgs)
	return msgs
}
