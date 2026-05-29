package parser

// https://interpreterbook.com/

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
)

const (
	_ int = iota
	LOWEST
	EQUALS      // ==
	LESSGREATER // > or <
	SUM         // +
	PRODUCT     // *
	PREFIX      // -X or !X
	CALL        // myFunction(X)
	INDEX       // array[index]
)

type Parser struct {
	l *lexer.Lexer

	errors []string

	curToken  token.Token
	peekToken token.Token

	seenSelect bool

	// strict enables validation-only diagnostics that the lenient
	// runtime parser must not raise — chiefly flagging tokens that
	// reach the top-level clause switch unrecognized (unknown verbs,
	// trailing garbage). Off by default so txcl.Resonator (runtime)
	// behaves exactly as before.
	strict bool

	// inGarbage suppresses cascading "unexpected token" errors: a run
	// of consecutive unrecognized top-level tokens (e.g. `SELEKT .x AS
	// .y` after a typo'd verb) reports a single diagnostic, not one
	// per token. Reset when a recognized clause keyword is reached.
	inGarbage bool
}

func New(l *lexer.Lexer) *Parser {
	p := &Parser{
		l:      l,
		errors: []string{},
	}

	// Read two tokens, so curToken and peekToken are both set
	p.nextToken()
	p.nextToken()

	return p
}

func (p *Parser) SeenSelect(hasSeen bool) {
	p.seenSelect = hasSeen
}

// SetStrict toggles validation-only diagnostics. txcl.Validate sets it;
// the runtime (txcl.Resonator) leaves it off.
func (p *Parser) SetStrict(strict bool) {
	p.strict = strict
}

func (p *Parser) nextToken() {
	p.curToken = p.peekToken
	p.peekToken = p.l.NextToken()
}

func (p *Parser) curTokenIs(t token.TokenType) bool {
	return p.curToken.Type == t
}

func (p *Parser) peekTokenIs(t token.TokenType) bool {
	return p.peekToken.Type == t
}

func (p *Parser) ParseEvent() *resonator.Resonator {
	// Walk tokens producing one Resonator per rule.
	// Clauses (in canonical order): WHEN  SET (pre)  SELECT  SET (post)  WITH  PRIORITY  EXEC
	r := resonator.New()

	// reset parse state in case we get called multiple times
	p.SeenSelect(false)

	for !p.curTokenIs(token.EOF) {
		phrase := p.parseEventPhrase()
		if phrase != nil {
			switch phrase.Type {
			case resonator.WHEN:
				r.When = phrase.When
			case resonator.PRIORITY:
				r.Priority = phrase.Priority
			case resonator.SELECT:
				r.Select = phrase.Select
			case resonator.WITH:
				r.With = phrase.With
			case resonator.SETPRE:
				r.SetPre = phrase.SetPre
			case resonator.SETPOST:
				r.SetPost = phrase.SetPost
			case resonator.EXEC:
				r.Exec = phrase.Exec
			case resonator.EMIT:
				r.Emit = phrase.Emit
			}
		}
		p.nextToken()
	}

	return r
}

func (p *Parser) parseEventPhrase() *resonator.Phrase {
	switch p.curToken.Type {
	case token.WHEN:
		p.inGarbage = false
		return p.parseWhenPhrase()
	case token.SET:
		p.inGarbage = false
		if !p.seenSelect {
			return p.parseSetPrePhrase()
		}
		return p.parseSetPostPhrase()
	case token.WITH:
		p.inGarbage = false
		return p.parseWithPhrase()
	case token.SELECT:
		p.inGarbage = false
		return p.parseSelectPhrase()
	case token.PRIORITY:
		p.inGarbage = false
		return p.parsePriorityPhrase()
	case token.EXEC:
		p.inGarbage = false
		return p.parseExecPhrase()
	case token.EMIT:
		p.inGarbage = false
		return p.parseEmitPhrase()
	default:
		// A token reached the top-level clause switch that isn't a
		// clause keyword. In well-formed TXCL only clause keywords (or
		// EOF) land here, so this is an unknown verb (`SELEKT …`) or
		// trailing garbage after a complete clause (`… DEFAULT "x" fmo`).
		// The lenient runtime parser silently skips it (returns nil);
		// strict validation reports it so authors see it at edit time.
		//
		// Only the first token of a contiguous garbage run is reported
		// (inGarbage gate) — a typo'd verb shouldn't spew one error per
		// trailing token.
		if p.strict && !p.inGarbage {
			p.errors = append(p.errors, fmt.Sprintf(
				"unexpected token %q — expected a clause keyword (WHEN, SET, SELECT, WITH, PRIORITY, EXEC, EMIT)",
				p.curToken.Literal))
			p.inGarbage = true
		}
		return nil
	}
}

