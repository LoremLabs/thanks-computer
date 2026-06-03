package cli

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// hiddenTopLevelCases are top-level case arms in Dispatch that are
// intentionally NOT advertised via completion: back-compat aliases the
// help text doesn't list either. Update this allowlist (with rationale)
// when adding a new hidden alias.
var hiddenTopLevelCases = map[string]string{
	"push": "back-compat alias for `draft` (pre-rename) — not advertised",
}

// TestCompletionTopLevelMatchesDispatchSwitch parses chassis/cli/cli.go
// with go/parser, walks the Dispatch function's `switch args[0]`
// arms, and asserts every non-hidden literal case has a corresponding
// entry in cliCommandTree.
//
// Catches "added a new top-level command but forgot to register it for
// completion." If you add a top-level command, either add it to
// cliCommandTree (the normal path) or add it to hiddenTopLevelCases
// with a one-line rationale.
func TestCompletionTopLevelMatchesDispatchSwitch(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cli.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse cli.go: %v", err)
	}

	dispatchCases := collectDispatchCases(t, file)
	if len(dispatchCases) == 0 {
		t.Fatalf("no case arms collected from Dispatch — parser walk likely broken")
	}

	tree := make(map[string]bool, len(cliCommandTree))
	for _, n := range cliCommandTree {
		tree[n.Name] = true
		for _, al := range n.Aliases {
			tree[al] = true
		}
	}

	for c := range dispatchCases {
		switch c {
		// Flags / help variants — not commands, not completable.
		case "-h", "--help", "-v", "--version":
			continue
		}
		if _, hidden := hiddenTopLevelCases[c]; hidden {
			continue
		}
		if !tree[c] {
			t.Errorf("Dispatch arm %q has no entry in cliCommandTree; either add it to the tree or to hiddenTopLevelCases", c)
		}
	}
}

// collectDispatchCases walks the `Dispatch` function's body, finds the
// top-level `switch cmd { ... }`, and returns the set of literal case
// arms. Only string literals are considered (skips non-trivial case
// values).
func collectDispatchCases(t *testing.T, file *ast.File) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}

	var dispatch *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Name != nil && fn.Name.Name == "Dispatch" {
			dispatch = fn
			break
		}
	}
	if dispatch == nil {
		t.Fatalf("Dispatch function not found in cli.go")
	}

	ast.Inspect(dispatch.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		for _, stmt := range sw.Body.List {
			cc, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			for _, expr := range cc.List {
				lit, ok := expr.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v := strings.Trim(lit.Value, `"`)
				if v == "" {
					continue
				}
				out[v] = struct{}{}
			}
		}
		return false // don't walk deeper into nested switches
	})
	return out
}

// TestCompletionAllShellsEmit asserts each emitter produces non-empty
// output AND that every top-level command name from cliCommandTree
// appears in each script. Catches "emitter dropped a top-level node."
func TestCompletionAllShellsEmit(t *testing.T) {
	emitters := map[string]func([]node) string{
		"bash": emitBash,
		"zsh":  emitZsh,
		"fish": emitFish,
	}
	for shell, emit := range emitters {
		t.Run(shell, func(t *testing.T) {
			out := emit(cliCommandTree)
			if out == "" {
				t.Fatalf("%s emitter produced empty output", shell)
			}
			for _, n := range cliCommandTree {
				if !strings.Contains(out, n.Name) {
					t.Errorf("%s output missing top-level command %q", shell, n.Name)
				}
			}
		})
	}
}

