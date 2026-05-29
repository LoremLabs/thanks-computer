package ast

import (
	"reflect"
	"testing"
)

func TestValueTypesSatisfyInterface(t *testing.T) {
	// Each concrete type must satisfy the Value interface. A
	// compile-time blank assignment would catch this too, but a
	// runtime type-switch confirms the dispatch shape callers will
	// use against Resolve.
	cases := []struct {
		name string
		v    Value
	}{
		{"Literal", Literal{V: "x"}},
		{"PathRef", PathRef{Path: "x.y"}},
		{"FunctionCall", FunctionCall{Name: "uuid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			switch tc.v.(type) {
			case Literal, PathRef, FunctionCall:
				// ok
			default:
				t.Fatalf("unexpected type %T", tc.v)
			}
		})
	}
}

func TestLiteralOrNil(t *testing.T) {
	cases := []struct {
		name string
		v    Value
		want interface{}
	}{
		{"Literal string", Literal{V: "hello"}, "hello"},
		{"Literal int64", Literal{V: int64(42)}, int64(42)},
		{"Literal float64", Literal{V: float64(3.14)}, float64(3.14)},
		{"Literal bool", Literal{V: true}, true},
		{"Literal nil inner", Literal{V: nil}, nil},
		{"Literal array", Literal{V: []interface{}{int64(1), "x"}}, []interface{}{int64(1), "x"}},
		{"PathRef returns nil", PathRef{Path: "a.b"}, nil},
		{"FunctionCall returns nil", FunctionCall{Name: "uuid"}, nil},
		{"nil Value returns nil", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LiteralOrNil(tc.v)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("LiteralOrNil(%v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestFunctionCallNestedArgs(t *testing.T) {
	// Confirm the recursive AST shape compiles and walks correctly.
	// Mirrors `&json(&b64decode("aGVsbG8="))`.
	fc := FunctionCall{
		Name: "json",
		Args: []Value{
			FunctionCall{
				Name: "b64decode",
				Args: []Value{Literal{V: "aGVsbG8="}},
			},
		},
	}
	if fc.Name != "json" {
		t.Fatalf("outer name: got %q", fc.Name)
	}
	if len(fc.Args) != 1 {
		t.Fatalf("outer args: got %d", len(fc.Args))
	}
	inner, ok := fc.Args[0].(FunctionCall)
	if !ok {
		t.Fatalf("inner arg type: got %T", fc.Args[0])
	}
	if inner.Name != "b64decode" {
		t.Fatalf("inner name: got %q", inner.Name)
	}
	if len(inner.Args) != 1 {
		t.Fatalf("inner args: got %d", len(inner.Args))
	}
	leaf, ok := inner.Args[0].(Literal)
	if !ok {
		t.Fatalf("leaf type: got %T", inner.Args[0])
	}
	if leaf.V != "aGVsbG8=" {
		t.Fatalf("leaf value: got %v", leaf.V)
	}
}
