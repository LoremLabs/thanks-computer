package resonator_test

import (
	"fmt"
	"strconv"
	"testing"

	// "github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

func TestResonatorWhenTable(t *testing.T) {

	tests := []struct {
		resonator string
		matches   bool
		rawEvent  string
		comments  string
	}{
		{
			`WHEN * WITH name = "highest"`,
			true,
			`{"num":6.13,"strs":["a","b"]}`,
			`star`,
		},
		{
			`WHEN .num == 6.13`,
			true,
			`{"num":6.13,"strs":["a","b"]}`,
			`branch match float`,
		},
		{
			`WHEN *`,
			false,
			``,
			`empty input`,
		},
		{
			`SELECT @x AS .y`,
			true,
			`{"num":6.13,"strs":["a","b"]}`,
			`no when star match`,
		},
		{
			`WHEN .num == 6.13, .str =~ /baz/, .str == "baz", .truthy == true, .a.number == 42`,
			true,
			`{"num":6.13,"str":"baz","truthy":true,"a":{"number":42}}`,
			`equals matches`,
		},
		{
			`WHEN .num != 6.14, .str !~ /foo/, .str != "faz", .truthy != false, .a.number != 43`,
			true,
			`{"num":6.13,"str":"baz","truthy":true,"a":{"number":42}}`,
			`not equals matches`,
		},
		{
			`WHEN .num < 6.14, .str < "faz", .a.number < 43`,
			true,
			`{"num":6.13,"str":"baz","truthy":true,"a":{"number":42}}`,
			`lt matches`,
		},
		{
			`WHEN .num <= 6.14, .str <= "baz", .a.number <= 43`,
			true,
			`{"num":6.13,"str":"baz","truthy":true,"a":{"number":42}}`,
			`lteq matches`,
		},
		{
			`WHEN .num > 6.129, .str > "aaz", .a.number > 4`,
			true,
			`{"num":6.13,"str":"baz","truthy":true,"a":{"number":42}}`,
			`gt matches`,
		},
		{
			`WHEN .num >= 6.129, .str >= "aaz", .a.number >= 42`,
			true,
			`{"num":6.13,"str":"baz","truthy":true,"a":{"number":42}}`,
			`gteq matches`,
		},

		// --- in: strict membership (the design's §2.2 table, then edges) ---
		{`WHEN .status in ["pending", "running"]`, true, `{"status":"pending"}`, `in string member`},
		{`WHEN .status in ["pending", "running"]`, false, `{"status":"failed"}`, `in string non-member`},
		{`WHEN .n in [1, 2, 3]`, true, `{"n":1}`, `in number member`},
		{`WHEN .n in [1, 2, 3]`, false, `{"n":"1"}`, `in: string "1" is not the number 1`},
		{`WHEN .s in ["1"]`, false, `{"s":1}`, `in: number 1 is not the string "1"`},
		{`WHEN .b in [true, false]`, true, `{"b":true}`, `in bool member`},
		{`WHEN .b in [true]`, false, `{"b":"true"}`, `in: string "true" is not true`},
		{`WHEN .b in [false]`, false, `{}`, `in: missing path is not false (unlike == false)`},
		{`WHEN .x in [null, "pending"]`, true, `{"x":null}`, `in explicit null matches null element`},
		{`WHEN .x in [null, "pending"]`, false, `{}`, `in missing path is a member of nothing`},
		{`WHEN .x in []`, false, `{"x":"x"}`, `in empty collection`},
		{`WHEN .n in [1, 2]`, true, `{"n":1.0}`, `in 1 equals 1.0`},
		{`WHEN .n in [1.0]`, true, `{"n":1}`, `in 1.0 equals 1`},
		{`WHEN .s in ["Pending"]`, false, `{"s":"pending"}`, `in is case-sensitive`},
		{`WHEN .x in [1, "a", true, null]`, true, `{"x":"a"}`, `in mixed scalars`},
		{`WHEN .x in ["a"]`, false, `{"x":["a"]}`, `in: an array on the left never matches`},
		{`WHEN .x in ["a"]`, false, `{"x":{"a":1}}`, `in: an object on the left never matches`},
		{`WHEN !(.x in ["a"])`, true, `{}`, `negated in with a missing path is true`},
		{`WHEN !(.x in ["a"])`, false, `{"x":"a"}`, `negated in with a member is false`},
		{`WHEN .r in &array("a", "b")`, true, `{"r":"b"}`, `in call rhs`},
		{`WHEN .n in &json("[1, 2]")`, true, `{"n":2}`, `in &json rhs (float64 elements)`},
		{`WHEN .n in &json("{\"n\": 2}")`, false, `{"n":2}`, `in: a call returning a non-array is no-match`},
		{`WHEN .a.0 in ["x"]`, true, `{"a":["x"]}`, `in array index path`},
		{`WHEN .h.content-type in ["text/plain"]`, true, `{"h":{"content-type":"text/plain"}}`, `in hyphenated segment`},
		{`WHEN .a == 1 && .x in ["q"] || .x in ["z"]`, true, `{"a":2,"x":"z"}`, `in composes with && and ||`},
		{`WHEN @src in ["inspect", "call"]`, true, `{"_txc":{"src":"call"}}`, `in with @ sugar`},

		// --- exists: presence, regardless of value (the addendum's table) ---
		{`WHEN exists .x`, false, `{}`, `exists missing`},
		{`WHEN exists .x`, true, `{"x":null}`, `exists explicit null`},
		{`WHEN exists .x`, true, `{"x":""}`, `exists empty string`},
		{`WHEN exists .x`, true, `{"x":false}`, `exists false`},
		{`WHEN exists .x`, true, `{"x":0}`, `exists zero`},
		{`WHEN exists .x`, true, `{"x":[]}`, `exists empty array`},
		{`WHEN exists .x`, true, `{"x":{}}`, `exists empty object`},
		{`WHEN !exists .x`, true, `{}`, `not exists missing`},
		{`WHEN !exists .x`, false, `{"x":null}`, `not exists explicit null`},
		{`WHEN exists .a.b.c`, false, `{"a":{}}`, `exists missing intermediate`},
		{`WHEN exists .a.b.c`, true, `{"a":{"b":{"c":null}}}`, `exists nested null`},
		{`WHEN exists .a.1`, true, `{"a":[1,2]}`, `exists array index present`},
		{`WHEN exists .a.1`, false, `{"a":[1]}`, `exists array index absent`},
		{`WHEN exists .h.content-type`, true, `{"h":{"content-type":"x"}}`, `exists hyphenated segment`},
		{`WHEN exists @call.input.run`, true, `{"_txc":{"call":{"input":{"run":""}}}}`, `exists with @ sugar`},
		{`WHEN exists .a && .status == "ready"`, true, `{"a":0,"status":"ready"}`, `exists composes with &&`},
		{`WHEN !(exists .a || exists .b)`, true, `{}`, `negated group of exists`},
		{`WHEN !(exists .a || exists .b)`, false, `{"b":null}`, `negated group of exists, one present`},
		{`WHEN exists .a, .a == ""`, true, `{"a":""}`, `exists with comma AND`},
		// The contrast that motivates exists: == null is stored as "" and
		// is true for a missing path, an explicit null AND an empty string.
		{`WHEN .x == null`, true, `{"x":""}`, `== null matches empty string (pinned existing behaviour)`},
		{`WHEN .x != null`, false, `{"x":""}`, `!= null is false for empty string (pinned existing behaviour)`},
	}

	for _, tt := range tests {

		// create resonators from slot slotDefinitions
		fmt.Printf("\t%s\n", tt.comments)
		res := getResonator(tt.resonator)
		matches := res.WhenMatches(tt.rawEvent)

		test.Equals(t, tt.comments+": "+strconv.FormatBool(matches), tt.comments+": "+strconv.FormatBool(tt.matches))
	}
}

func getResonator(def string) *resonator.Resonator {
	// l := lexer.New(def)
	// p := parser.New(l)
	// res := p.ParseEvent()
	// return res
	return txcl.ResonatorNoErr(def)
}

// TestWhenExprBoolean covers the grammar v2 evaluator: tree-walk
// semantics for &&, ||, !, and paren grouping, plus short-circuit and
// legacy-shape back-compat.
func TestWhenExprBoolean(t *testing.T) {
	env := `{"a":1,"b":2,"c":3}`

	cases := []struct {
		name  string
		rule  string
		match bool
	}{
		{"or first match", `WHEN .a == 1 || .b == 99`, true},
		{"or second match", `WHEN .a == 99 || .b == 2`, true},
		{"or both fail", `WHEN .a == 99 || .b == 99`, false},
		{"and both match", `WHEN .a == 1 && .b == 2`, true},
		{"and one fail", `WHEN .a == 1 && .b == 99`, false},
		{"not match becomes miss", `WHEN !(.a == 1)`, false},
		{"not miss becomes match", `WHEN !(.a == 99)`, true},
		{"double not", `WHEN !!(.a == 1)`, true},
		// && binds tighter than ||: `.a==1 || .b==99 && .c==99`
		// evaluates as `1 || (99 && 99)` = true.
		{"precedence: or wraps and", `WHEN .a == 1 || .b == 99 && .c == 99`, true},
		{"parens force or-first", `WHEN (.a == 99 || .b == 2) && .c == 3`, true},
		{"parens force or-first miss", `WHEN (.a == 99 || .b == 99) && .c == 3`, false},
		// Comma is AND-precedence: comma binds tighter than ||.
		// `.a==1, .b==99 || .c==3` → `(1 && 99) || 3` → true.
		{"comma with or: comma binds tighter", `WHEN .a == 1, .b == 99 || .c == 3`, true},
		// All branches false.
		{"comma with or: all miss", `WHEN .a == 99, .b == 99 || .c == 99`, false},
		// N-ary and: all must match.
		{"n-ary and all match", `WHEN .a == 1 && .b == 2 && .c == 3`, true},
		{"n-ary and one fail", `WHEN .a == 1 && .b == 99 && .c == 3`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := getResonator(tc.rule)
			got := res.WhenMatches(env)
			if got != tc.match {
				t.Errorf("%s: WhenMatches = %v, want %v", tc.rule, got, tc.match)
			}
		})
	}
}