// parseLiteralValue parses the RHS of a SET / WITH assignment at the
// current token. Accepts a scalar (bool/int/float/string) OR an
// array literal `[v1, v2, …]` of literal values. Arrays are
// heterogeneous, may be nested, may be empty (`[]`), and tolerate a
// trailing comma.
//
// On success, returns (value, true) and leaves the parser positioned
// on the literal's final token — for scalars that's the token
// itself; for arrays it's the closing `]`. The existing call-site
// pattern (post-switch nextToken / comma handling) keeps working
// unchanged for both forms.
//
// On an unexpected token, emits wrongTypeParseError and returns
// (nil, false) without advancing.
func (p *Parser) parseLiteralValue() (interface{}, bool) {
	switch p.curToken.Type {
	case token.INT:
		v, _ := strconv.ParseInt(p.curToken.Literal, 0, 64)
		return v, true
	case token.FLOAT:
		v, _ := strconv.ParseFloat(p.curToken.Literal, 64)
		return v, true
	case token.TRUE, token.FALSE:
		v, _ := strconv.ParseBool(p.curToken.Literal)
		return v, true
	case token.STRING:
		return p.curToken.Literal, true
	case token.LBRACKET:
		return p.parseArrayLiteral()
	default:
		p.wrongTypeParseError("bool, int, float, string, or array")
		return nil, false
	}
}

// parseValueExpr is the top-level RHS-value parser. Returns an
// ast.Value — ast.Literal for scalars and array literals,
// ast.PathRef for `@x.y.z` / `.x.y.z` envelope references, and
// ast.FunctionCall for `&fn(args...)` forms. Replaces direct
// parseLiteralValue calls at SET / EMIT / WITH / SELECT-DEFAULT
// sites so the three runtime-value shapes all plumb through one
// helper.
//
// Path-on-RHS support (PathRef) lets a rule do `SET .x = @y` or
// pass `@field` as a function arg. The lexer emits BRANCH for both
// `.path` and `@path` (the latter rewritten to `._txc.path`); the
// parser's BRANCH branch here unwraps the leading `.` and stores
// the gjson-walkable path on the PathRef. Without this, the new
// `&fn(@arg)` pattern from internal docs/todo-txcl-expressions.md §3.1
// would fail at parse time.
//
// Array elements stay literal-only in this PR: parseArrayLiteral
// still recurses through parseLiteralValue, not parseValueExpr.
// Function calls / paths inside array literals would require unwrap
// semantics that aren't worth designing until a real use case
// motivates it.
func (p *Parser) parseValueExpr() (ast.Value, bool) {
	if p.curTokenIs(token.AMP_IDENT) {
		return p.parseFunctionCall()
	}
	if p.curTokenIs(token.BRANCH) {
		// Strip the leading `.` so the PathRef carries the
		// gjson/sjson-shaped path. The lexer already rewrote
		// `@x.y` to `._txc.x.y`, so PathRef downstream addresses
		// the envelope via the same path convention WHEN clauses
		// use.
		return ast.PathRef{Path: strings.TrimPrefix(p.curToken.Literal, ".")}, true
	}
	v, ok := p.parseLiteralValue()
	if !ok {
		return nil, false
	}
	return ast.Literal{V: v}, true
}

