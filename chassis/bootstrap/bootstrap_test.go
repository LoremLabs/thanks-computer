package bootstrap_test

import (
	"fmt"
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/bootstrap"
	// "github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/parser"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

func TestEvalTable(t *testing.T) {

	tests := []struct {
		slotDefinitions  []string
		best             int
		rawEvent         string
		transformedEvent string
		comments         string
	}{
		{
			[]string{`
				WHEN *
				WITH name = "highest
				"`,
				`
				WHEN *
				WITH name = "lowest"
				`},
			1,
			`{"num":6.13,"strs":["a","b"]}`,
			`{"num":6.13,"strs":["a","b"]}`,
			`no priority`,
		},
		{
			[]string{`WHEN .topic =~ /ads/ SELECT .num AS .copied WITH name = "highest" PRIORITY 1`},
			0,
			`{"topic":"ads-and-other-things","num":6.13,"strs":["a","b"]}`,
			`{"copied":6.13,"topic":"ads-and-other-things","num":6.13,"strs":["a","b"]}`,
			`SELECT path-copy (number)`,
		},
		{
			[]string{`WHEN * SELECT .num AS .alias WITH fallback = true PRIORITY 1"`, `WHEN * PRIORITY 2"`},
			1,
			`{"num":6.13,"strs":["a","b"]}`,
			`{"num":6.13,"strs":["a","b"]}`,
			`priority 2 wins; selected rule has SELECT but doesn't win`,
		},
		{
			[]string{`WHEN * SELECT .strs AS .strs_copy WITH fallback = true PRIORITY 1"`, `WHEN * PRIORITY 2"`},
			1,
			`{"num":6.13,"strs":["a","b"]}`,
			`{"num":6.13,"strs":["a","b"]}`,
			`select doesn't fire when its rule isn't the winner`,
		},
		{
			[]string{`WHEN * SELECT .num AS .out PRIORITY 1"`},
			0,
			`{"num":6.13,"strs":["a","b"]}`,
			`{"out":6.13,"num":6.13,"strs":["a","b"]}`,
			`SELECT writes alongside untouched input`,
		},
		{
			[]string{`WHEN * SELECT .noexiste AS .out DEFAULT "fallback" PRIORITY 1"`},
			0,
			`{"num":6.13,"strs":["a","b"]}`,
			`{"out":"fallback","num":6.13,"strs":["a","b"]}`,
			`SELECT with missing source uses DEFAULT`,
		},
		{
			[]string{`SET .type.another = "huh" PRIORITY 1"`},
			0,
			`{"num":6.13,"strs":["a","b"],"type":{"hrm":true}}`,
			`{"num":6.13,"strs":["a","b"],"type":{"another":"huh","hrm":true}}`,
			`no select, with set`,
		},
		{
			[]string{`SET .type.another = "huh" SELECT .type AS .type_copy PRIORITY 1"`},
			0,
			`{"num":6.13,"strs":["a","b"],"type":{"hrm":true}}`,
			`{"type_copy":{"another":"huh","hrm":true},"num":6.13,"strs":["a","b"],"type":{"another":"huh","hrm":true}}`,
			`SET-pre runs before SELECT, so the copy sees the post-SET value`,
		},
		{
			[]string{`SELECT .float AS .float_copy SET .float2 = 8.1 PRIORITY 10 EXEC "hello"`},
			0,
			`{"float":6.13}`,
			`{"float2":8.1,"float_copy":6.13,"float":6.13}`,
			`SELECT + SET-post`,
		},
		{
			[]string{`WHEN .moo == 2 SELECT .moo AS .moo_copy SET .matched = true PRIORITY 100 EXEC "hello"`},
			-1,
			`{"float":6.13}`,
			`{"float":6.13}`,
			`when doesn't match`,
		},
		{
			// SET-post (the SET after SELECT) runs LAST, so `matched`
			// prepends after `moo_copy`.
			[]string{`WHEN .moo == 2 SELECT .moo AS .moo_copy SET .matched = true PRIORITY 100 EXEC "hello"`, `WHEN .moo != false SELECT .moo AS .moo_copy SET .matched = false PRIORITY 99 EXEC "hi"`},
			0,
			`{"moo":2, "baz":"moose"}`,
			`{"matched":true,"moo_copy":2,"moo":2, "baz":"moose"}`,
			`when does match; winner's SELECT/SET overlays applied`,
		},
		{
			[]string{`WHEN .moo == 2 SELECT .moo AS .moo_copy SET .matched = true PRIORITY 100 EXEC "hello"`, `WHEN .moo != false SELECT .moo AS .moo_copy SET .matched = true PRIORITY 190 EXEC "hi"`},
			1,
			`{"moo":2, "baz":"moose"}`,
			`{"matched":true,"moo_copy":2,"moo":2, "baz":"moose"}`,
			`when does match; winner's SELECT copies .moo → .moo_copy`,
		},
		{
			[]string{`WHEN .moo.foo == 2 SELECT .moo AS .moo_copy SET .matched = true PRIORITY 100 EXEC "hello"`, `WHEN .moo.foo != 2 SELECT .moo AS .moo_copy SET .matched = true PRIORITY 101 EXEC "hello"`, `WHEN .moo.foo <= 2 SELECT .moo AS .moo_copy SET .matched = true PRIORITY 102 EXEC "hello"`, `WHEN .moo.foo >= 2 SELECT .moo AS .moo_copy SET .matched = true PRIORITY 103 EXEC "hello"`},
			3,
			`{"moo":{"foo":2}, "baz":"moose"}`,
			`{"matched":true,"moo_copy":{"foo":2},"moo":{"foo":2}, "baz":"moose"}`,
			`multi-layer when does match; SELECT copies the nested object`,
		},
	}

	for _, tt := range tests {

		// create resonators from slot slotDefinitions
		resonators := make([]*resonator.Resonator, 0)
		for _, def := range tt.slotDefinitions {
			res := getResonator(def)
			resonators = append(resonators, res)
		}

		// choose the best resonator
		best, e := bootstrap.Eval(tt.rawEvent, resonators, "testing")

		fmt.Printf("\t%s\n", tt.comments)

		// expected
		var expectedBest *resonator.Resonator
		if tt.best != -1 {
			expectedBest = getResonator(tt.slotDefinitions[tt.best])
		} else {
			expectedBest = nil
		}

		test.Equals(t, expectedBest, best)

		// see if input filters
		test.Equals(t, tt.transformedEvent, e)
	}
}

func TestEmptyEval(t *testing.T) {

	// create resonators from slot slotDefinitions
	resonators := make([]*resonator.Resonator, 0)

	// input
	rawEvent := `{"num":6.13,"strs":["a","b"]}`

	// choose the best resonator
	best, _ := bootstrap.Eval(rawEvent, resonators, "testing")

	var r *resonator.Resonator = nil

	// no parser errors
	test.Equals(t, best, r)
}

func getResonator(def string) *resonator.Resonator {
	l := lexer.New(def)
	p := parser.New(l)
	res := p.ParseEvent()
	return res
}

func BenchmarkBootstrapEval(b *testing.B) {
	slotDefinitions := [3]string{`WHEN * PRIORITY 10 WITH name = "highest"`, `WHEN .num != 6.12, .a !~ /hi/ HAVING "thing" PRIORITY 10 WITH name = "highest"`, `WHEN * PRIORITY 1 WITH name = "lowest"`}

	// create resonators from slot slotDefinitions
	resonators := make([]*resonator.Resonator, 0)
	for _, def := range slotDefinitions {
		res := getResonator(def)
		resonators = append(resonators, res)
	}

	// input
	rawEvent := `{"a":"hellohellohellohellohellohellohellohellohello","num":6.13,"strs":["a","b"]}`

	for n := 0; n < b.N; n++ {
		// choose the best resonator
		bootstrap.Eval(rawEvent, resonators, "testing")
	}

	b.ReportAllocs()
}