// TestWhenExprShortCircuit verifies that || returns true on first
// match without evaluating later leaves, and && returns false on first
// miss without evaluating later leaves. We detect over-evaluation via
// a sentinel Condition that would never legitimately match — if the
// evaluator touches it, we'd see false where we expected true.
func TestWhenExprShortCircuit(t *testing.T) {
	env := `{"a":1}`

	// Build a tree by hand so we can plant a sentinel that should never
	// be reached. The sentinel has an unrecognized MatchType, so if
	// evaluated it returns false. Wrapped in `||` with a true-leaf on
	// the left, short-circuit must keep the whole OR at true.
	matchA := resonator.Condition{
		Branch: &resonator.Branch{Path: ".a"}, MatchType: "eq", MatchValue: int64(1),
	}
	sentinel := resonator.Condition{
		Branch: &resonator.Branch{Path: ".a"}, MatchType: "unrecognized", MatchValue: int64(0),
	}

	orRes := resonator.Resonator{When: &resonator.When{Expr: &resonator.WhenExpr{
		Or: []resonator.WhenExpr{
			{Leaf: matchA, HasLeaf: true},
			{Leaf: sentinel, HasLeaf: true},
		},
	}}}
	if got := orRes.WhenMatches(env); !got {
		t.Errorf("OR short-circuit failed: expected true (left leaf matches), got false")
	}

	// Mirror for AND: sentinel-on-the-right should be unreachable
	// when the left fails.
	failingA := resonator.Condition{
		Branch: &resonator.Branch{Path: ".a"}, MatchType: "eq", MatchValue: int64(999),
	}
	andRes := resonator.Resonator{When: &resonator.When{Expr: &resonator.WhenExpr{
		And: []resonator.WhenExpr{
			{Leaf: failingA, HasLeaf: true},
			{Leaf: sentinel, HasLeaf: true},
		},
	}}}
	if got := andRes.WhenMatches(env); got {
		t.Errorf("AND short-circuit failed: expected false (left leaf fails), got true")
	}
}