// parseFunctionCall parses `AMP_IDENT LPAREN arg-list? RPAREN`.
// Called with p.curToken as the AMP_IDENT. On success returns the
// FunctionCall AST node with the parser positioned on the closing
// RPAREN (so the call-site's existing post-value advance logic
// works unchanged). Arg-list is comma-separated parseValueExpr
// calls, making nested calls and mixed-type args (literal, path,
// nested call) all work via the same recursion.
func (p *Parser) parseFunctionCall() (ast.Value, bool) {
	name := p.curToken.Literal
	if !p.peekTokenIs(token.LPAREN) {
		// Advance so the error message names the offending token
		// (the thing where '(' should have been).
		p.nextToken()
		p.wrongTypeParseError("'(' after &" + name)
		return nil, false
	}
	p.nextToken() // onto '('

	// Zero-arg form: `&fn()`. Empty Args slice is canonical (nil
	// would force every consumer to nil-check; the runtime walks
	// Args via range, which is happy with a nil/empty slice).
	if p.peekTokenIs(token.RPAREN) {
		p.nextToken() // onto ')'
		return ast.FunctionCall{Name: name, Args: []ast.Value{}}, true
	}

	args := []ast.Value{}
	for {
		p.nextToken() // onto first/next arg's start token
		v, ok := p.parseValueExpr()
		if !ok {
			return nil, false
		}
		args = append(args, v)
		switch {
		case p.peekTokenIs(token.COMMA):
			p.nextToken() // onto the comma; loop advances past it
		case p.peekTokenIs(token.RPAREN):
			p.nextToken() // onto ')'
			return ast.FunctionCall{Name: name, Args: args}, true
		default:
			// Advance so the error message names the actual
			// offending token rather than the last successfully-
			// parsed arg.
			p.nextToken()
			p.wrongTypeParseError("',' or ')'")
			return nil, false
		}
	}
}

// parseArrayLiteral parses `[v1, v2, …]` starting at LBRACKET. On
// success returns ([]interface{}, true) with the parser positioned
// on the closing RBRACKET. Empty arrays and trailing commas are
// accepted.
func (p *Parser) parseArrayLiteral() (interface{}, bool) {
	items := []interface{}{}
	// Empty array `[]`: advance to RBRACKET and we're done.
	if p.peekTokenIs(token.RBRACKET) {
		p.nextToken()
		return items, true
	}
	for {
		p.nextToken()
		// Trailing comma: `[1, 2,]` lands us on RBRACKET after the
		// comma was consumed in the previous iteration's tail.
		if p.curTokenIs(token.RBRACKET) {
			return items, true
		}
		v, ok := p.parseLiteralValue()
		if !ok {
			return nil, false
		}
		items = append(items, v)
		switch {
		case p.peekTokenIs(token.COMMA):
			p.nextToken() // step onto the comma; next iter advances past it
		case p.peekTokenIs(token.RBRACKET):
			p.nextToken() // step onto the closing bracket and exit
			return items, true
		default:
			// Advance so the error message names the actual
			// offending token (peek) rather than the last value
			// we successfully parsed.
			p.nextToken()
			p.wrongTypeParseError("',' or ']'")
			return nil, false
		}
	}
}

// parse tokens for With Phrase (WITH foo = "bar", moo = 1)
func (p *Parser) parseWithPhrase() *resonator.Phrase {
	withs := make(map[string]ast.Value)
	for !p.curTokenIs(token.EOF) && !p.peekTokenIs(token.SET) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) && !p.peekTokenIs(token.EMIT) {

		if p.curTokenIs(token.WITH) {
			p.nextToken()
		}

		if !p.curTokenIs(token.IDENT) {
			p.wrongTypeParseError("with variable name")
			return nil
		}

		with := p.curToken.Literal
		p.nextToken()

		// Dotted-path WITH keys (e.g. `secrets.headers.authorization.secret`)
		// lex as IDENT + BRANCH(".headers.authorization.secret"). Concat
		// the BRANCH literal directly so the key becomes the full path.
		// The processor's ResonatingOps decorator uses sjson.Set on this
		// key, which interprets the dots as nested-object navigation —
		// producing the structure the secrets-store machinery expects
		// (see internal docs/todo-secret-store.md §4 Option A).
		if p.curTokenIs(token.BRANCH) {
			with += p.curToken.Literal
			p.nextToken()
		}

		if !p.curTokenIs(token.ASSIGN) {
			p.curError(token.ASSIGN)
			return nil
		}
		p.nextToken()

		withValue, ok := p.parseValueExpr()
		if !ok {
			return nil
		}

		withs[with] = withValue

		if p.peekTokenIs(token.COMMA) {
			p.nextToken()
		}
		if !p.peekTokenIs(token.SET) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) && !p.peekTokenIs(token.EMIT) {
			p.nextToken()
		}
	}

	phrase := &resonator.Phrase{Type: resonator.WITH, With: withs}

	return phrase
}

