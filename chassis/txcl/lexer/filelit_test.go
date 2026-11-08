package lexer_test

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

// `$[path]` is one FILELIT token whose literal is the path, so the CLI's
// include pass can find and splice it by span, and so a `$[` inside a
// string, a regex or a comment is never a directive.
func TestFileLiteral(t *testing.T) {
	input := `WHEN .x in $[inspect-nouns.json] && .y == "$[not.json]" && .z =~ /\$\[re\]/
WITH schema = $[schemas/reply.json], up = $[../shared/x.json] # $[comment.json]
SET .a = [$[a.json], 1], .d = $`
	l := lexer.New(input)
	want := []struct {
		typ token.TokenType
		lit string
	}{
		{token.WHEN, "WHEN"}, {token.BRANCH, ".x"}, {token.IDENT, "in"}, {token.FILELIT, "inspect-nouns.json"},
		{token.LAND, "&&"}, {token.BRANCH, ".y"}, {token.EQ, "=="}, {token.STRING, "$[not.json]"},
		{token.LAND, "&&"}, {token.BRANCH, ".z"}, {token.MATCH, "=~"}, {token.REGEX, `\$\[re\]`},
		{token.WITH, "WITH"}, {token.IDENT, "schema"}, {token.ASSIGN, "="}, {token.FILELIT, "schemas/reply.json"}, {token.COMMA, ","},
		{token.IDENT, "up"}, {token.ASSIGN, "="}, {token.FILELIT, "../shared/x.json"},
		{token.SET, "SET"}, {token.BRANCH, ".a"}, {token.ASSIGN, "="}, {token.LBRACKET, "["}, {token.FILELIT, "a.json"}, {token.COMMA, ","}, {token.INT, "1"}, {token.RBRACKET, "]"}, {token.COMMA, ","},
		{token.BRANCH, ".d"}, {token.ASSIGN, "="}, {token.ILLEGAL, "$"},
		{token.EOF, ""},
	}
	for _, w := range want {
		tok := l.NextToken()
		test.Equals(t, w.typ, tok.Type)
		test.Equals(t, w.lit, tok.Literal)
	}
	test.Equals(t, 0, len(l.Errors()))
}

// The span of a FILELIT covers `$[` through `]`, like a STRING's covers
// its quotes.
func TestFileLiteralSpan(t *testing.T) {
	input := "SET .o = $[schemas/reply.json], .n = 1"
	l := lexer.New(input)
	var got []string
	for {
		tok := l.NextToken()
		if tok.Type == token.EOF {
			break
		}
		s, e := l.Span()
		got = append(got, input[s:e])
	}
	test.Equals(t, []string{"SET", ".o", "=", "$[schemas/reply.json]", ",", ".n", "=", "1"}, got)
}

// An unterminated literal ends at the line (or input) and is a lexical
// error strict callers see; the token still comes out so lenient parsing
// goes on.
func TestFileLiteralUnterminated(t *testing.T) {
	for _, tc := range []struct {
		input string
		lit   string
		next  token.TokenType
	}{
		{"SET .o = $[schemas/reply.json\nEMIT .x = 1", "schemas/reply.json", token.EMIT},
		{"SET .o = $[x", "x", token.EOF},
		{"SET .o = $[", "", token.EOF},
	} {
		l := lexer.New(tc.input)
		var toks []token.Token
		for {
			tok := l.NextToken()
			toks = append(toks, tok)
			if tok.Type == token.EOF {
				break
			}
		}
		test.Equals(t, token.TokenType(token.FILELIT), toks[3].Type)
		test.Equals(t, tc.lit, toks[3].Literal)
		test.Equals(t, tc.next, toks[4].Type)
		test.Equals(t, []string{"unterminated $[...] file literal"}, l.Errors())
	}
}
