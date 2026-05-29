package txcl

import (
	"errors"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/parser"
)

func Resonator(def string) (*resonator.Resonator, error) {
	l := lexer.New(def)
	p := parser.New(l)
	res := p.ParseEvent()

	var errs error

	if len(p.Errors()) != 0 {
		errStr := strings.Join(p.Errors(), " : ")
		errs = errors.New(errStr)
	}

	return res, errs
}

// ResonatorNoErr Used mainly for testing
func ResonatorNoErr(def string) (*resonator.Resonator) {
	l := lexer.New(def)
	p := parser.New(l)
	res := p.ParseEvent()

	return res
}

// Validate parses def in strict mode and returns each diagnostic as its
// own message (empty slice == valid). Strict mode adds checks the
// lenient runtime parser (Resonator) deliberately omits so it never
// rejects an already-deployed rule:
//
//   - unterminated string / regex literals (from the lexer)
//   - tokens that aren't a recognized clause keyword reaching the
//     top level (unknown verbs, trailing garbage)
//
// Diagnostics are returned individually (not joined) so callers can
// report an accurate count. Lexer diagnostics come first, then parser
// diagnostics, collected after ParseEvent drains the lexer to EOF.
//
// Intended for the admin validate endpoint and authoring tools — the
// runtime path stays on Resonator.
func Validate(def string) []string {
	l := lexer.New(def)
	p := parser.New(l)
	p.SetStrict(true)
	p.ParseEvent()

	msgs := append([]string{}, l.Errors()...)
	msgs = append(msgs, p.Errors()...)
	return msgs
}
