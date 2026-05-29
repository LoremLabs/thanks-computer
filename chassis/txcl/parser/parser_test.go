package parser_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/parser"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

func TestParserNoInput(t *testing.T) {
	input := ``
	l := lexer.New(input)
	p := parser.New(l)
	p.ParseEvent()

	// no parser errors
	test.Equals(t, 0, len(p.Errors()))
}

// starWhen builds the canonical "WHEN *" / empty-WHEN tree.
func starWhen() *resonator.When {
	return &resonator.When{Expr: &resonator.WhenExpr{Star: true}}
}

// leafWhen wraps a single Condition into a leaf WhenExpr. Used so the
// table-driven tests don't have to spell out WhenExpr{Leaf: ..., HasLeaf: true}
// for every single-leaf WHEN.
func leafWhen(c resonator.Condition) *resonator.When {
	return &resonator.When{Expr: &resonator.WhenExpr{Leaf: c, HasLeaf: true}}
}

func TestParserSimple(t *testing.T) {

	tests := []struct {
		input   string
		res     *resonator.Resonator
		errs    int
		errmsgs []string
	}{
		{
			"WHEN *",
			&resonator.Resonator{When: starWhen()},
			0,
			[]string{},
		},
		{
			"WHEN .a =~ /^testing/",
			&resonator.Resonator{When: leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: ".a"}, MatchType: resonator.MatchType("=~"), MatchValue: "^testing"})},
			0,
			[]string{},
		},
		{
			`WHEN * SELECT @x AS .y`,
			&resonator.Resonator{
				When: starWhen(),
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
			},
			0,
			[]string{},
		},
		{
			`WHEN ._txc.src == "cron"

SELECT @web.req.url.query.q.0 AS .q DEFAULT "fallback"
`,
			&resonator.Resonator{
				When: leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: "._txc.src"}, MatchType: resonator.MatchType("eq"), MatchValue: "cron"}),
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.web.req.url.query.q.0", To: ".q", Default: ast.Literal{V: "fallback"}, HasDefault: true},
				}},
			},
			0,
			[]string{},
		},
		{
			`WHEN * SELECT @a AS .x, @b AS .y DEFAULT 42 PRIORITY 2`,
			&resonator.Resonator{
				When: starWhen(),
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.a", To: ".x"},
					{From: "._txc.b", To: ".y", Default: ast.Literal{V: int64(42)}, HasDefault: true},
				}},
				Priority: 2,
			},
			0,
			[]string{},
		},
		{
			`WHEN * SELECT @x AS .y PRIORITY moo`,
			&resonator.Resonator{
				When: starWhen(),
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
				Priority: 0,
			},
			1,
			[]string{`could not parse priority "moo" as integer`},
		},
		{
			"SET .a = 5 PRIORITY 11",
			&resonator.Resonator{
				SetPre:   &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".a", Value: ast.Literal{V: int64(5)}}}},
				Priority: 11,
			},
			0,
			[]string{},
		},
		{
			"SET .a = 5.6 PRIORITY 11",
			&resonator.Resonator{
				SetPre:   &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".a", Value: ast.Literal{V: float64(5.6)}}}},
				Priority: 11,
			},
			0,
			[]string{},
		},
		// --- array-literal RHS for SET (and WITH) ---
		{
			`SET .words = ["cruel"]`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".words", Value: ast.Literal{V: []interface{}{"cruel"}}},
				}},
			},
			0,
			[]string{},
		},
		{
			`SET .nums = [1, 2, 3.5]`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".nums", Value: ast.Literal{V: []interface{}{int64(1), int64(2), float64(3.5)}}},
				}},
			},
			0,
			[]string{},
		},
		{
			`SET .empty = []`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".empty", Value: ast.Literal{V: []interface{}{}}},
				}},
			},
			0,
			[]string{},
		},
		{
			`SET .mixed = [true, "x", 7]`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".mixed", Value: ast.Literal{V: []interface{}{true, "x", int64(7)}}},
				}},
			},
			0,
			[]string{},
		},
		{
			`SET .nested = [[1, 2], [3]]`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".nested", Value: ast.Literal{V: []interface{}{
						[]interface{}{int64(1), int64(2)},
						[]interface{}{int64(3)},
					}}},
				}},
			},
			0,
			[]string{},
		},
		{
			`SET .trailing = [1, 2,]`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".trailing", Value: ast.Literal{V: []interface{}{int64(1), int64(2)}}},
				}},
			},
			0,
			[]string{},
		},
		{
			// Multiple branches in one SET, mixing scalar and array RHS.
			`SET .a = 5, .b = ["x", "y"]`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".a", Value: ast.Literal{V: int64(5)}},
					{Path: ".b", Value: ast.Literal{V: []interface{}{"x", "y"}}},
				}},
			},
			0,
			[]string{},
		},
		{
			// null isn't a scalar the SET gate accepts today, so it's
			// also rejected inside arrays. If we ever lift the gate to
			// allow null, this case becomes an accept.
			`SET .x = [null]`,
			&resonator.Resonator{},
			1,
			[]string{`Expecting bool, int, float, string, or array, received null`},
		},
		{
			// Sanity: an unterminated array hits the comma/bracket
			// check in parseArrayLiteral.
			`SET .x = [1 2]`,
			&resonator.Resonator{},
			1,
			[]string{`Expecting ',' or ']', received 2`},
		},
		// --- EMIT clause ---
		{
			// EMIT alone — synthetic emitter; no EXEC needed.
			`EMIT .words = ["cruel"]`,
			&resonator.Resonator{
				Emit: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".words", Value: ast.Literal{V: []interface{}{"cruel"}}},
				}},
			},
			0,
			[]string{},
		},
		{
			// EMIT alongside a real EXEC — overlay onto the
			// dispatched response.
			`EXEC "http://api/foo" EMIT .extra = "tag"`,
			&resonator.Resonator{
				Exec: "http://api/foo",
				Emit: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".extra", Value: ast.Literal{V: "tag"}},
				}},
			},
			0,
			[]string{},
		},
		{
			// Multiple branches + heterogeneous values.
			`EMIT .a = 1, .b = "x", .c = [true, 2]`,
			&resonator.Resonator{
				Emit: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".a", Value: ast.Literal{V: int64(1)}},
					{Path: ".b", Value: ast.Literal{V: "x"}},
					{Path: ".c", Value: ast.Literal{V: []interface{}{true, int64(2)}}},
				}},
			},
			0,
			[]string{},
		},
		{
			// WHEN gate composed with EMIT — typical pattern.
			`WHEN ._txc.src == "http" EMIT .tag = "cruel"`,
			&resonator.Resonator{
				When: leafWhen(resonator.Condition{
					Branch:     &resonator.Branch{Path: "._txc.src"},
					MatchType:  resonator.MatchType("eq"),
					MatchValue: "http",
				}),
				Emit: &resonator.Set{Overrides: []resonator.BranchValue{
					{Path: ".tag", Value: ast.Literal{V: "cruel"}},
				}},
			},
			0,
			[]string{},
		},
		{
			`WHEN * SELECT @x AS .y PRIORITY 2 EXEC "hello-world"`,
			&resonator.Resonator{
				When: starWhen(),
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
				Priority: 2,
				Exec:     "hello-world",
			},
			0,
			[]string{},
		},
		{
			`WHEN * SELECT @x AS .y PRIORITY "moo" EXEC "hello-world"`,
			&resonator.Resonator{
				When: starWhen(),
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
				Priority: 0,
				Exec:     "hello-world",
			},
			1,
			[]string{`could not parse priority "moo" as integer`},
		},
		{
			`WHEN .a == !`,
			&resonator.Resonator{},
			1,
			[]string{`Expecting bool, int, float, string, null, or regex, received !`},
		},
		{
			`WHEN .a == null`,
			&resonator.Resonator{
				When: leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: ".a"}, MatchType: resonator.MatchType("eq"), MatchValue: ""}),
			},
			0,
			[]string{},
		},
		{
			`WHEN .a !~ /thing/`,
			&resonator.Resonator{
				When: leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: ".a"}, MatchType: resonator.MatchType("!~"), MatchValue: "thing"}),
			},
			0,
			[]string{},
		},
		{
			`WHEN .a != false`,
			&resonator.Resonator{
				When: leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: ".a"}, MatchType: resonator.MatchType("ne"), MatchValue: false}),
			},
			0,
			[]string{},
		},
		{
			`EXEC`,
			&resonator.Resonator{},
			1,
			[]string{`exec missing execname`},
		},
		{
			`WITH 7`,
			&resonator.Resonator{},
			1,
			[]string{`Expecting with variable name, received 7`},
		},
		{
			`WITH a 7`,
			&resonator.Resonator{},
			1,
			[]string{`expected token to be =, got INT instead`},
		},
		{
			`WITH a = SELECT`,
			&resonator.Resonator{},
			1,
			[]string{`Expecting bool, int, float, string, or array, received SELECT`},
		},
		{
			`SET a = 7`,
			&resonator.Resonator{},
			1,
			[]string{`Expecting .branch, received a`},
		},
		{
			`SET .a 7`,
			&resonator.Resonator{},
			1,
			[]string{`expected token to be =, got INT instead`},
		},
		{
			`SET .a = SELECT`,
			&resonator.Resonator{},
			1,
			[]string{`Expecting bool, int, float, string, or array, received SELECT`},
		},
		{
			`SELECT @x AS .y SET a`,
			&resonator.Resonator{
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
			},
			1,
			[]string{`Expecting .Branch, received a`},
		},
		{
			`SELECT @x AS .y SET .a SELECT`,
			&resonator.Resonator{
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
			},
			1,
			[]string{`expected token to be =, got SELECT instead`},
		},
		{
			`SELECT @x AS .y SET .a = SELECT`,
			&resonator.Resonator{
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
			},
			1,
			[]string{`Expecting bool, int, float, string, or array, received SELECT`},
		},
		{
			`SELECT`,
			&resonator.Resonator{},
			1,
			[]string{`SELECT expected a source path (e.g. ` + "`.foo`" + ` or ` + "`@web.req.…`)"},
		},
		{
			`SELECT @x`,
			&resonator.Resonator{},
			1,
			[]string{"SELECT expected `AS <dest>` after the source path"},
		},
		{
			`SELECT @x AS`,
			&resonator.Resonator{},
			1,
			[]string{"SELECT expected a destination path after `AS`"},
		},
		{
			`WHEN >=`,
			&resonator.Resonator{},
			1,
			[]string{`WHEN expected branch or '(', got '>='`},
		},
		{
			`WHEN .a != null`,
			&resonator.Resonator{
				When: leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: ".a"}, MatchType: resonator.MatchType("ne"), MatchValue: ""}),
			},
			0,
			[]string{},
		},
		{
			`WHEN * SET .a = "thing" SELECT @x AS .y PRIORITY 2 EXEC "hello-world"`,
			&resonator.Resonator{
				SetPre: &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".a", Value: ast.Literal{V: "thing"}}}},
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
				When:     starWhen(),
				Priority: 2,
				Exec:     "hello-world",
			},
			0,
			[]string{},
		},
		{
			`WHEN * SET .a = "thing" SELECT @x AS .y SET .b = "foo", .c = "bar" PRIORITY 2 EXEC "hello-world"`,
			&resonator.Resonator{
				SetPre:  &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".a", Value: ast.Literal{V: "thing"}}}},
				SetPost: &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".b", Value: ast.Literal{V: "foo"}}, {Path: ".c", Value: ast.Literal{V: "bar"}}}},
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
				When:     starWhen(),
				Priority: 2,
				Exec:     "hello-world",
			},
			0,
			[]string{},
		},
		{
			`WHEN ._src == "web" SET .a = "thing" SELECT @x AS .y SET .b = "foo", .c = "bar" WITH timeout = 1000 tf = false, stringy = "value" PRIORITY 2 EXEC "hello-world"`,
			&resonator.Resonator{
				SetPre:  &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".a", Value: ast.Literal{V: "thing"}}}},
				SetPost: &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".b", Value: ast.Literal{V: "foo"}}, {Path: ".c", Value: ast.Literal{V: "bar"}}}},
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
				When:     leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: "._src"}, MatchType: resonator.MatchType("eq"), MatchValue: "web"}),
				With:     map[string]ast.Value{"timeout": ast.Literal{V: int64(1000)}, "tf": ast.Literal{V: false}, "stringy": ast.Literal{V: "value"}},
				Priority: 2,
				Exec:     "hello-world",
			},
			0,
			[]string{},
		},
		{
			`WHEN ._src != 123.4 SET .a = 5.55 SELECT @x AS .y SET .b = "foo", .c = 1.1 WITH timeout = 1000 tf = false, stringy = "value", floaty = 9.9 PRIORITY 2 EXEC "hello-world"`,
			&resonator.Resonator{
				SetPre:  &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".a", Value: ast.Literal{V: float64(5.55)}}}},
				SetPost: &resonator.Set{Overrides: []resonator.BranchValue{{Path: ".b", Value: ast.Literal{V: "foo"}}, {Path: ".c", Value: ast.Literal{V: float64(1.1)}}}},
				Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
					{From: "._txc.x", To: ".y"},
				}},
				When:     leafWhen(resonator.Condition{Branch: &resonator.Branch{Path: "._src"}, MatchType: resonator.MatchType("ne"), MatchValue: float64(123.4)}),
				With:     map[string]ast.Value{"timeout": ast.Literal{V: int64(1000)}, "tf": ast.Literal{V: false}, "stringy": ast.Literal{V: "value"}, "floaty": ast.Literal{V: float64(9.9)}},
				Priority: 2,
				Exec:     "hello-world",
			},
			0,
			[]string{},
		},
	}

	for _, tt := range tests {
		l := lexer.New(tt.input)
		p := parser.New(l)
		res := p.ParseEvent()

		// parser errors
		if tt.errs != 0 {
			test.Equals(t, tt.errmsgs, p.Errors())
		}
		if tt.errs != len(p.Errors()) {
			fmt.Printf("errs %s\n", p.Errors())
		}
		test.Equals(t, tt.errs, len(p.Errors()))

		test.Equals(t, tt.res, res)
	}
}