// TestWhenLegacyConditionsBackCompat builds a When using the old
// Conditions slice directly (the shape any in-flight callers or JSON
// might still carry) and verifies the promotion path treats it as a
// flat AND.
func TestWhenLegacyConditionsBackCompat(t *testing.T) {
	env := `{"a":1,"b":2}`

	condA := resonator.Condition{
		Branch: &resonator.Branch{Path: ".a"}, MatchType: "eq", MatchValue: int64(1),
	}
	condB := resonator.Condition{
		Branch: &resonator.Branch{Path: ".b"}, MatchType: "eq", MatchValue: int64(2),
	}
	condBFail := resonator.Condition{
		Branch: &resonator.Branch{Path: ".b"}, MatchType: "eq", MatchValue: int64(99),
	}

	// Two matching conditions: legacy AND should match.
	resAllMatch := resonator.Resonator{When: &resonator.When{
		Conditions: []resonator.Condition{condA, condB},
	}}
	if !resAllMatch.WhenMatches(env) {
		t.Error("legacy Conditions AND: all match should be true")
	}

	// One miss: legacy AND should fail.
	resOneFails := resonator.Resonator{When: &resonator.When{
		Conditions: []resonator.Condition{condA, condBFail},
	}}
	if resOneFails.WhenMatches(env) {
		t.Error("legacy Conditions AND: one miss should be false")
	}

	// Star mixed in: matches everything (preserved legacy semantics).
	resStarMixed := resonator.Resonator{When: &resonator.When{
		Conditions: []resonator.Condition{{Star: true}, condBFail},
	}}
	if !resStarMixed.WhenMatches(env) {
		t.Error("legacy Conditions with Star should match regardless")
	}
}

