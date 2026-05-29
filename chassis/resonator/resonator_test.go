package resonator_test

import (
	"fmt"
	"strconv"
	"testing"

	// "github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl"
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