// TestParserSugar locks in the lexer-level sugars: `@foo` rewrites to
// `._txc.foo` paths in WHEN/SET, and `b64"..."` rewrites to a base64-
// encoded STRING. The parser doesn't know either form exists — it just
// sees normal BRANCH and STRING tokens — so the assertion is "sugared
// input parses to the same *Resonator as the desugared form."
func TestParserSugar(t *testing.T) {
	sugared := `WHEN @web.req.url.path != "/" SET @halt = true, @web.res.status = 404, @web.res.body = b64"not found" EXEC "txco://noop"`
	desugared := `WHEN ._txc.web.req.url.path != "/" SET ._txc.halt = true, ._txc.web.res.status = 404, ._txc.web.res.body = "bm90IGZvdW5k" EXEC "txco://noop"`

	parse := func(input string) (*resonator.Resonator, []string) {
		l := lexer.New(input)
		p := parser.New(l)
		return p.ParseEvent(), p.Errors()
	}

	sugRes, sugErrs := parse(sugared)
	test.Equals(t, []string{}, sugErrs)

	desRes, desErrs := parse(desugared)
	test.Equals(t, []string{}, desErrs)

	// Both forms must produce identical resonators.
	test.Equals(t, desRes, sugRes)

	// Also assert the concrete shape so a regression in either form
	// stands out independently of the round-trip.
	want := &resonator.Resonator{
		When: leafWhen(resonator.Condition{
			Branch:     &resonator.Branch{Path: "._txc.web.req.url.path"},
			MatchType:  resonator.MatchType("ne"),
			MatchValue: "/",
		}),
		SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
			{Path: "._txc.halt", Value: ast.Literal{V: true}},
			{Path: "._txc.web.res.status", Value: ast.Literal{V: int64(404)}},
			{Path: "._txc.web.res.body", Value: ast.Literal{V: "bm90IGZvdW5k"}},
		}},
		Exec: "txco://noop",
	}
	test.Equals(t, want, sugRes)
}

