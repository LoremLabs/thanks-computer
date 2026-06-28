package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/pflag"

	"github.com/loremlabs/thanks-computer/chassis/cli/banner"
	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// runLint validates the local OPS/ tree WITHOUT a chassis. It runs the same
// walker `apply` uses (bundle.WalkDiag — stack/scope/name resolution plus the
// name-collision and "no numbered step directory" diagnostics) and then
// strict-parses every rule's txcl. It's the offline pre-flight that `apply` and
// `diff` can't be (they need a running server), and `--list` prints the
// resolved op graph (the stack's execution order at a glance). Exits 1 on any
// diagnostic or parse error, so it drops cleanly into CI / a pre-commit hook.
func runLint(args []string, stdout, stderr io.Writer) int {
	fs := pflag.NewFlagSet("lint", pflag.ContinueOnError)
	fs.SetOutput(stderr)
	listOut := fs.Bool("list", false, "also print the resolved op graph (stack → scope → name)")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON instead of the text report")
	fs.Usage = func() {
		banner.PrintLogo(stderr)
		fmt.Fprint(stderr, `
Usage: txco lint [flags] [<dir>]

Validate the local <dir>/OPS/ tree offline (no chassis): resolve every rule's
stack/scope/name, report name collisions and mis-placed files, and strict-parse
each rule's txcl. <dir> defaults to "." and walks up to the nearest OPS/. Exits
1 if anything is wrong — safe for CI and pre-commit. Covers the same app stacks
'txco apply' deploys (system '_'-stacks are validated by the chassis at boot).

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	dirArg := ""
	if fs.NArg() > 0 {
		dirArg = fs.Arg(0)
	}
	dir, err := workspaceDir(dirArg)
	if err != nil {
		fmt.Fprintf(stderr, "lint: resolve dir: %v\n", err)
		return 1
	}

	ops, diags, err := bundle.WalkDiag(dir)
	if err != nil {
		fmt.Fprintf(stderr, "lint: walk %s: %v\n", dir, err)
		return 1
	}

	// WalkDiag only checks directory structure; strict-parse every rule to
	// catch txcl syntax errors the walker can't see.
	var parseErrs []lintParseError
	for _, op := range ops {
		if msgs := txcl.Validate(op.Txcl); len(msgs) > 0 {
			parseErrs = append(parseErrs, lintParseError{op.Stack, op.Scope, op.Name, msgs})
		}
	}

	// Reuse apply's static loop-shape lint (unconditional self-loops /
	// 2-stack ping-pongs). Warnings only, same conservative semantics as
	// apply (see loop_lint.go) — they don't flip the exit code.
	loopWarns := lintStackLoops(ops)

	// Stable order for the listing and the report.
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Stack != ops[j].Stack {
			return ops[i].Stack < ops[j].Stack
		}
		if ops[i].Scope != ops[j].Scope {
			return ops[i].Scope < ops[j].Scope
		}
		return ops[i].Name < ops[j].Name
	})
	sort.Slice(parseErrs, func(i, j int) bool {
		if parseErrs[i].Stack != parseErrs[j].Stack {
			return parseErrs[i].Stack < parseErrs[j].Stack
		}
		return parseErrs[i].Scope < parseErrs[j].Scope
	})

	stackCount := countStacks(ops)
	clean := len(diags) == 0 && len(parseErrs) == 0

	if *jsonOut {
		return emitLintJSON(stdout, stderr, ops, stackCount, diags, parseErrs, loopWarns, clean)
	}

	if *listOut {
		printOpGraph(stdout, ops)
	}

	if len(ops) == 0 && len(diags) == 0 {
		fmt.Fprintf(stdout, "lint: no operations found under %s/OPS/\n", dir)
		return 0
	}

	for _, d := range diags {
		fmt.Fprintf(stdout, "✗ %s\n", d.Msg)
	}
	for _, pe := range parseErrs {
		fmt.Fprintf(stdout, "✗ %s/%d/%s: %s\n", pe.Stack, pe.Scope, pe.Name, joinMsgs(pe.Messages))
	}
	for _, w := range loopWarns {
		fmt.Fprintf(stdout, "⚠ %s\n", w)
	}

	if clean {
		if len(loopWarns) == 0 {
			fmt.Fprintf(stdout, "✓ %d operation(s) across %d stack(s) — no issues\n", len(ops), stackCount)
		} else {
			fmt.Fprintf(stdout, "✓ %d operation(s) across %d stack(s) — %d warning(s)\n", len(ops), stackCount, len(loopWarns))
		}
		return 0
	}
	fmt.Fprintf(stdout, "\n%d diagnostic(s), %d parse error(s), %d warning(s)\n", len(diags), len(parseErrs), len(loopWarns))
	return 1
}

// lintParseError is one rule that failed strict txcl validation.
type lintParseError struct {
	Stack    string   `json:"stack"`
	Scope    int      `json:"scope"`
	Name     string   `json:"name"`
	Messages []string `json:"messages"`
}

// printOpGraph prints ops grouped by stack in execution order — the stack's
// "graph" at a glance. ops must already be sorted by (stack, scope, name).
func printOpGraph(w io.Writer, ops []bundle.Op) {
	cur := ""
	for _, op := range ops {
		if op.Stack != cur {
			if cur != "" {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "%s\n", op.Stack)
			cur = op.Stack
		}
		fmt.Fprintf(w, "  %6d  %s\n", op.Scope, op.Name)
	}
	if cur != "" {
		fmt.Fprintln(w)
	}
}

func countStacks(ops []bundle.Op) int {
	seen := map[string]struct{}{}
	for _, op := range ops {
		seen[op.Stack] = struct{}{}
	}
	return len(seen)
}

func joinMsgs(msgs []string) string {
	out := ""
	for i, m := range msgs {
		if i > 0 {
			out += "; "
		}
		out += m
	}
	return out
}

// emitLintJSON writes a stable {ok, stacks, ops, diagnostics, parseErrors,
// graph} object. The graph is always included so machine consumers get the op
// map regardless of --list. Exit mirrors the text path: 1 when not clean.
func emitLintJSON(stdout, stderr io.Writer, ops []bundle.Op, stackCount int, diags []bundle.Diag, parseErrs []lintParseError, warnings []string, clean bool) int {
	type diagJSON struct {
		Stack   string `json:"stack"`
		Path    string `json:"path"`
		Message string `json:"message"`
	}
	type opJSON struct {
		Stack string `json:"stack"`
		Scope int    `json:"scope"`
		Name  string `json:"name"`
	}
	ds := make([]diagJSON, 0, len(diags))
	for _, d := range diags {
		ds = append(ds, diagJSON{d.Stack, d.Path, d.Msg})
	}
	if parseErrs == nil {
		parseErrs = []lintParseError{}
	}
	if warnings == nil {
		warnings = []string{}
	}
	graph := make([]opJSON, 0, len(ops))
	for _, op := range ops {
		graph = append(graph, opJSON{op.Stack, op.Scope, op.Name})
	}
	payload := struct {
		OK          bool             `json:"ok"`
		Stacks      int              `json:"stacks"`
		Ops         int              `json:"ops"`
		Diagnostics []diagJSON       `json:"diagnostics"`
		ParseErrors []lintParseError `json:"parseErrors"`
		Warnings    []string         `json:"warnings"`
		Graph       []opJSON         `json:"graph"`
	}{clean, stackCount, len(ops), ds, parseErrs, warnings, graph}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		fmt.Fprintf(stderr, "lint: encode json: %v\n", err)
		return 1
	}
	if clean {
		return 0
	}
	return 1
}
