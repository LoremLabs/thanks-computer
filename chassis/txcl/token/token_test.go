package token_test

import (
	"testing"

	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
	"github.com/loremlabs/thanks-computer/chassis/utils/test"
)

func TestConst(t *testing.T) {

	const (
		ILLEGAL  = "ILLEGAL"
		EOF      = "EOF"
		IDENT    = "IDENT"
		INT      = "INT"
		STRING   = "STRING"
		NULL     = "NULL"
		ASSIGN   = "="
		PLUS     = "+"
		MINUS    = "-"
		BANG     = "!"
		ASTERISK = "*"
		SLASH    = "/"
		CONCAT   = "."
		FLOAT    = "FLOAT"
		LT       = "<"
		LT_EQ    = "<="
		GT       = ">"
		GT_EQ    = ">="
		EQ       = "=="
		NOT_EQ   = "!="
		COMMA    = ","
		LPAREN   = "("
		RPAREN   = ")"
		LBRACE   = "{"
		RBRACE   = "}"
		LBRACKET = "["
		RBRACKET = "]"
		REGEX    = "REGEX"
		MATCH    = "=~"
		NO_MATCH = "!~"
		LAND     = "&&"
		LOR      = "||"
		SET      = "SET"
		TRUE     = "TRUE"
		FALSE    = "FALSE"
		WHEN     = "WHEN"
		SELECT   = "SELECT"
		WITH     = "WITH"
		PRIORITY = "PRIORITY"
		EXEC     = "EXEC"
		BRANCH   = "BRANCH"
		STAR     = "STAR"
		NEGATIVE = "-"
	)

	test.Equals(t, EXEC, token.EXEC)
	test.Equals(t, PRIORITY, token.PRIORITY)
	test.Equals(t, WITH, token.WITH)
	test.Equals(t, SELECT, token.SELECT)
	test.Equals(t, WHEN, token.WHEN)
	test.Equals(t, FALSE, token.FALSE)
	test.Equals(t, TRUE, token.TRUE)
	test.Equals(t, SET, token.SET)
	test.Equals(t, RBRACKET, token.RBRACKET)
	test.Equals(t, LBRACKET, token.LBRACKET)
	test.Equals(t, COMMA, token.COMMA)
	test.Equals(t, REGEX, token.REGEX)
	test.Equals(t, MATCH, token.MATCH)
	test.Equals(t, NO_MATCH, token.NO_MATCH)
	test.Equals(t, LAND, token.LAND)
	test.Equals(t, LOR, token.LOR)
	test.Equals(t, NOT_EQ, token.NOT_EQ)
	test.Equals(t, EQ, token.EQ)
	test.Equals(t, LT, token.LT)
	test.Equals(t, LT_EQ, token.LT_EQ)
	test.Equals(t, GT, token.GT)
	test.Equals(t, GT_EQ, token.GT_EQ)
	test.Equals(t, CONCAT, token.CONCAT)
	test.Equals(t, FLOAT, token.FLOAT)
	test.Equals(t, SLASH, token.SLASH)
	test.Equals(t, LBRACE, token.LBRACE)
	test.Equals(t, RBRACE, token.RBRACE)
	test.Equals(t, LPAREN, token.LPAREN)
	test.Equals(t, RPAREN, token.RPAREN)
	test.Equals(t, ASTERISK, token.ASTERISK)
	test.Equals(t, BANG, token.BANG)
	test.Equals(t, MINUS, token.MINUS)
	test.Equals(t, NEGATIVE, token.NEGATIVE)
	test.Equals(t, PLUS, token.PLUS)
	test.Equals(t, ASSIGN, token.ASSIGN)
	test.Equals(t, IDENT, token.IDENT)
	test.Equals(t, INT, token.INT)
	test.Equals(t, STRING, token.STRING)
	test.Equals(t, NULL, token.NULL)
	test.Equals(t, EOF, token.EOF)
	test.Equals(t, ILLEGAL, token.ILLEGAL)
	test.Equals(t, BRANCH, token.BRANCH)
	test.Equals(t, STAR, token.STAR)
}

func TestLookupIdent(t *testing.T) {
	var keywords = map[string]token.TokenType{
		"fn":       token.FUNCTION,
		"let":      token.LET,
		"true":     token.TRUE,
		"false":    token.FALSE,
		"if":       token.IF,
		"else":     token.ELSE,
		"return":   token.RETURN,
		"RETURN":   token.RETURN,
		"select":   token.SELECT,
		"SELECT":   token.SELECT,
		"with":     token.WITH,
		"WITH":     token.WITH,
		"when":     token.WHEN,
		"WHEN":     token.WHEN,
		"priority": token.PRIORITY,
		"PRIORITY": token.PRIORITY,
		"exec":     token.EXEC,
		"EXEC":     token.EXEC,
		"set":      token.SET,
		"SET":      token.SET,
		"null":     token.NULL,
		"NULL":     token.NULL,
		"abc":      token.IDENT,
	}

	for k := range keywords {
		test.Equals(t, keywords[k], token.LookupIdent(k))
	}
}