// parse tokens for (pre SET) Phrase (SET .foo = "bar", .moo = 1)
func (p *Parser) parseSetPrePhrase() *resonator.Phrase {
	overrides := make([]resonator.BranchValue, 0)

	for !p.curTokenIs(token.EOF) && !p.peekTokenIs(token.SELECT) && !p.peekTokenIs(token.WITH) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) && !p.peekTokenIs(token.EMIT) {

		if p.curTokenIs(token.COMMA) || p.curTokenIs(token.SET) {
			p.nextToken()
		}

		// .foo
		if !p.curTokenIs(token.BRANCH) {
			p.wrongTypeParseError(".branch")
			return nil
		}

		branch := p.curToken.Literal
		p.nextToken()

		if !p.curTokenIs(token.ASSIGN) {
			p.curError(token.ASSIGN)
			return nil
		}
		p.nextToken()

		branchValue, ok := p.parseValueExpr()
		if !ok {
			return nil
		}

		override := resonator.BranchValue{Path: branch, Value: branchValue}
		overrides = append(overrides, override)

		if p.peekTokenIs(token.COMMA) {
			p.nextToken()
		}
		if !p.peekTokenIs(token.SELECT) && !p.peekTokenIs(token.WITH) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) && !p.peekTokenIs(token.EMIT) {
			p.nextToken()
		}
	}

	setPre := &resonator.Set{Overrides: overrides}
	phrase := &resonator.Phrase{Type: resonator.SETPRE, SetPre: setPre}

	return phrase
}

// parse tokens for (post SET) Phrase (SET .foo = "bar", .moo = 1)
func (p *Parser) parseSetPostPhrase() *resonator.Phrase {
	overrides := make([]resonator.BranchValue, 0)

	for !p.curTokenIs(token.EOF) && !p.peekTokenIs(token.WITH) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) && !p.peekTokenIs(token.EMIT) {

		if p.curTokenIs(token.COMMA) || p.curTokenIs(token.SET) {
			p.nextToken()
		}

		// .foo
		if !p.curTokenIs(token.BRANCH) {
			p.wrongTypeParseError(".Branch")
			return nil
		}

		branch := p.curToken.Literal
		p.nextToken()

		if !p.curTokenIs(token.ASSIGN) {
			p.curError(token.ASSIGN)
			return nil
		}
		p.nextToken()

		branchValue, ok := p.parseValueExpr()
		if !ok {
			return nil
		}

		override := resonator.BranchValue{Path: branch, Value: branchValue}
		overrides = append(overrides, override)

		if p.peekTokenIs(token.COMMA) {
			p.nextToken()
		}
		if !p.peekTokenIs(token.WITH) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) && !p.peekTokenIs(token.EMIT) {
			p.nextToken()
		}
	}

	setPost := &resonator.Set{Overrides: overrides}
	phrase := &resonator.Phrase{Type: resonator.SETPOST, SetPost: setPost}

	return phrase
}

