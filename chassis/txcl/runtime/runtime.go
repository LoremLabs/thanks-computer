// Package runtime is the txcl value-resolution layer. It owns the
// single function — Resolve — that walks an ast.Value against an
// Env and returns a concrete Go value.
//
// Every consumer of a txcl Value (the processor's SET / EMIT / WITH
// paths; eventually the resonator's WHEN evaluator) calls Resolve
// rather than reaching inside Value nodes directly. This keeps the
// dispatch logic in one place and means new value shapes
// (FunctionCall when PR 3 wires it in, future node types after) add
// to one switch instead of being scattered.
//
// See internal docs/todo-txcl-expressions.md and
// internal docs/todo-txcl-expressions-implementation.md for the broader
// plan.
package runtime

import (
	"fmt"

	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/txcl/funcs"
)

// Resolve evaluates a Value against env. Single entry point shared
// by every caller that needs a runtime value from a parsed AST node.
//
// Semantics:
//   - nil Value returns (nil, nil). Useful for optional fields
//     (SelectAssignment.Default when HasDefault is false).
//   - Literal returns its wrapped V directly.
//   - PathRef returns env.Get(path)'s value. A missing path returns
//     (nil, nil) — absence is not an error; callers can write nil
//     into the envelope or compare against null in WHEN clauses.
//   - FunctionCall resolves each Arg (recursively) then dispatches
//     through chassis/txcl/funcs. The registered function returns
//     either a value or an error; the error propagates as a strict
//     halt.
//
// Callers must treat a non-nil error as a halt signal for the rule
// being evaluated (strict-by-default semantics — see design doc §5).
// Silent skipping is a footgun: a value that "couldn't be computed"
// is not the same as a value that "is empty."
func Resolve(v ast.Value, env Env) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch n := v.(type) {
	case ast.Literal:
		return n.V, nil
	case ast.PathRef:
		val, _ := env.Get(n.Path) // missing path → (nil, nil); not an error
		return val, nil
	case ast.FunctionCall:
		args := make([]any, len(n.Args))
		for i, a := range n.Args {
			arg, err := Resolve(a, env) // recursive — nested calls supported
			if err != nil {
				return nil, err
			}
			args[i] = arg
		}
		return funcs.Call(n.Name, args)
	}
	return nil, fmt.Errorf("unknown value node type %T", v)
}