// TestParserFunctionCall covers the `&fn(args)` grammar — both the
// happy-path shapes (zero-arg, one-arg, multi-arg, nested) and the
// "bad syntax surfaces a useful error" path. Function calls on the
// RHS of SET (and EMIT / WITH / SELECT-DEFAULT) parse to
// ast.FunctionCall nodes that the runtime evaluator dispatches
// through the funcs registry.
func TestParserFunctionCall(t *testing.T) {
	parse := func(input string) (*resonator.Resonator, []string) {
		l := lexer.New(input)
		p := parser.New(l)
		return p.ParseEvent(), p.Errors()
	}

	t.Run("zero-arg call", func(t *testing.T) {
		res, errs := parse(`SET .x = &uuid()`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".x", Value: ast.FunctionCall{Name: "uuid", Args: []ast.Value{}}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("one-arg call with literal", func(t *testing.T) {
		res, errs := parse(`SET .x = &now("rfc3339")`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".x", Value: ast.FunctionCall{
					Name: "now",
					Args: []ast.Value{ast.Literal{V: "rfc3339"}},
				}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("multi-arg call mixes literals", func(t *testing.T) {
		res, errs := parse(`SET .x = &concat("a", "b", "c")`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".x", Value: ast.FunctionCall{
					Name: "concat",
					Args: []ast.Value{
						ast.Literal{V: "a"},
						ast.Literal{V: "b"},
						ast.Literal{V: "c"},
					},
				}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("nested calls", func(t *testing.T) {
		res, errs := parse(`SET .x = &json(&b64decode("aGVsbG8="))`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".x", Value: ast.FunctionCall{
					Name: "json",
					Args: []ast.Value{
						ast.FunctionCall{
							Name: "b64decode",
							Args: []ast.Value{ast.Literal{V: "aGVsbG8="}},
						},
					},
				}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("function call in EMIT", func(t *testing.T) {
		res, errs := parse(`EMIT .id = &uuid()`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			Emit: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".id", Value: ast.FunctionCall{Name: "uuid", Args: []ast.Value{}}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("function call in WITH", func(t *testing.T) {
		res, errs := parse(`WITH ts = &now() EXEC "txco://noop"`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			With: map[string]ast.Value{
				"ts": ast.FunctionCall{Name: "now", Args: []ast.Value{}},
			},
			Exec: "txco://noop",
		}
		test.Equals(t, want, res)
	})

	t.Run("function call in SELECT DEFAULT", func(t *testing.T) {
		res, errs := parse(`SELECT .src AS .id DEFAULT &uuid()`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			Select: &resonator.Select{Assignments: []resonator.SelectAssignment{
				{From: ".src", To: ".id",
					Default: ast.FunctionCall{Name: "uuid", Args: []ast.Value{}}, HasDefault: true},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("path-on-RHS as standalone value", func(t *testing.T) {
		// SET .x = .y copies the value at path .y into .x via a
		// PathRef RHS. This is new in PR 3 — pre-PR-3 SET RHS was
		// literal-only, so `.y` errored. Test pins the new shape.
		res, errs := parse(`SET .x = .y`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".x", Value: ast.PathRef{Path: "y"}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("path-on-RHS with @ sugar", func(t *testing.T) {
		// @rpc desugars to ._txc.rpc; PathRef carries the
		// post-trim path.
		res, errs := parse(`SET .out = @rpc.method`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".out", Value: ast.PathRef{Path: "_txc.rpc.method"}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("path-on-RHS as function argument", func(t *testing.T) {
		// The motivating case: function calls take @path args
		// alongside literals. Parses to FunctionCall with mixed
		// arg types.
		res, errs := parse(`SET .x = &get(@rpc, "params.name")`)
		test.Equals(t, []string{}, errs)
		want := &resonator.Resonator{
			SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
				{Path: ".x", Value: ast.FunctionCall{
					Name: "get",
					Args: []ast.Value{
						ast.PathRef{Path: "_txc.rpc"},
						ast.Literal{V: "params.name"},
					},
				}},
			}},
		}
		test.Equals(t, want, res)
	})

	t.Run("bad syntax: missing parenthesis", func(t *testing.T) {
		// `&uuid` with no `(` is a parse error — the only place
		// AMP_IDENT is legal is as the head of a function call.
		_, errs := parse(`SET .x = &uuid`)
		if len(errs) == 0 {
			t.Fatalf("expected at least one parse error")
		}
		// Loose match on the message — it should mention either the
		// expected '(' or call out &uuid specifically.
		joined := strings.Join(errs, " ; ")
		if !strings.Contains(joined, "(") && !strings.Contains(joined, "&uuid") {
			t.Errorf("error message should mention '(' or '&uuid', got: %s", joined)
		}
	})
}

// TestParserHyphenatedHeaderPath is the motivating case: setting an
// HTTP response header whose key has a hyphen, via the `@` sugar. It
// must parse to the gjson/sjson path the web inlet reads
// (_txc.web.res.headers.content-type.0). Also covers the quoted-segment
// form and a key with a literal dot (escaped).
func TestParserHyphenatedHeaderPath(t *testing.T) {
	parse := func(input string) (*resonator.Resonator, []string) {
		l := lexer.New(input)
		p := parser.New(l)
		return p.ParseEvent(), p.Errors()
	}

	// Bare hyphen via @ sugar — the real-world line.
	res, errs := parse(`SET @web.res.headers.content-type.0 = "text/plain" EXEC "txco://noop"`)
	test.Equals(t, []string{}, errs)
	test.Equals(t, &resonator.Resonator{
		SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
			{Path: "._txc.web.res.headers.content-type.0", Value: ast.Literal{V: "text/plain"}},
		}},
		Exec: "txco://noop",
	}, res)

	// Quoted-segment form, dotted `.`-led path — equivalent path.
	res2, errs2 := parse(`SET ._txc.web.res.headers."content-type".0 = "text/plain"`)
	test.Equals(t, []string{}, errs2)
	test.Equals(t, &resonator.Resonator{
		SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
			{Path: "._txc.web.res.headers.content-type.0", Value: ast.Literal{V: "text/plain"}},
		}},
	}, res2)

	// Whole-header array literal instead of indexed .0 — equivalent
	// runtime shape (content-type becomes ["text/plain"]).
	resArr, errsArr := parse(`SET @web.res.headers.content-type = ["text/plain"] EXEC "txco://noop"`)
	test.Equals(t, []string{}, errsArr)
	test.Equals(t, &resonator.Resonator{
		SetPre: &resonator.Set{Overrides: []resonator.BranchValue{
			{Path: "._txc.web.res.headers.content-type", Value: ast.Literal{V: []interface{}{"text/plain"}}},
		}},
		Exec: "txco://noop",
	}, resArr)

	// WHEN gate on a hyphenated request header.
	res3, errs3 := parse(`WHEN @web.req.headers.x-forwarded-proto.0 == "https" EXEC "txco://noop"`)
	test.Equals(t, []string{}, errs3)
	test.Equals(t, &resonator.Resonator{
		When: leafWhen(resonator.Condition{
			Branch:     &resonator.Branch{Path: "._txc.web.req.headers.x-forwarded-proto.0"},
			MatchType:  resonator.MatchType("eq"),
			MatchValue: "https",
		}),
		Exec: "txco://noop",
	}, res3)
}

// TestParserWhenBooleanExpressions covers the grammar v2 surface:
// `&&`, `||`, `!`, parens, precedence, comma-as-AND, and all the
// error-recovery cases authors hit when they fat-finger an operator.
func TestParserWhenBooleanExpressions(t *testing.T) {
	leafA := resonator.Condition{Branch: &resonator.Branch{Path: ".a"}, MatchType: resonator.MatchType("eq"), MatchValue: int64(1)}
	leafB := resonator.Condition{Branch: &resonator.Branch{Path: ".b"}, MatchType: resonator.MatchType("eq"), MatchValue: int64(2)}
	leafC := resonator.Condition{Branch: &resonator.Branch{Path: ".c"}, MatchType: resonator.MatchType("eq"), MatchValue: int64(3)}

	leaf := func(c resonator.Condition) resonator.WhenExpr {
		return resonator.WhenExpr{Leaf: c, HasLeaf: true}
	}
	exprWhen := func(e *resonator.WhenExpr) *resonator.When {
		return &resonator.When{Expr: e}
	}

	cases := []struct {
		name    string
		input   string
		res     *resonator.Resonator
		errs    int
		errmsgs []string
	}{
		// --- happy path: each operator in isolation ---
		{
			name:  "logical or",
			input: `WHEN .a == 1 || .b == 2`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{Or: []resonator.WhenExpr{
				leaf(leafA), leaf(leafB),
			}})},
		},
		{
			name:  "logical and",
			input: `WHEN .a == 1 && .b == 2`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{And: []resonator.WhenExpr{
				leaf(leafA), leaf(leafB),
			}})},
		},
		{
			name:  "comma is AND",
			input: `WHEN .a == 1, .b == 2`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{And: []resonator.WhenExpr{
				leaf(leafA), leaf(leafB),
			}})},
		},
		{
			name:  "logical not",
			input: `WHEN !(.a == 1)`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{
				Not: &resonator.WhenExpr{Leaf: leafA, HasLeaf: true},
			})},
		},
		{
			name:  "paren grouping preserves operand",
			input: `WHEN (.a == 1)`,
			res:   &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{Leaf: leafA, HasLeaf: true})},
		},

		// --- precedence: && binds tighter than || ---
		{
			name:  "or binds looser than and",
			input: `WHEN .a == 1 || .b == 2 && .c == 3`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{Or: []resonator.WhenExpr{
				leaf(leafA),
				{And: []resonator.WhenExpr{leaf(leafB), leaf(leafC)}},
			}})},
		},
		{
			name:  "and binds looser than not",
			input: `WHEN !.a == 1 && .b == 2`,
			// `!` only consumes the next primary, so this is (!leaf_a) && leaf_b.
			// `.a == 1` is one primary so `!` wraps the full leaf.
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{And: []resonator.WhenExpr{
				{Not: &resonator.WhenExpr{Leaf: leafA, HasLeaf: true}},
				leaf(leafB),
			}})},
		},
		{
			name:  "parens override precedence",
			input: `WHEN (.a == 1 || .b == 2) && .c == 3`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{And: []resonator.WhenExpr{
				{Or: []resonator.WhenExpr{leaf(leafA), leaf(leafB)}},
				leaf(leafC),
			}})},
		},

		// --- associativity: n-ary flattening ---
		{
			name:  "or is n-ary not nested binary",
			input: `WHEN .a == 1 || .b == 2 || .c == 3`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{Or: []resonator.WhenExpr{
				leaf(leafA), leaf(leafB), leaf(leafC),
			}})},
		},
		{
			name:  "and is n-ary not nested binary",
			input: `WHEN .a == 1 && .b == 2 && .c == 3`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{And: []resonator.WhenExpr{
				leaf(leafA), leaf(leafB), leaf(leafC),
			}})},
		},

		// --- comma interaction (binds at AND precedence) ---
		{
			name:  "comma mixed with or: comma binds tighter",
			input: `WHEN .a == 1, .b == 2 || .c == 3`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{Or: []resonator.WhenExpr{
				{And: []resonator.WhenExpr{leaf(leafA), leaf(leafB)}},
				leaf(leafC),
			}})},
		},
		{
			name:  "comma and && interchangeable in an AND chain",
			input: `WHEN .a == 1, .b == 2 && .c == 3`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{And: []resonator.WhenExpr{
				leaf(leafA), leaf(leafB), leaf(leafC),
			}})},
		},

		// --- nesting / double negation ---
		{
			name:  "double parens",
			input: `WHEN ((.a == 1))`,
			res:   &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{Leaf: leafA, HasLeaf: true})},
		},
		{
			name:  "double not",
			input: `WHEN !!(.a == 1)`,
			res: &resonator.Resonator{When: exprWhen(&resonator.WhenExpr{
				Not: &resonator.WhenExpr{Not: &resonator.WhenExpr{Leaf: leafA, HasLeaf: true}},
			})},
		},

		// --- error recovery cases ---
		{
			name:    "trailing &&",
			input:   `WHEN .a == 1 &&`,
			res:     &resonator.Resonator{},
			errs:    1,
			errmsgs: []string{`WHEN expected expression after '&&'`},
		},
		{
			name:    "trailing ||",
			input:   `WHEN .a == 1 ||`,
			res:     &resonator.Resonator{},
			errs:    1,
			errmsgs: []string{`WHEN expected expression after '||'`},
		},
		{
			name:    "lone !",
			input:   `WHEN !`,
			res:     &resonator.Resonator{},
			errs:    1,
			errmsgs: []string{`WHEN expected expression after '!'`},
		},
		{
			name:    "missing closing paren",
			input:   `WHEN (.a == 1 || .b == 2`,
			res:     &resonator.Resonator{},
			errs:    1,
			errmsgs: []string{`WHEN expected ')' to close group`},
		},
		{
			// `)` after `||` is technically a primary-position error,
			// but the parser sees it as the operand `||` is missing
			// and prefers the operator-centric message ("you've left
			// `||` dangling"). That's more actionable than naming the
			// downstream token.
			name:    "stray closing paren after operator",
			input:   `WHEN .a == 1 || )`,
			res:     &resonator.Resonator{},
			errs:    1,
			errmsgs: []string{`WHEN expected expression after '||'`},
		},
		{
			// `)` at fresh primary position (no prior operator) hits
			// the catch-all primary error.
			name:    "leading closing paren",
			input:   `WHEN )`,
			res:     &resonator.Resonator{},
			errs:    1,
			errmsgs: []string{`WHEN expected branch or '(', got ')'`},
		},
		{
			name:    "empty group",
			input:   `WHEN ()`,
			res:     &resonator.Resonator{},
			errs:    1,
			errmsgs: []string{`WHEN expected expression inside '()'`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := lexer.New(tc.input)
			p := parser.New(l)
			res := p.ParseEvent()

			if tc.errs != 0 {
				test.Equals(t, tc.errmsgs, p.Errors())
			}
			if tc.errs != len(p.Errors()) {
				fmt.Printf("errs %s\n", p.Errors())
			}
			test.Equals(t, tc.errs, len(p.Errors()))
			test.Equals(t, tc.res, res)
		})
	}
}
