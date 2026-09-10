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

// lintCrossStackGoto warns when a rule emits a `@goto` literal naming a stack
// other than the one the rule lives in.
//
// A goto moves the STAGE but not the envelope's stack identity: `_txc.stack` is
// stamped once by the inlet (server/ingress/router.go, server.go) and nothing in
// the processor rewrites it, while resolveGoto only rebuilds the stage string.
// So a rule that jumps from `core` into `kind-x` runs kind-x's ops while still
// identifying as `core` — and every stack-scoped default follows the identity,
// not the stage: the kv namespace (server/kv.go), the read-file root
// (server/readfile.go) and the dataset root (server/dataset.go). A `kv/set` with
// no explicit namespace lands in the ORIGIN stack's namespace, silently.
//
// `txco://route` is the intended cross-stack mechanism: routeBody emits
// `_txc.goto` and `_txc.stack` together, so the identity follows the jump. Hence
// a warning rather than an error — a bare cross-stack goto is legal and works so
// long as every stack-scoped read in the target is explicit, which is the sort of
// thing that is true on the day it is written and false six months later.
//
// Unlike lintStackLoops this does NOT skip guarded rules: a real dispatch is
// almost always behind a WHEN, so skipping them would skip every true positive.
// Only literal targets are checked — a path-valued goto (`@goto = ._ret`) is the
// subroutine-return idiom and its target is not knowable statically.
func lintCrossStackGoto(ops []bundle.Op) []string {
	var warnings []string

	for _, op := range ops {
		r, perr := txcl.Resonator(op.Txcl)
		if perr != nil || r == nil || r.Emit == nil {
			// Parse errors are reported by the upstream parse loop in
			// apply.go; lint silently skips so we don't double-report.
			continue
		}
		for _, ov := range r.Emit.Overrides {
			if !isGotoPath(ov.Path) {
				continue
			}
			lit, ok := literalString(ov.Value)
			if !ok {
				continue
			}
			target, ok := resolveStageRef(lit, op.Stack)
			if !ok || target.Stack == op.Stack {
				continue
			}
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s (%s/%d/%s) emits @goto into stack %q — a cross-stack goto does "+
					"not re-pin _txc.stack, so kv/read-file/dataset defaults inside %q still "+
					"resolve against %q; use txco://route to re-pin, or make every "+
					"stack-scoped read there explicit",
				op.SourcePath, op.Stack, op.Scope, op.Name,
				target.Stack, target.Stack, op.Stack))
		}
	}

	return warnings
}

