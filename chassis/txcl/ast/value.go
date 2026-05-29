// Package ast defines the runtime value AST nodes shared across the
// txcl parser, runtime evaluator, and processor. The package is a
// leaf — it depends on nothing — so downstream packages (runtime,
// resonator, processor) can all reference it without creating
// import cycles when the runtime evaluator (chassis/txcl/runtime)
// is introduced.
//
// Value is a discriminated sum:
//
//   - Literal{V}            — a parsed scalar or array literal.
//   - PathRef{Path}         — an `@x.y.z` envelope reference. Reserved
//                             for future use; the parser does not
//                             produce this node yet (paths on the RHS
//                             of SET/EMIT/WITH are not supported in
//                             the current grammar).
//   - FunctionCall{Name,Args} — a `&fn(args...)` runtime call.
//                               Reserved; parser does not produce this
//                               yet either.
//
// See internal docs/todo-txcl-expressions.md for the broader plan; this
// package lands the AST shape so the runtime evaluator can be added
// next without a churn-heavy retrofit.
package ast

// Value is one node in the value-expression tree. The closed set of
// implementations is Literal, PathRef, and FunctionCall.
type Value interface {
	isValue()
}

// Literal wraps a parsed scalar (string, int64, float64, bool) or
// array literal ([]interface{}). The underlying type is whatever
// the parser produced — this struct only carries it, it does not
// constrain it.
type Literal struct {
	V interface{}
}

// PathRef is an envelope path reference (`@x.y.z`). Resolved by the
// runtime evaluator against the current envelope.
type PathRef struct {
	Path string
}

// FunctionCall is a `&fn(args...)` runtime call. Resolved by the
// runtime evaluator through the function registry. Args may include
// nested FunctionCall nodes (resolution is recursive).
type FunctionCall struct {
	Name string
	Args []Value
}

func (Literal) isValue()      {}
func (PathRef) isValue()      {}
func (FunctionCall) isValue() {}

// LiteralOrNil extracts the wrapped value from an ast.Literal,
// returning nil for any other Value type (including a nil Value).
// Convenience for transitional code that pre-dates the runtime
// evaluator — every call site that uses this should ultimately
// migrate to runtime.Resolve, which handles all three node types.
func LiteralOrNil(v Value) interface{} {
	if lit, ok := v.(Literal); ok {
		return lit.V
	}
	return nil
}
