package token

// https://interpreterbook.com/

type TokenType string

// WHEN * HAVING "wasm-name-here" SET .manual = true, .multi.level = {}, .string = "yup", .number = 7 SELECT * SET .number = 8 WITH timeout = 1000, stackName = "canary" PRIORITY 10 EXEC [$env . "-" . $meta[stackName] . "-hello"]

const (
	ILLEGAL = "ILLEGAL"
	EOF     = "EOF"

	// Identifiers + literals
	IDENT  = "IDENT"  // add, foobar, x, y, ...
	INT    = "INT"    // 1343456
	STRING = "STRING" // "foobar"
	NULL   = "NULL"   // null

	// Operators
	ASSIGN   = "="
	PLUS     = "+"
	MINUS    = "-"
	BANG     = "!"
	ASTERISK = "*"
	SLASH    = "/"
	CONCAT   = "."
	MATCH    = "=~"
	NO_MATCH = "!~"
	LAND     = "&&"
	LOR      = "||"

	// Number
	FLOAT    = "FLOAT"
	NEGATIVE = "-"

	// Comparisons
	LT     = "<"
	LT_EQ  = "<="
	GT     = ">"
	GT_EQ  = ">="
	EQ     = "=="
	NOT_EQ = "!="

	// Delimiters
	COMMA     = ","
	SEMICOLON = ";"
	COLON     = ":"

	LPAREN   = "("
	RPAREN   = ")"
	LBRACE   = "{"
	RBRACE   = "}"
	LBRACKET = "["
	RBRACKET = "]"

	// Keywords
	FUNCTION = "FUNCTION"
	LET      = "LET"
	SET      = "SET"
	TRUE     = "TRUE"
	FALSE    = "FALSE"
	IF       = "IF"
	ELSE     = "ELSE"
	RETURN   = "RETURN"
	WHEN     = "WHEN"
	SELECT   = "SELECT"
	AS       = "AS"
	DEFAULT  = "DEFAULT"
	WITH     = "WITH"
	PRIORITY = "PRIORITY"
	EXEC     = "EXEC"
	EMIT     = "EMIT"

	// Filtering
	BRANCH = "BRANCH"
	STAR   = "STAR"
	REGEX  = "REGEX"

	// Function-call sigil. `&fn(args...)` introduces a runtime
	// function-call value. Literal carries the identifier (no `&`).
	// See internal docs/todo-txcl-expressions.md.
	AMP_IDENT = "AMP_IDENT"
)

type Token struct {
	Type    TokenType
	Literal string
}

var keywords = map[string]TokenType{
	"fn":       FUNCTION,
	"let":      LET,
	"true":     TRUE,
	"false":    FALSE,
	"if":       IF,
	"else":     ELSE,
	"return":   RETURN,
	"RETURN":   RETURN,
	"select":   SELECT,
	"SELECT":   SELECT,
	"as":       AS,
	"AS":       AS,
	"default":  DEFAULT,
	"DEFAULT":  DEFAULT,
	"with":     WITH,
	"WITH":     WITH,
	"when":     WHEN,
	"WHEN":     WHEN,
	"priority": PRIORITY,
	"PRIORITY": PRIORITY,
	"exec":     EXEC,
	"EXEC":     EXEC,
	"emit":     EMIT,
	"EMIT":     EMIT,
	"set":      SET,
	"SET":      SET,
	"null":     NULL,
	"NULL":     NULL,
}

func LookupIdent(ident string) TokenType {
	if tok, ok := keywords[ident]; ok {
		return tok
	}
	return IDENT
}