// lintRepeatDirectives checks the in-op repeat family — `WITH
// repeat_until = <predicate>, repeat_max = N` — for the mistakes the
// runtime turns into a silently dropped op or a loop that never
// iterates. Warnings only, same convention as lintStackLoops.
//
//   - repeat_until without a positive integer literal repeat_max: the
//     chassis rejects the op at dispatch (dropped from the merge).
//   - repeat_max without repeat_until: ignored, the op runs once.
//   - repeat_until on a non-txco:// EXEC: rejected at dispatch (v1).
//   - a predicate that is TRUE when its path is missing (`== false`,
//     `!= true`): the loop exits after the first pass before the op has
//     written anything worth testing — the missing-path polarity trap.
//   - repeat_until plus an unconditional EMIT @goto back into its own
//     stage: two independent loops stacked, each with its own budget.
//   - repeat_budget: reserved, not yet supported.
func lintRepeatDirectives(ops []bundle.Op) []string {
	var warnings []string

	for _, op := range ops {
		r, perr := txcl.Resonator(op.Txcl)
		if perr != nil || r == nil {
			continue
		}
		where := fmt.Sprintf("%s (%s/%d/%s)", op.SourcePath, op.Stack, op.Scope, op.Name)
		maxVal, hasMax := r.With["repeat_max"]
		_, hasBudget := r.With["repeat_budget"]

		if r.RepeatUntil == nil {
			if hasMax {
				warnings = append(warnings, fmt.Sprintf(
					"lint: %s has repeat_max without repeat_until — it is ignored; the op runs once", where))
			}
			if hasBudget {
				warnings = append(warnings, fmt.Sprintf(
					"lint: %s sets repeat_budget, which is reserved and not yet supported; use WITH timeout to bound the loop", where))
			}
			continue
		}

		if !hasMax {
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s has repeat_until without repeat_max — the op is dropped at dispatch; add repeat_max = <passes>", where))
		} else if n, ok := literalInt(maxVal); !ok || n <= 0 {
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s repeat_max must be a positive integer literal (the per-op pass ceiling); the op is dropped at dispatch otherwise", where))
		}
		if !strings.HasPrefix(r.Exec, "txco://") {
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s uses repeat_until with EXEC %q — this version repeats txco:// ops only; the op is dropped at dispatch", where, r.Exec))
		}
		if leaf := repeatPolarityTrap(r.RepeatUntil, false); leaf != "" {
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s repeat_until compares %s — a missing path reads as false, so the loop exits after the first pass; compare `== true`, `!= \"\"`, or a value the op always writes", where, leaf))
		}
		if r.When == nil && !ruleHalts(r) && r.Emit != nil {
			self := stageRef{Stack: op.Stack, Scope: op.Scope}
			for _, ov := range r.Emit.Overrides {
				if !isGotoPath(ov.Path) {
					continue
				}
				lit, ok := literalString(ov.Value)
				if !ok {
					continue
				}
				if target, ok := resolveStageRef(lit, op.Stack); ok && target == self {
					warnings = append(warnings, fmt.Sprintf(
						"lint: %s combines repeat_until with an unconditional @goto back into its own stage — two loops stacked, each with its own budget; keep one", where))
				}
			}
		}
		if hasBudget {
			warnings = append(warnings, fmt.Sprintf(
				"lint: %s sets repeat_budget, which is reserved and not yet supported; use WITH timeout to bound the loop", where))
		}
	}

	return warnings
}

// repeatPolarityTrap walks a repeat_until expression and returns the
// spelling of the first leaf that is TRUE when its path is missing —
// `.x == false` / `.x != true`, or their negated forms under `!` — since
// WHEN coerces a missing path to the zero value. Empty when none.
func repeatPolarityTrap(e *resonator.WhenExpr, negated bool) string {
	if e == nil {
		return ""
	}
	switch {
	case e.HasLeaf:
		b, ok := e.Leaf.MatchValue.(bool)
		if !ok || e.Leaf.Branch == nil {
			return ""
		}
		eq := e.Leaf.MatchType == resonator.MatchType("eq")
		ne := e.Leaf.MatchType == resonator.MatchType("ne")
		// Missing path → false. `== false` and `!= true` hold; under a
		// `!` the other two do.
		trap := (eq && !b) || (ne && b)
		if negated {
			trap = (eq && b) || (ne && !b)
		}
		if !trap {
			return ""
		}
		op := "=="
		if ne {
			op = "!="
		}
		spelled := fmt.Sprintf("`%s %s %t`", e.Leaf.Branch.Path, op, b)
		if negated {
			spelled = "`!(" + strings.Trim(spelled, "`") + ")`"
		}
		return spelled
	case e.Not != nil:
		return repeatPolarityTrap(e.Not, !negated)
	}
	for i := range e.And {
		if s := repeatPolarityTrap(&e.And[i], negated); s != "" {
			return s
		}
	}
	for i := range e.Or {
		if s := repeatPolarityTrap(&e.Or[i], negated); s != "" {
			return s
		}
	}
	return ""
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

// literalInt extracts the underlying integer from an ast.Literal (the
// parser produces int64 for INT tokens).
func literalInt(v ast.Value) (int64, bool) {
	lit, ok := v.(ast.Literal)
	if !ok {
		return 0, false
	}
	n, ok := lit.V.(int64)
	return n, ok
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
