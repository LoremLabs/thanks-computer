package cli

import (
	"fmt"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/cli/bundle"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
)

// lintWhenZeroEquality warns about a WHEN leaf that holds when its path is
// missing, because a comparison coerces a missing path to the literal's
// zero value: `.x == false` and `.x == 0` are true for a path that was
// never written, not only for one written with that value. A rule meant
// to catch "the step ran and said no" then fires on every run where the
// step never ran, and in a scope beside the rule for "yes" the two race.
//
// Not warned, on purpose:
//   - `!= true` and the other `!=` forms: they say "anything but", which
//     includes absent, and are the documented idiom.
//   - a leaf under `!`: its polarity is inverted, so `!(.x == false)` is
//     the `!=` form in other clothes.
//   - a leaf whose path, or a prefix of it, is guarded by a non-negated
//     `exists` in the same conjunction (`exists ._r && ._r.ok == false`):
//     the author has said what they mean.
//   - `== ""`: the parser stores `== null` the same way, and `== null`
//     matching a missing path is documented and relied on.
//
// Warnings only, like the loop lints; they never flip the exit code.
func lintWhenZeroEquality(ops []bundle.Op) []string {
	var warnings []string
	for _, op := range ops {
		r, perr := txcl.Resonator(op.Txcl)
		if perr != nil || r == nil || r.When == nil {
			continue
		}
		var leaves []string
		if r.When.Expr != nil {
			leaves = zeroEqualityLeaves(r.When.Expr, false, nil)
		} else {
			// The legacy flat-AND shape: one conjunction of leaves.
			expr := resonator.WhenExpr{}
			for _, c := range r.When.Conditions {
				expr.And = append(expr.And, resonator.WhenExpr{Leaf: c, HasLeaf: true})
			}
			if len(expr.And) > 0 {
				leaves = zeroEqualityLeaves(&expr, false, nil)
			}
		}
		if len(leaves) == 0 {
			continue
		}
		where := fmt.Sprintf("%s (%s/%d/%s)", op.SourcePath, op.Stack, op.Scope, op.Name)
		warnings = append(warnings, fmt.Sprintf(
			"lint: %s WHEN compares %s — a missing path reads as the zero value, so this also fires when the path was never written; write `exists <path> && …` for \"written, and false\", or `!= true` for \"anything but true\"",
			where, strings.Join(leaves, ", ")))
	}
	return warnings
}

// zeroEqualityLeaves walks e and returns the spelling of each leaf that
// compares a path `== false` or `== 0` outside a `!`, except where an
// `exists` in the enclosing conjunction guards the path. present holds the
// paths such guards name.
func zeroEqualityLeaves(e *resonator.WhenExpr, negated bool, present []string) []string {
	if e == nil {
		return nil
	}
	switch {
	case e.HasLeaf:
		if negated || e.Leaf.Branch == nil || e.Leaf.MatchType != resonator.MatchType("eq") {
			return nil
		}
		path := e.Leaf.Branch.Path
		for _, p := range present {
			if path == p || strings.HasPrefix(path, p+".") {
				return nil
			}
		}
		switch v := e.Leaf.MatchValue.(type) {
		case bool:
			if !v {
				return []string{"`" + path + " == false`"}
			}
		case int64:
			if v == 0 {
				return []string{"`" + path + " == 0`"}
			}
		case float64:
			if v == 0 {
				return []string{"`" + path + " == 0`"}
			}
		}
		return nil
	case e.Not != nil:
		return zeroEqualityLeaves(e.Not, !negated, present)
	}
	var out []string
	if len(e.And) > 0 {
		// A non-negated `exists` among the siblings guards its path for
		// the whole conjunction, whichever side of `&&` it is on.
		guards := append([]string(nil), present...)
		if !negated {
			for i := range e.And {
				if l := e.And[i]; l.HasLeaf && l.Leaf.Branch != nil && l.Leaf.MatchType == resonator.MatchType("exists") {
					guards = append(guards, l.Leaf.Branch.Path)
				}
			}
		}
		for i := range e.And {
			out = append(out, zeroEqualityLeaves(&e.And[i], negated, guards)...)
		}
	}
	for i := range e.Or {
		out = append(out, zeroEqualityLeaves(&e.Or[i], negated, present)...)
	}
	return out
}
