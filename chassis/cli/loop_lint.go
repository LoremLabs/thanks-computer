// Apply-time lint for unconditional loop shapes in the assembled stack.
//
// This is the design-time complement to the runtime budget guards
// (chassis/processor/budget.go). The runtime guards always catch loops via
// fuel / TTL exhaustion; the lint catches *typos* — the unconditional
// `EMIT @goto = "self/0"` that the author meant as `"self/1"` — before
// they hit production. Warnings only; `txco apply` continues regardless.
//
// Conservatism is the design: we surface only the unambiguous cases
// (unconditional self-loop, unconditional 2-stack ping-pong). Intentional
// polling and conditional state-machine loops slip past unflagged.
// Detecting deeper cycles or conditional loops without a counter-witness
// risks false positives on legitimate idioms; v1 sticks to the obvious.

package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

// stageRef is a (stack, scope) identity used to compare loop endpoints.
type stageRef struct {
	Stack string
	Scope int
}

// stagePartsRE mirrors processor.StagePartsRE — "<stack>/<scope>" with
// integer scope as the final path segment. The processor uses the same
// pattern at dispatch time; we use it here at parse time to recognize
// stage-jump EXEC targets.
var stagePartsRE = regexp.MustCompile(`^(.*)/+(\d+)$`)

// lintStackLoops scans the parsed-and-bundled ops for unconditional loop
// shapes and returns one warning line per shape. The caller prefixes
// "apply: " and prints to stderr; lint never returns errors.
//
// The two detected shapes:
//
//  1. Unconditional self-loop — a rule with no WHEN guard and no terminating
//     EMIT @halt that emits @goto back to its own (stack, scope) OR EXECs
//     into its own (stack, scope).
//
//  2. Unconditional 2-stack ping-pong — stage A points unconditionally to
//     stage B, and stage B points unconditionally back to A.
//
// Both shapes are statically detectable from EMIT overrides + EXEC targets;
// no symbolic execution, no envelope-value analysis.
func lintStackLoops(ops []bundle.Op) []string {
	var warnings []string

	// Build a graph of unconditional outgoing edges per (stack, scope).
	// Multiple rules at the same stage merge — any unconditional edge
	// from any rule contributes.
	edges := map[stageRef][]stageRef{}

	for _, op := range ops {
		r, perr := txcl.Resonator(op.Txcl)
		if perr != nil || r == nil {
			// Parse errors are reported by the upstream parse loop in
			// apply.go; lint silently skips so we don't double-report.
			continue
		}

		// "Guarded" = the rule won't fire unconditionally. A WHEN clause
		// gates the firing on envelope state; a halt EMIT terminates the
		// pipeline before the goto/EXEC can repeat. Either makes any
		// outbound edge conditional from the lint's point of view.
		if r.When != nil || ruleHalts(r) {
			continue
		}

		self := stageRef{Stack: op.Stack, Scope: op.Scope}

		// EMIT @goto = "<target>"
		if r.Emit != nil {
			for _, ov := range r.Emit.Overrides {
				if !isGotoPath(ov.Path) {
					continue
				}
				lit, ok := literalString(ov.Value)
				if !ok {
					continue
				}
				target, ok := resolveStageRef(lit, op.Stack)
				if !ok {
					continue
				}
				if target == self {
					warnings = append(warnings, fmt.Sprintf(
						"lint: %s (%s/%d/%s) unconditionally emits @goto back to its own stage",
						op.SourcePath, op.Stack, op.Scope, op.Name))
				}
				edges[self] = append(edges[self], target)
			}
		}

		// EXEC "<stack>/<scope>" — unschemed stage jump.
		if r.Exec != "" {
			if target, ok := parseStageJump(r.Exec); ok {
				if target == self {
					warnings = append(warnings, fmt.Sprintf(
						"lint: %s (%s/%d/%s) unconditionally EXECs into its own stage",
						op.SourcePath, op.Stack, op.Scope, op.Name))
				}
				edges[self] = append(edges[self], target)
			}
		}
	}

	// 2-stack ping-pong: A -> B and B -> A, both unconditional, distinct.
	// Self-loops were already caught above; skip them here.
	seenCycle := map[string]bool{}
	for from, targets := range edges {
		for _, to := range targets {
			if from == to {
				continue
			}
			backs, ok := edges[to]
			if !ok {
				continue
			}
			for _, back := range backs {
				if back != from {
					continue
				}
				// De-dup by canonical key (smaller endpoint first) so
				// A<->B and B<->A produce one warning, not two.
				key := canonCycleKey(from, to)
				if seenCycle[key] {
					continue
				}
				seenCycle[key] = true
				warnings = append(warnings, fmt.Sprintf(
					"lint: unconditional 2-stack cycle: %s/%d <-> %s/%d",
					from.Stack, from.Scope, to.Stack, to.Scope))
			}
		}
	}

	return warnings
}