// parse tokens for EMIT Phrase (EMIT .foo = "bar", .moo = [1, 2])
//
// Same grammar as SET — comma-separated <branch> = <literal> pairs —
// but folded onto Resonator.Emit and applied with overwrite semantics
// to the rule's own response (after EXEC) before the per-scope merge.
// Stops on the next top-level clause keyword (WITH / PRIORITY / EXEC).
func (p *Parser) parseEmitPhrase() *resonator.Phrase {
	overrides := make([]resonator.BranchValue, 0)

	for !p.curTokenIs(token.EOF) && !p.peekTokenIs(token.SET) && !p.peekTokenIs(token.SELECT) && !p.peekTokenIs(token.WITH) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) {

		if p.curTokenIs(token.COMMA) || p.curTokenIs(token.EMIT) {
			p.nextToken()
		}

		if !p.curTokenIs(token.BRANCH) {
			p.wrongTypeParseError(".branch")
			return nil
		}

		branch := p.curToken.Literal
		p.nextToken()

		if !p.curTokenIs(token.ASSIGN) {
			p.curError(token.ASSIGN)
			return nil
		}
		p.nextToken()

		branchValue, ok := p.parseValueExpr()
		if !ok {
			return nil
		}

		overrides = append(overrides, resonator.BranchValue{Path: branch, Value: branchValue})

		if p.peekTokenIs(token.COMMA) {
			p.nextToken()
		}
		if !p.peekTokenIs(token.SET) && !p.peekTokenIs(token.SELECT) && !p.peekTokenIs(token.WITH) && !p.peekTokenIs(token.PRIORITY) && !p.peekTokenIs(token.EXEC) {
			p.nextToken()
		}
	}

	return &resonator.Phrase{
		Type: resonator.EMIT,
		Emit: &resonator.Set{Overrides: overrides},
	}
}

// parse tokens for PRIORITY Phrase (PRIORITY [INT])
func (p *Parser) parsePriorityPhrase() *resonator.Phrase {
	p.nextToken()

	priority, err := strconv.ParseInt(p.curToken.Literal, 0, 64)
	if err != nil {
		msg := fmt.Sprintf("could not parse priority %q as integer", p.curToken.Literal)
		p.errors = append(p.errors, msg)
		return nil
	}
	phrase := &resonator.Phrase{Type: resonator.PRIORITY, Priority: priority}
	return phrase
}

// parse tokens for EXEC Phrase (EXEC "string")
func (p *Parser) parseExecPhrase() *resonator.Phrase {
	var execname string

	if !p.peekTokenIs(token.STRING) {
		msg := "exec missing execname"
		p.errors = append(p.errors, msg)
		return nil
	}
	p.nextToken()
	execname = p.curToken.Literal

	phrase := &resonator.Phrase{Type: resonator.EXEC, Exec: execname}
	return phrase
}

// parseSelectPhrase parses one or more SELECT assignments of the form
//
//	SELECT <src-path> AS <dst-path> [DEFAULT <literal>]
//	      [, <src-path> AS <dst-path> [DEFAULT <literal>]]*
//
// Source / destination paths use the same `.foo` / `@foo` sugar as
// WHEN and SET; both lex to BRANCH tokens. DEFAULT's RHS is any
// literal accepted by parseLiteralValue (scalar or array).
func (p *Parser) parseSelectPhrase() *resonator.Phrase {
	assignments := make([]resonator.SelectAssignment, 0)

	for {
		// Advance to the next source path. First iteration moves past
		// the SELECT keyword; subsequent iterations move past the
		// comma consumed at the tail of the previous one.
		p.nextToken()
		if !p.curTokenIs(token.BRANCH) {
			p.errors = append(p.errors, "SELECT expected a source path (e.g. `.foo` or `@web.req.…`)")
			return nil
		}
		asn := resonator.SelectAssignment{From: p.curToken.Literal}

		if !p.peekTokenIs(token.AS) {
			p.errors = append(p.errors,
				"SELECT expected `AS <dest>` after the source path")
			return nil
		}
		p.nextToken() // onto AS
		if !p.peekTokenIs(token.BRANCH) {
			p.errors = append(p.errors,
				"SELECT expected a destination path after `AS`")
			return nil
		}
		p.nextToken() // onto destination BRANCH
		asn.To = p.curToken.Literal

		// Optional DEFAULT <literal>.
		if p.peekTokenIs(token.DEFAULT) {
			p.nextToken() // onto DEFAULT
			p.nextToken() // onto the literal
			v, ok := p.parseValueExpr()
			if !ok {
				return nil
			}
			asn.Default = v
			asn.HasDefault = true
		}

		assignments = append(assignments, asn)

		// More assignments?
		if !p.peekTokenIs(token.COMMA) {
			break
		}
		p.nextToken() // consume comma; loop advances past it
	}

	phrase := &resonator.Phrase{
		Type:   resonator.SELECT,
		Select: &resonator.Select{Assignments: assignments},
	}
	p.SeenSelect(true)
	return phrase
}