// TestWhenRegexWithNonStringPatternIsFalse: a regex comparison whose
// pattern isn't a string is no-match. It used to be an unchecked type
// assertion, which panicked on the processor's goroutines and took the
// chassis down.
func TestWhenRegexWithNonStringPatternIsFalse(t *testing.T) {
	env := `{"x":"5"}`
	for _, mt := range []resonator.MatchType{"=~", "!~"} {
		for _, mv := range []interface{}{int64(5), 5.0, true, nil} {
			res := resonator.Resonator{When: &resonator.When{Expr: &resonator.WhenExpr{
				Leaf:    resonator.Condition{Branch: &resonator.Branch{Path: ".x"}, MatchType: mt, MatchValue: mv},
				HasLeaf: true,
			}}}
			if res.WhenMatches(env) {
				t.Errorf("%s %v (%T): want no match", mt, mv, mv)
			}
		}
	}
}

// TestWhenHyphenatedKey confirms a hyphenated branch path round-trips
// through the parser into a working gjson lookup at evaluation time.
func TestWhenHyphenatedKey(t *testing.T) {
	env := `{"_txc":{"web":{"req":{"headers":{"x-forwarded-proto":["https"]}}}}}`

	match := getResonator(`WHEN @web.req.headers.x-forwarded-proto.0 == "https"`)
	if !match.WhenMatches(env) {
		t.Error("hyphenated header path should match https")
	}

	miss := getResonator(`WHEN @web.req.headers.x-forwarded-proto.0 == "http"`)
	if miss.WhenMatches(env) {
		t.Error("hyphenated header path should not match http")
	}

	// Quoted-segment form must resolve to the same key.
	quoted := getResonator(`WHEN ._txc.web.req.headers."x-forwarded-proto".0 == "https"`)
	if !quoted.WhenMatches(env) {
		t.Error("quoted-segment hyphenated path should match https")
	}
}

// TestWhenNilStillMatches preserves the existing semantics: a
// Resonator with no WHEN clause matches every (non-empty) input.
func TestWhenNilStillMatches(t *testing.T) {
	env := `{"anything":true}`
	res := resonator.Resonator{When: nil}
	if !res.WhenMatches(env) {
		t.Error("nil When should match everything")
	}
	// And keep the empty-input contract.
	if res.WhenMatches("") {
		t.Error("nil When should still not match empty input")
	}
}

// func BenchmarkEval(b *testing.B) {
// 	slotDefinitions := [3]string{`WHEN * PRIORITY 10 WITH name = "highest"`, `WHEN * HAVING "thing" PRIORITY 10 WITH name = "highest"`, `WHEN * PRIORITY 1 WITH name = "lowest"`}
//
// 	// create resonators from slot slotDefinitions
// 	resonators := make([]*resonator.Resonator, 0)
// 	for _, def := range slotDefinitions {
// 		res := getResonator(def)
// 		resonators = append(resonators, res)
// 	}
//
// 	// input
// 	rawEvent := `{"num":6.13,"strs":["a","b"]}`
//
// 	for n := 0; n < b.N; n++ {
// 		// choose the best resonator
// 		bootstrap.Eval(rawEvent, resonators, "testing")
// 	}
//
// 	b.ReportAllocs()
// }