// TestCompletionEmittersAvoidShellHazards is the regression that caught
// "command not found: package" when the user sourced the zsh script.
// The `packages` node had `Desc: "Alias for `package list`"` — backticks
// inside the double-quoted zsh `_values` entry triggered command
// substitution at script-load time. Sanitization (sanitizeDesc) strips
// shell-hazardous characters; this test pins that no emitter output
// contains a raw backtick or unescaped `$`.
func TestCompletionEmittersAvoidShellHazards(t *testing.T) {
	emitters := map[string]func([]node) string{
		"bash": emitBash,
		"zsh":  emitZsh,
		"fish": emitFish,
	}
	for shell, emit := range emitters {
		t.Run(shell, func(t *testing.T) {
			out := emit(cliCommandTree)
			// Strip lines that legitimately contain backticks/$ as
			// script syntax (these are the function body, not data).
			// We're looking specifically for hazards inside DATA —
			// the entries built from node descriptions.
			lines := strings.Split(out, "\n")
			for i, ln := range lines {
				// zsh _values entries look like:  "name[desc]"  (double-quoted)
				// fish complete lines:  complete -c txco -n "..." -a "..." -d "..."
				// bash compgen -W "...": just space-separated words
				// All three would be hazardous if a description has ` or $ in it.
				if strings.Contains(ln, "`") {
					t.Errorf("%s line %d contains raw backtick (zsh command substitution hazard): %q",
						shell, i+1, ln)
				}
				// Lines built from descriptions never need a literal $.
				// The zsh emitter does use ${...} for some shell variables
				// like $state, $line[1], ${fpath[1]}, $+functions — those
				// are syntax. Detect data-side $ by looking for $ inside
				// a description-shaped quoted string.
				if strings.Contains(ln, `["`) && strings.Contains(ln, `]"`) {
					// description-shaped — should have no $
					if strings.Contains(ln, "$") {
						t.Errorf("%s line %d has $ inside a description-shaped entry: %q",
							shell, i+1, ln)
					}
				}
			}
		})
	}
}

// TestCompletionAuthTreeIncludesKeyVerbs locks in the most-typed paths
// across the auth arc. If a future refactor drops one of these, the
// failure points exactly at the regression.
func TestCompletionAuthTreeIncludesKeyVerbs(t *testing.T) {
	mustContain := []string{
		// Top-level high-traffic verbs
		"apply",
		"dev",
		"serve",
		"completion",
		// Auth arc (the ai://chat smoke needed these)
		"hostnames",
		"add",
		"secrets",
		"set",
		"generate",
		"rotate",
		"verify",
		"revoke-actor",
	}
	emitters := map[string]func([]node) string{
		"bash": emitBash,
		"zsh":  emitZsh,
		"fish": emitFish,
	}
	for shell, emit := range emitters {
		t.Run(shell, func(t *testing.T) {
			out := emit(cliCommandTree)
			for _, v := range mustContain {
				if !strings.Contains(out, v) {
					t.Errorf("%s output missing critical verb %q", shell, v)
				}
			}
		})
	}
}

// TestRunCompletionDispatchesByShell verifies the top-level dispatcher
// honors each shell name and rejects unknowns.
func TestRunCompletionDispatchesByShell(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			rc := runCompletion([]string{shell}, &stdout, &stderr)
			if rc != 0 {
				t.Errorf("runCompletion(%q) rc = %d, want 0; stderr=%s", shell, rc, stderr.String())
			}
			if stdout.Len() == 0 {
				t.Errorf("runCompletion(%q) produced empty stdout", shell)
			}
		})
	}

	t.Run("unknown shell errors", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		rc := runCompletion([]string{"powershell"}, &stdout, &stderr)
		if rc != 2 {
			t.Errorf("rc = %d, want 2; stderr=%s", rc, stderr.String())
		}
		if !strings.Contains(stderr.String(), "unknown shell") {
			t.Errorf("stderr should say 'unknown shell'; got %s", stderr.String())
		}
	})

	t.Run("no args prints help", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		rc := runCompletion(nil, &stdout, &stderr)
		if rc != 0 {
			t.Errorf("rc = %d, want 0", rc)
		}
		if !strings.Contains(stdout.String(), "Usage: txco completion") {
			t.Errorf("expected usage on stdout; got %s", stdout.String())
		}
	})
}