// parseWhenPhrase parses a WHEN clause into a boolean expression tree.
//
//	WHEN *
//	WHEN .branch == "value"
//	WHEN .branch.subbranch != true
//	WHEN .branch.subbranch > 90, .branch == "thing"     // comma = &&
//	WHEN .a == 1 && (.b == 2 || .c == 3)
//	WHEN !(._txc.src == "http")
//
// Precedence (low → high): `||`  <  `&&` (and `,`)  <  `!`  <  primary.
// Primary is either a parenthesized sub-expression or a single leaf
// (`<branch> <cmp> <scalar>`). Comma binds at `&&` precedence so legacy
// flat-AND rules keep parsing unchanged.
//
// Postcondition: curToken sits on the last token of the WHEN expression
// (the rightmost scalar or `)`). The outer ParseEvent loop calls
// nextToken once on return.
func (p *Parser) parseWhenPhrase() *resonator.Phrase {
	// WHEN *
	if p.peekTokenIs(token.STAR) {
		p.nextToken()
		return &resonator.Phrase{Type: resonator.WHEN,
			When: &resonator.When{Expr: &resonator.WhenExpr{Star: true}}}
	}

	// WHEN immediately followed by a clause-stop (or EOF) — empty WHEN,
	// matches everything. Mirrors the old parser's silent-empty path.
	if p.isStopForWhen(p.peekToken.Type) {
		return &resonator.Phrase{Type: resonator.WHEN,
			When: &resonator.When{Expr: &resonator.WhenExpr{Star: true}}}
	}

	// Step from WHEN onto the first token of the expression.
	p.nextToken()
	expr := p.parseWhenOr()
	if expr == nil {
		return nil
	}
	return &resonator.Phrase{Type: resonator.WHEN,
		When: &resonator.When{Expr: expr}}
}

// parseWhenOr = parseWhenAnd ( "||" parseWhenAnd )*
func (p *Parser) parseWhenOr() *resonator.WhenExpr {
	first := p.parseWhenAnd()
	if first == nil {
		return nil
	}
	parts := []resonator.WhenExpr{*first}
	for p.peekTokenIs(token.LOR) {
		p.nextToken() // curToken = ||
		if !p.isWhenPrimaryStart(p.peekToken.Type) {
			p.errors = append(p.errors, "WHEN expected expression after '||'")
			return nil
		}
		p.nextToken() // curToken = first token of right operand
		right := p.parseWhenAnd()
		if right == nil {
			return nil
		}
		parts = append(parts, *right)
	}
	if len(parts) == 1 {
		return first
	}
	return &resonator.WhenExpr{Or: parts}
}

// parseWhenAnd = parseWhenNot ( ("&&" | ",") parseWhenNot )*
func (p *Parser) parseWhenAnd() *resonator.WhenExpr {
	first := p.parseWhenNot()
	if first == nil {
		return nil
	}
	parts := []resonator.WhenExpr{*first}
	for p.peekTokenIs(token.LAND) || p.peekTokenIs(token.COMMA) {
		opLit := p.peekToken.Literal
		p.nextToken() // curToken = && or ,
		if !p.isWhenPrimaryStart(p.peekToken.Type) {
			p.errors = append(p.errors,
				fmt.Sprintf("WHEN expected expression after '%s'", opLit))
			return nil
		}
		p.nextToken() // curToken = first token of right operand
		right := p.parseWhenNot()
		if right == nil {
			return nil
		}
		parts = append(parts, *right)
	}
	if len(parts) == 1 {
		return first
	}
	return &resonator.WhenExpr{And: parts}
}

// parseWhenNot = "!" parseWhenNot | parseWhenPrimary
func (p *Parser) parseWhenNot() *resonator.WhenExpr {
	if p.curTokenIs(token.BANG) {
		if !p.isWhenPrimaryStart(p.peekToken.Type) {
			p.errors = append(p.errors, "WHEN expected expression after '!'")
			return nil
		}
		p.nextToken()
		inner := p.parseWhenNot()
		if inner == nil {
			return nil
		}
		return &resonator.WhenExpr{Not: inner}
	}
	return p.parseWhenPrimary()
}