func TestWhenFunctionCallEvaluation(t *testing.T) {
	// End-to-end: parse rule text, evaluate WhenMatches. The RHS call
	// is envelope-independent by construction (literal args only), so
	// each row is deterministic — except &tz, which is pinned to UTC
	// where local wall-clock == UTC wall-clock on every date.
	cases := []struct {
		name      string
		resonator string
		rawEvent  string
		matches   bool
	}{
		{"eq int result", `WHEN .n == &add(1, 2)`, `{"n":3}`, true},
		{"eq int no match", `WHEN .n == &add(1, 2)`, `{"n":4}`, false},
		{"gt against sub", `WHEN .n > &sub(10, 4)`, `{"n":7}`, true},
		{"lt against div float", `WHEN .n < &div(7, 2)`, `{"n":3.4}`, true},
		{"string result", `WHEN .s == &concat("a", "-", "b")`, `{"s":"a-b"}`, true},
		{"bool result", `WHEN .ok == &has("{\"a\":1}", "a")`, `{"ok":true}`, true},
		{"tz UTC hour", `WHEN .h == &tz("UTC", "hour", 9)`, `{"h":9}`, true},
		{"numeric string envelope value coerces", `WHEN .n == &add(1, 2)`, `{"n":"3"}`, true},
		{"nested calls", `WHEN .n == &add(1, &mul(2, 3))`, `{"n":7}`, true},

		// Total semantics: a failing or nil call is FALSE, never an
		// error — including under != where "not equal to a value that
		// couldn't be computed" might read as true. One bad rule must
		// not halt the scope; strict mode catches typos at authoring.
		{"divide by zero is false", `WHEN .n == &div(1, 0)`, `{"n":1}`, false},
		{"divide by zero under ne is still false", `WHEN .n != &div(1, 0)`, `{"n":1}`, false},
		{"unknown zone is false", `WHEN .h == &tz("Not/AZone", "hour", 9)`, `{"h":9}`, false},
		{"nil try_ result is false", `WHEN .s == &try_json("not json")`, `{"s":""}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := txcl.Resonator(tc.resonator)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.resonator, err)
			}
			if got := res.WhenMatches(tc.rawEvent); got != tc.matches {
				t.Errorf("%q vs %s = %v, want %v", tc.resonator, tc.rawEvent, got, tc.matches)
			}
		})
	}
}

func TestWhenFunctionCallUnknownNameIsFalse(t *testing.T) {
	// Lenient parse accepts the name; evaluation treats the resolve
	// failure as no-match. Constructed programmatically because
	// txcl.Resonator would be identical — this pins the resonator-side
	// contract independent of parser behavior.
	cond := resonator.Condition{
		Branch:    &resonator.Branch{Path: ".n"},
		MatchType: resonator.MatchType("eq"),
		MatchCall: &ast.FunctionCall{Name: "definitely-not-real", Args: []ast.Value{}},
	}
	res := resonator.Resonator{When: &resonator.When{
		Expr: &resonator.WhenExpr{Leaf: cond, HasLeaf: true},
	}}
	if res.WhenMatches(`{"n":1}`) {
		t.Errorf("unknown function should evaluate to no-match")
	}
}

func TestWhenFunctionCallStrayPathArgIsFalse(t *testing.T) {
	// The parser forbids path args, but Condition can be built
	// programmatically. A stray PathRef resolves against an empty env
	// → nil → no-match, never a read of the real envelope.
	cond := resonator.Condition{
		Branch:    &resonator.Branch{Path: ".n"},
		MatchType: resonator.MatchType("eq"),
		MatchCall: &ast.FunctionCall{Name: "concat", Args: []ast.Value{ast.PathRef{Path: "n"}}},
	}
	res := resonator.Resonator{When: &resonator.When{
		Expr: &resonator.WhenExpr{Leaf: cond, HasLeaf: true},
	}}
	if res.WhenMatches(`{"n":"1"}`) {
		t.Errorf("stray path arg must not read the envelope (want no-match)")
	}
}
