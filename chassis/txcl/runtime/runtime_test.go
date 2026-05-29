package runtime

import (
	"reflect"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

// --- Env implementations ---------------------------------------

func TestJSONEnv_GetHit(t *testing.T) {
	env := JSONEnv(`{"a":{"b":"hello"},"n":42,"arr":[1,2,3]}`)
	cases := []struct {
		path string
		want any
	}{
		{"a.b", "hello"},
		{"n", float64(42)}, // gjson returns numbers as float64
		{"arr.0", float64(1)},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			got, ok := env.Get(tc.path)
			if !ok {
				t.Fatalf("expected hit, got miss")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v (%T), want %v (%T)", got, got, tc.want, tc.want)
			}
		})
	}
}

func TestJSONEnv_GetMiss(t *testing.T) {
	env := JSONEnv(`{"a":"x"}`)
	got, ok := env.Get("missing.path")
	if ok {
		t.Fatalf("expected miss, got hit: %v", got)
	}
	if got != nil {
		t.Fatalf("expected nil on miss, got %v", got)
	}
}

func TestMapEnv(t *testing.T) {
	env := MapEnv{"a": "hello", "n": int64(42)}
	if v, ok := env.Get("a"); !ok || v != "hello" {
		t.Fatalf("a: got (%v, %v)", v, ok)
	}
	if v, ok := env.Get("n"); !ok || v != int64(42) {
		t.Fatalf("n: got (%v, %v)", v, ok)
	}
	if v, ok := env.Get("missing"); ok || v != nil {
		t.Fatalf("missing: got (%v, %v); want (nil, false)", v, ok)
	}
}

// --- Resolve ---------------------------------------------------

func TestResolve_NilValue(t *testing.T) {
	got, err := Resolve(nil, MapEnv{})
	if err != nil {
		t.Fatalf("nil Value: got err %v", err)
	}
	if got != nil {
		t.Fatalf("nil Value: got %v, want nil", got)
	}
}

func TestResolve_Literal(t *testing.T) {
	cases := []struct {
		name string
		v    ast.Value
		want any
	}{
		{"string", ast.Literal{V: "hello"}, "hello"},
		{"int64", ast.Literal{V: int64(42)}, int64(42)},
		{"float64", ast.Literal{V: float64(3.14)}, float64(3.14)},
		{"bool", ast.Literal{V: true}, true},
		{"nil inner", ast.Literal{V: nil}, nil},
		{"array", ast.Literal{V: []interface{}{int64(1), "x"}}, []interface{}{int64(1), "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.v, MapEnv{})
			if err != nil {
				t.Fatalf("got err %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolve_PathRefHit(t *testing.T) {
	env := JSONEnv(`{"x":{"y":"found"}}`)
	got, err := Resolve(ast.PathRef{Path: "x.y"}, env)
	if err != nil {
		t.Fatalf("got err %v", err)
	}
	if got != "found" {
		t.Fatalf("got %v, want 'found'", got)
	}
}

func TestResolve_PathRefMiss(t *testing.T) {
	// Missing path is NOT an error — Resolve returns (nil, nil) so
	// callers can write nil or compare against null in WHEN.
	env := JSONEnv(`{"x":"present"}`)
	got, err := Resolve(ast.PathRef{Path: "nope.gone"}, env)
	if err != nil {
		t.Fatalf("missing path should not error, got %v", err)
	}
	if got != nil {
		t.Fatalf("missing path should resolve to nil, got %v", got)
	}
}

func TestResolve_FunctionCall_KnownReturnsValue(t *testing.T) {
	// &uuid() is wired through chassis/txcl/funcs as of PR 3 and
	// returns a UUID v7 string. Other behavior — argument
	// resolution, recursion, error propagation — is covered below.
	v := ast.FunctionCall{Name: "uuid"}
	got, err := Resolve(v, MapEnv{})
	if err != nil {
		t.Fatalf("expected success, got err %v", err)
	}
	s, ok := got.(string)
	if !ok || s == "" {
		t.Fatalf("expected non-empty uuid string, got %T %v", got, got)
	}
	if len(s) != 36 {
		t.Fatalf("expected 36-char uuid, got %q (len %d)", s, len(s))
	}
}

func TestResolve_FunctionCall_UnknownReturnsError(t *testing.T) {
	// Functions not registered with funcs.Call fail loudly. The
	// error message names the missing function so authors can
	// see exactly what's misspelled.
	v := ast.FunctionCall{Name: "not-a-real-fn"}
	got, err := Resolve(v, MapEnv{})
	if err == nil {
		t.Fatalf("expected error, got nil (val %v)", got)
	}
	if !strings.Contains(err.Error(), "&not-a-real-fn") {
		t.Errorf("error should name the missing function, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil value on error, got %v", got)
	}
}

func TestResolve_FunctionCall_ArgsResolvedRecursively(t *testing.T) {
	// FunctionCall args go through Resolve before the dispatcher
	// is called — so nested function calls / path refs / literals
	// all work as args. Verified here with `&now(<literal>)` where
	// the literal is the format selector. (Nested function-call
	// recursion is the same code path; this test pins it.)
	v := ast.FunctionCall{
		Name: "now",
		Args: []ast.Value{ast.Literal{V: "rfc3339"}},
	}
	got, err := Resolve(v, MapEnv{})
	if err != nil {
		t.Fatalf("expected success, got err %v", err)
	}
	s, ok := got.(string)
	if !ok {
		t.Fatalf("expected string, got %T", got)
	}
	// Loose shape check — full format validation is in funcs/.
	if len(s) < 19 || !strings.Contains(s, "T") {
		t.Errorf("expected RFC 3339-ish, got %q", s)
	}
}

func TestResolve_FunctionCall_ArgResolveErrorPropagates(t *testing.T) {
	// If an arg's resolution errors (e.g. nested unknown
	// function), the dispatcher never runs and the error bubbles
	// up unchanged.
	v := ast.FunctionCall{
		Name: "now",
		Args: []ast.Value{ast.FunctionCall{Name: "bogus-inner"}},
	}
	got, err := Resolve(v, MapEnv{})
	if err == nil {
		t.Fatalf("expected error from nested call, got val %v", got)
	}
	if !strings.Contains(err.Error(), "&bogus-inner") {
		t.Errorf("error should mention nested fn, got %v", err)
	}
}

// Note: the "unknown Value type" branch in Resolve is defensive
// code for a case the type system prevents — ast.Value uses an
// unexported isValue() method, sealing the interface to the three
// concrete types in the ast package. New node types would have to
// be added in ast/, at which point Resolve must be updated. Until
// then there is no way to construct an external Value to test the
// default branch, and there is no test for it.
