package lexer_test

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

func TestNoInput(t *testing.T) {
	input := ``
	l := lexer.New(input)

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.EOF, ""},
	}

	for _, tt := range tests {
		tok := l.NextToken()
    test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

func TestWithComments(t *testing.T) {
	input := `# this is a test with comments
	SELECT @x AS .y # at the end of the line
	# and done
	`
	l := lexer.New(input)

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.SELECT, "SELECT"},
		// `@x` is sugar — the lexer normalizes to `._txc.x`.
		{token.BRANCH, "._txc.x"},
		{token.AS, "AS"},
		{token.BRANCH, ".y"},
		{token.EOF, ""},
	}

	for _, tt := range tests {
		tok := l.NextToken()
    test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

func TestQuoted(t *testing.T) {
	input := `WHEN .a == "inside: \"quoted\""`
	l := lexer.New(input)

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.WHEN, "WHEN"},
		{token.BRANCH, ".a"},
		{token.EQ, "=="},
		{token.STRING, `inside: "quoted"`},
		{token.EOF, ""},
	}

	for _, tt := range tests {
		tok := l.NextToken()
    test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

func TestNegativeNumber(t *testing.T) {
	// (The trailing `"` this input used to carry was unintentional —
	// the previous lexer bug silently swallowed it. Removed when
	// the `-` case stopped over-advancing; see
	// TestNegativeNumberFollowedByPunctuation.)
	input := `WHEN .a == -1 -1.1`
	l := lexer.New(input)

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.WHEN, "WHEN"},
		{token.BRANCH, ".a"},
		{token.EQ, "=="},
		{token.INT, `-1`},
		{token.FLOAT, `-1.1`},
		{token.EOF, ""},
	}

	for _, tt := range tests {
		tok := l.NextToken()
    test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

// TestNegativeNumberFollowedByPunctuation covers the previously-
// buggy case where the `-` branch in NextToken fell through to the
// trailing readChar() — that swallowed whatever punctuation came
// next (e.g. the `,` in `&object("k", -1, "v")`). The earlier
// TestNegativeNumber didn't catch it because its inputs had
// whitespace between the negative number and the next token, and
// skipWhitespace at the next NextToken call hid the symptom.
func TestNegativeNumberFollowedByPunctuation(t *testing.T) {
	input := `[-32601,"x"]`
	l := lexer.New(input)
	tests := []struct {
		ty  token.TokenType
		lit string
	}{
		{token.LBRACKET, "["},
		{token.INT, "-32601"},
		{token.COMMA, ","}, // pre-fix this was swallowed
		{token.STRING, "x"},
		{token.RBRACKET, "]"},
		{token.EOF, ""},
	}
	for _, tt := range tests {
		tok := l.NextToken()
		test.Equals(t, tt.ty, tok.Type)
		test.Equals(t, tt.lit, tok.Literal)
	}
}

func TestRegex(t *testing.T) {
	input := `WHEN .a =~ /^th\/ing/ !~ /aa(b)/ /`
	l := lexer.New(input)

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.WHEN, "WHEN"},
		{token.BRANCH, ".a"},
		{token.MATCH, "=~"},
		{token.REGEX, `^th/ing`},
		{token.NO_MATCH, "!~"},
		{token.REGEX, `aa(b)`},
		{token.SLASH, "/"},
		{token.EOF, ""},
	}

	for _, tt := range tests {
		tok := l.NextToken()
    test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

func TestEndBranch(t *testing.T) {
	input := `.t`
	l := lexer.New(input)

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
    {token.BRANCH, ".t"},
		{token.EOF, ""},
	}

	for _, tt := range tests {
		tok := l.NextToken()
    test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

func TestNextToken(t *testing.T) {
	input := `WHEN EXEC PRIORITY WITH .a = "moo" +(){},;-!/* SELECT * < > <= >= : [] != $ 5.5`

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.WHEN, "WHEN"},
		{token.EXEC, "EXEC"},
		{token.PRIORITY, "PRIORITY"},
		{token.WITH, "WITH"},
		{token.BRANCH, ".a"},
		{token.ASSIGN, "="},
		{token.STRING, "moo"},
		{token.PLUS, "+"},
		{token.LPAREN, "("},
		{token.RPAREN, ")"},
		{token.LBRACE, "{"},
		{token.RBRACE, "}"},
		{token.COMMA, ","},
		{token.SEMICOLON, ";"},
		{token.MINUS, "-"},
		{token.BANG, "!"},
		{token.SLASH, "/"},
		{token.ASTERISK, "*"},
		{token.SELECT, "SELECT"},
		// `*` after SELECT is now ASTERISK (legacy `SELECT *`
		// branch-filter projection has been removed; only WHEN
		// keeps the STAR carve-out).
		{token.ASTERISK, "*"},
		{token.LT, "<"},
		{token.GT, ">"},
		{token.LT_EQ, "<="},
		{token.GT_EQ, ">="},
		{token.COLON, ":"},
		{token.LBRACKET, "["},
		{token.RBRACKET, "]"},
		{token.NOT_EQ, "!="},
		{token.ILLEGAL, "$"},
		{token.FLOAT, "5.5"},
		{token.EOF, ""},
	}

	l := lexer.New(input)

	for _, tt := range tests {
		tok := l.NextToken()
		test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

// TestLogicalOperators locks in the symbolic tokens added for WHEN
// grammar v2. `&&` → LAND, `||` → LOR, standalone `!` → BANG (the
// existing `!=`/`!~` peek paths are unchanged). A single `&` or `|`
// has no use today and falls into ILLEGAL.
func TestLogicalOperators(t *testing.T) {
	input := `&& || ! .a & |`

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.LAND, "&&"},
		{token.LOR, "||"},
		{token.BANG, "!"},
		{token.BRANCH, ".a"},
		{token.ILLEGAL, "&"},
		{token.ILLEGAL, "|"},
		{token.EOF, ""},
	}

	l := lexer.New(input)
	for _, tt := range tests {
		tok := l.NextToken()
		test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

// TestAmpIdent covers the `&fn` function-call sigil. The lexer
// produces AMP_IDENT with the identifier as its literal (no leading
// `&`). Bare `&` (followed by non-letter, non-`&`) stays ILLEGAL, so
// the existing `TestLogicalOperators` coverage is preserved.
func TestAmpIdent(t *testing.T) {
	// Mix function calls with literal and path syntax so we can see
	// they interleave cleanly.
	input := `&uuid() &now() &b64decode(@web.req.body) SET .x = &concat("a", .b)`

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.AMP_IDENT, "uuid"},
		{token.LPAREN, "("},
		{token.RPAREN, ")"},
		{token.AMP_IDENT, "now"},
		{token.LPAREN, "("},
		{token.RPAREN, ")"},
		{token.AMP_IDENT, "b64decode"},
		{token.LPAREN, "("},
		// `@web.req.body` becomes `._txc.web.req.body` via lexer sugar.
		{token.BRANCH, "._txc.web.req.body"},
		{token.RPAREN, ")"},
		{token.SET, "SET"},
		{token.BRANCH, ".x"},
		{token.ASSIGN, "="},
		{token.AMP_IDENT, "concat"},
		{token.LPAREN, "("},
		{token.STRING, "a"},
		{token.COMMA, ","},
		{token.BRANCH, ".b"},
		{token.RPAREN, ")"},
		{token.EOF, ""},
	}

	l := lexer.New(input)
	for _, tt := range tests {
		tok := l.NextToken()
		test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

// TestAtBranch locks in the `@foo` shorthand for `._txc.foo`. The lexer
// rewrites the path at token-emission time, so downstream still sees a
// normal BRANCH token.
func TestAtBranch(t *testing.T) {
	input := `@foo @a.b.c WHEN @web.req.url.path != "/"`

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.BRANCH, "._txc.foo"},
		{token.BRANCH, "._txc.a.b.c"},
		{token.WHEN, "WHEN"},
		{token.BRANCH, "._txc.web.req.url.path"},
		{token.NOT_EQ, "!="},
		{token.STRING, "/"},
		{token.EOF, ""},
	}

	l := lexer.New(input)
	for _, tt := range tests {
		tok := l.NextToken()
		test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

// TestHyphenBranch locks in `-` as a mid-run branch char so hyphenated
// keys (HTTP header names) are addressable without quoting, through
// both the `.`-led and `@`-sugar forms.
func TestHyphenBranch(t *testing.T) {
	input := `.headers.content-type.0 @web.res.headers.content-type.0 WHEN .x-y == 5`

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.BRANCH, ".headers.content-type.0"},
		{token.BRANCH, "._txc.web.res.headers.content-type.0"},
		{token.WHEN, "WHEN"},
		{token.BRANCH, ".x-y"},
		{token.EQ, "=="},
		{token.INT, "5"},
		{token.EOF, ""},
	}

	l := lexer.New(input)
	for _, tt := range tests {
		tok := l.NextToken()
		test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}

// TestQuotedBranchSegment covers `."quoted"` segments. A plain key
// passes through verbatim; a key containing a gjson/sjson metacharacter
// (`.`) is backslash-escaped so it stays one path segment. Quoted
// segments compose with bare runs and with the `@` sugar, and may end a
// path or sit mid-path.
func TestQuotedBranchSegment(t *testing.T) {
	cases := []struct {
		input   string
		literal string
	}{
		// plain quoted key, mid-path, followed by an array index
		{`.headers."content-type".0`, `.headers.content-type.0`},
		// quoted key terminating the path
		{`.headers."content-type"`, `.headers.content-type`},
		// dot inside the key must be escaped for gjson/sjson
		{`.a."b.c".d`, `.a.b\.c.d`},
		// @ sugar + quoted segment
		{`@web.res.headers."content-type".0`, `._txc.web.res.headers.content-type.0`},
		// space inside a key
		{`.a."first name"`, `.a.first name`},
	}
	for _, c := range cases {
		l := lexer.New(c.input)
		tok := l.NextToken()
		test.Equals(t, token.TokenType(token.BRANCH), tok.Type)
		test.Equals(t, c.literal, tok.Literal)
		eof := l.NextToken()
		test.Equals(t, token.TokenType(token.EOF), eof.Type)
	}
}

// TestAtBranchErrors covers the malformed cases: bare `@`, `@.foo` (would
// be ambiguous with `._txc.foo`), `@1`, and `@@foo`. All emit ILLEGAL on
// the offending byte.
func TestAtBranchErrors(t *testing.T) {
	cases := []struct {
		input string
		first token.Token
	}{
		{`@`, token.Token{Type: token.ILLEGAL, Literal: "@"}},
		{`@.foo`, token.Token{Type: token.ILLEGAL, Literal: "@"}},
		{`@1`, token.Token{Type: token.ILLEGAL, Literal: "@"}},
		{`@@foo`, token.Token{Type: token.ILLEGAL, Literal: "@"}},
	}
	for _, c := range cases {
		l := lexer.New(c.input)
		tok := l.NextToken()
		test.Equals(t, c.first.Type, tok.Type)
		test.Equals(t, c.first.Literal, tok.Literal)
	}
}

// TestB64Literal locks in the `b64"..."` typed-string sugar. The lexer
// applies normal string-escape handling first, then base64-encodes the
// resulting bytes, so `b64"not found\n"` produces the encoding of
// `not found` + a real `0x0a` byte.
func TestB64Literal(t *testing.T) {
	cases := []struct {
		input   string
		literal string
	}{
		{`b64"not found"`, "bm90IGZvdW5k"},
		{`b64""`, ""},
		{`b64"with \"quotes\""`, "d2l0aCAicXVvdGVzIg=="},
		{`b64"not found\n"`, "bm90IGZvdW5kCg=="},
	}
	for _, c := range cases {
		l := lexer.New(c.input)
		tok := l.NextToken()
		test.Equals(t, token.TokenType(token.STRING), tok.Type)
		test.Equals(t, c.literal, tok.Literal)
		eof := l.NextToken()
		test.Equals(t, token.TokenType(token.EOF), eof.Type)
	}
}

// TestB64NotALiteral confirms that `b64` is only special when *immediately*
// followed by `"`. Whitespace or any other adjacent character breaks the
// rule and leaves `b64` as an ordinary identifier.
func TestB64NotALiteral(t *testing.T) {
	cases := []struct {
		input  string
		tokens []token.Token
	}{
		{`b64 "spaced"`, []token.Token{
			{Type: token.IDENT, Literal: "b64"},
			{Type: token.STRING, Literal: "spaced"},
			{Type: token.EOF, Literal: ""},
		}},
		{`xb64"sandwich"`, []token.Token{
			{Type: token.IDENT, Literal: "xb64"},
			{Type: token.STRING, Literal: "sandwich"},
			{Type: token.EOF, Literal: ""},
		}},
		{`b64x"sandwich"`, []token.Token{
			{Type: token.IDENT, Literal: "b64x"},
			{Type: token.STRING, Literal: "sandwich"},
			{Type: token.EOF, Literal: ""},
		}},
	}
	for _, c := range cases {
		l := lexer.New(c.input)
		for _, want := range c.tokens {
			tok := l.NextToken()
			test.Equals(t, want.Type, tok.Type)
			test.Equals(t, want.Literal, tok.Literal)
		}
	}
}

// TestStringEscapes locks in the expanded escape set for regular string
// literals (independent of `b64`). The reader previously only unescaped
// `\"`; it now also handles `\\`, `\n`, `\r`, `\t`. Unknown escapes
// preserve both bytes verbatim.
func TestStringEscapes(t *testing.T) {
	cases := []struct {
		input   string
		literal string
	}{
		{`"\n"`, "\n"},
		{`"a\tb"`, "a\tb"},
		{`"\\"`, "\\"},
		{`"unknown \z escape"`, `unknown \z escape`},
		{`"mix \n\t\r\\\""`, "mix \n\t\r\\\""},
	}
	for _, c := range cases {
		l := lexer.New(c.input)
		tok := l.NextToken()
		test.Equals(t, token.TokenType(token.STRING), tok.Type)
		test.Equals(t, c.literal, tok.Literal)
	}
}

func TestInput(t *testing.T) {
	input := `WHEN .foo > 123 SELECT .web, .thing, .another.thing, .moo SET .b = 9, .multi.level = "apple" PRIORITY 100 WITH moo = 1 EXEC "hello-world"`

	tests := []struct {
		expectedType    token.TokenType
		expectedLiteral string
	}{
		{token.WHEN, "WHEN"},
		{token.BRANCH, ".foo"},
		{token.GT, ">"},
		{token.INT, "123"},
		{token.SELECT, "SELECT"},
		{token.BRANCH, ".web"},
		{token.COMMA, ","},
		{token.BRANCH, ".thing"},
		{token.COMMA, ","},
		{token.BRANCH, ".another.thing"},
		{token.COMMA, ","},
		{token.BRANCH, ".moo"},
		{token.SET, "SET"},
		{token.BRANCH, ".b"},
		{token.ASSIGN, "="},
		{token.INT, "9"},
		{token.COMMA, ","},
		{token.BRANCH, ".multi.level"},
		{token.ASSIGN, "="},
		{token.STRING, "apple"},
		{token.PRIORITY, "PRIORITY"},
		{token.INT, "100"},
		{token.WITH, "WITH"},
		{token.IDENT, "moo"},
		{token.ASSIGN, "="},
		{token.INT, "1"},
		{token.EXEC, "EXEC"},
		{token.STRING, "hello-world"},
		{token.EOF, ""},
	}

	l := lexer.New(input)

	for _, tt := range tests {
		tok := l.NextToken()
		test.Equals(t, tt.expectedType, tok.Type)
		test.Equals(t, tt.expectedLiteral, tok.Literal)
	}
}