// parseWhenPrimary = "(" parseWhenOr ")" | parseWhenLeaf
func (p *Parser) parseWhenPrimary() *resonator.WhenExpr {
	if p.curTokenIs(token.LPAREN) {
		if p.peekTokenIs(token.RPAREN) {
			p.errors = append(p.errors, "WHEN expected expression inside '()'")
			return nil
		}
		p.nextToken() // step past (
		inner := p.parseWhenOr()
		if inner == nil {
			return nil
		}
		if !p.peekTokenIs(token.RPAREN) {
			p.errors = append(p.errors, "WHEN expected ')' to close group")
			return nil
		}
		p.nextToken() // consume )
		return inner
	}
	return p.parseWhenLeaf()
}

// parseWhenLeaf = <branch> <cmp> <scalar>. The body is the original
// leaf-parsing logic lifted unchanged from the flat-AND parseWhenPhrase.
func (p *Parser) parseWhenLeaf() *resonator.WhenExpr {
	if !p.curTokenIs(token.BRANCH) {
		msg := fmt.Sprintf("WHEN expected branch or '(', got '%s'",
			p.curToken.Literal)
		p.errors = append(p.errors, msg)
		return nil
	}
	branch := &resonator.Branch{Path: p.curToken.Literal}
	p.nextToken()

	if !p.curTokenIsComparison() {
		msg := fmt.Sprintf("WHEN expected =~, !~, ==, !=, >, <, >=, or <= (%s)",
			p.curToken.Type)
		p.errors = append(p.errors, msg)
		return nil
	}

	var matchtype resonator.MatchType
	switch p.curToken.Literal {
	case "==":
		matchtype = resonator.MatchType("eq")
	case ">=":
		matchtype = resonator.MatchType("gteq")
	case "=~":
		matchtype = resonator.MatchType("=~")
	case "!~":
		matchtype = resonator.MatchType("!~")
	case "<=":
		matchtype = resonator.MatchType("lteq")
	case "!=":
		matchtype = resonator.MatchType("ne")
	case ">":
		matchtype = resonator.MatchType("gt")
	case "<":
		matchtype = resonator.MatchType("lt")
	}

	p.nextToken()

	if !p.curTokenIs(token.TRUE) && !p.curTokenIs(token.FALSE) && !p.curTokenIs(token.FLOAT) && !p.curTokenIs(token.INT) && !p.curTokenIs(token.STRING) && !p.curTokenIs(token.NULL) && !p.curTokenIs(token.REGEX) {
		p.wrongTypeParseError("bool, int, float, string, null, or regex")
		return nil
	}

	var matchValue interface{}
	switch p.curToken.Type {
	case token.INT:
		mv, _ := strconv.ParseInt(p.curToken.Literal, 0, 64)
		matchValue = mv
	case token.FLOAT:
		mv, _ := strconv.ParseFloat(p.curToken.Literal, 64)
		matchValue = mv
	case token.TRUE, token.FALSE:
		mv, _ := strconv.ParseBool(p.curToken.Literal)
		matchValue = mv
	case token.NULL:
		matchValue = ""
	default:
		matchValue = p.curToken.Literal // string
	}

	leaf := resonator.Condition{
		Branch:     branch,
		MatchValue: matchValue,
		MatchType:  matchtype,
	}
	return &resonator.WhenExpr{Leaf: leaf, HasLeaf: true}
}

func (p *Parser) isWhenPrimaryStart(t token.TokenType) bool {
	switch t {
	case token.BRANCH, token.LPAREN, token.BANG:
		return true
	}
	return false
}

func (p *Parser) isStopForWhen(t token.TokenType) bool {
	switch t {
	case token.EOF, token.SET, token.SELECT, token.WITH,
		token.PRIORITY, token.EXEC, token.EMIT:
		return true
	}
	return false
}

func (p *Parser) curTokenIsComparison() bool {
	switch p.curToken.Type {
	case token.GT, token.LT, token.LT_EQ, token.GT_EQ, token.EQ, token.NOT_EQ, token.MATCH, token.NO_MATCH:
		return true
	default:
		return false
	}
}