// ruleHalts reports whether the rule emits a terminating `@halt = true`.
// A halt EMIT terminates the pipeline after this scope's merge, so any
// goto/EXEC the rule also carries cannot loop.
func ruleHalts(r *resonator.Resonator) bool {
	if r == nil || r.Emit == nil {
		return false
	}
	for _, ov := range r.Emit.Overrides {
		if !isHaltPath(ov.Path) {
			continue
		}
		if lit, ok := literalBool(ov.Value); ok && lit {
			return true
		}
	}
	return false
}

// isGotoPath matches the EMIT-side paths that the chassis interprets as
// `_txc.goto`. The processor normalizes `@foo` to `_txc.foo` at evaluation
// time; we accept the same surface forms here so the lint matches how
// authors actually write the rule.
func isGotoPath(p string) bool {
	p = strings.TrimPrefix(p, ".")
	return p == "_txc.goto" || p == "@goto"
}

// isHaltPath matches the EMIT-side paths that the chassis interprets as
// `_txc.halt`. Same surface forms as the goto path.
func isHaltPath(p string) bool {
	p = strings.TrimPrefix(p, ".")
	return p == "_txc.halt" || p == "@halt"
}

// literalString extracts the underlying string from an ast.Literal, or
// returns ok=false if the value is not a literal string (PathRef,
// FunctionCall, or non-string literal — all of which the lint treats as
// unknown / conditional).
func literalString(v ast.Value) (string, bool) {
	lit, ok := v.(ast.Literal)
	if !ok {
		return "", false
	}
	s, ok := lit.V.(string)
	return s, ok
}

// literalBool extracts the underlying bool from an ast.Literal.
func literalBool(v ast.Value) (bool, bool) {
	lit, ok := v.(ast.Literal)
	if !ok {
		return false, false
	}
	b, ok := lit.V.(bool)
	return b, ok
}

// resolveStageRef parses a goto literal value into a stageRef. The
// processor's resolveGoto accepts either "<stack>/<scope>" or bare
// "<scope>" (interpreted as the current stack). We mirror the same
// resolution so the lint sees the same target the runtime would.
func resolveStageRef(literal, currentStack string) (stageRef, bool) {
	literal = strings.TrimSpace(literal)
	if literal == "" {
		return stageRef{}, false
	}
	if m := stagePartsRE.FindStringSubmatch(literal); m != nil {
		scope, err := strconv.Atoi(m[2])
		if err != nil {
			return stageRef{}, false
		}
		return stageRef{Stack: m[1], Scope: scope}, true
	}
	// Bare numeric → current stack, that scope.
	if n, err := strconv.Atoi(literal); err == nil {
		return stageRef{Stack: currentStack, Scope: n}, true
	}
	return stageRef{}, false
}

// parseStageJump matches the unschemed EXEC stage-jump syntax — the
// "<stack>/<scope>" form the processor synthesizes into _txc.goto at
// dispatch time.
func parseStageJump(exec string) (stageRef, bool) {
	exec = strings.TrimSpace(exec)
	if exec == "" {
		return stageRef{}, false
	}
	// Schemed forms (http://, txco://, etc.) are not stage jumps;
	// the runtime treats them as op dispatches, not loops.
	if strings.Contains(exec, "://") {
		return stageRef{}, false
	}
	m := stagePartsRE.FindStringSubmatch(exec)
	if m == nil {
		return stageRef{}, false
	}
	scope, err := strconv.Atoi(m[2])
	if err != nil {
		return stageRef{}, false
	}
	return stageRef{Stack: m[1], Scope: scope}, true
}

// canonCycleKey produces a stable identifier for an unordered (A, B) pair
// so that the (A->B, B->A) cycle is reported once rather than twice.
func canonCycleKey(a, b stageRef) string {
	ak := a.Stack + "/" + strconv.Itoa(a.Scope)
	bk := b.Stack + "/" + strconv.Itoa(b.Scope)
	if ak < bk {
		return ak + "|" + bk
	}
	return bk + "|" + ak
}
