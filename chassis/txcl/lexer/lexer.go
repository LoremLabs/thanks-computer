package lexer

// https://interpreterbook.com/

import (
	//  "fmt"
	"encoding/base64"
	"strings"

	"github.com/loremlabs/thanks-computer/chassis/txcl/token"
)

type Lexer struct {
	input        string
	position     int         // current position in input (points to current char)
	readPosition int         // current reading position in input (after current char)
	ch           byte        // current char under examination
	LastToken    token.Token // last token for lookback

	// errs collects lexical errors (e.g. unterminated string/regex
	// literals). Recorded unconditionally as the input is scanned, but
	// only consulted by strict callers via Errors(); the lenient
	// runtime path (txcl.Resonator) never reads it, so accumulating
	// here changes no runtime behavior.
	errs []string
}

func New(input string) *Lexer {
	l := &Lexer{input: input}
	l.readChar()
	return l
}

// Errors returns lexical errors accumulated while scanning. Meaningful
// only after the input has been fully consumed (e.g. the parser drove
// NextToken to EOF). Used by txcl.Validate; ignored by the runtime.
func (l *Lexer) Errors() []string {
	return l.errs
}

func (l *Lexer) NextToken() token.Token {
	var tok token.Token

	l.skipWhitespace()

	//
	// skip single-line comments
	//
	if l.ch == '#' {
		l.skipComment()
		return (l.NextToken())
	}

	switch l.ch {
	case '=':
		switch l.peekChar() {
		case '=':
			ch := l.ch
			l.readChar()
			literal := string(ch) + string(l.ch)
			tok = token.Token{Type: token.EQ, Literal: literal}
		case '~':
			ch := l.ch
			l.readChar()
			literal := string(ch) + string(l.ch)
			tok = token.Token{Type: token.MATCH, Literal: literal}

		default:
			tok = newToken(token.ASSIGN, l.ch)
		}
	case '+':
		tok = newToken(token.PLUS, l.ch)
	case '-':
		if !isDigit(l.peekChar()) {
			tok = newToken(token.MINUS, l.ch)
		} else {
			tok.Literal = l.readNumber()
			if strings.Contains(tok.Literal, ".") {
				tok.Type = token.FLOAT
			} else {
				tok.Type = token.INT
			}

			l.LastToken = tok
			// readNumber already advanced l.ch past the number's
			// last digit. The trailing readChar() below would skip
			// one MORE character, swallowing whatever follows the
			// number (e.g. `,` in `-32601,...`). Return early —
			// same pattern the default-case isDigit branch uses
			// for unsigned numbers.
			return tok
		}
	case '!':
		switch l.peekChar() {
		case '=':
			ch := l.ch
			l.readChar()
			literal := string(ch) + string(l.ch)
			tok = token.Token{Type: token.NOT_EQ, Literal: literal}
		case '~':
			ch := l.ch
			l.readChar()
			literal := string(ch) + string(l.ch)
			tok = token.Token{Type: token.NO_MATCH, Literal: literal}
		default:
			tok = newToken(token.BANG, l.ch)
		}
	case '/':
		if isBinding(l.LastToken.Type) {
			tok.Type = token.REGEX
			tok.Literal = l.readRegex()
		} else {
			tok = newToken(token.SLASH, l.ch)
		}
	case '*':
		if l.LastToken.Type == token.WHEN {
			tok = newToken(token.STAR, l.ch)
		} else {
			tok = newToken(token.ASTERISK, l.ch)
		}
	case '<':
		if l.peekChar() == '=' {
			ch := l.ch
			l.readChar()
			literal := string(ch) + string(l.ch)
			tok = token.Token{Type: token.LT_EQ, Literal: literal}
		} else {
			tok = newToken(token.LT, l.ch)
		}
	case '>':
		if l.peekChar() == '=' {
			ch := l.ch
			l.readChar()
			literal := string(ch) + string(l.ch)
			tok = token.Token{Type: token.GT_EQ, Literal: literal}
		} else {
			tok = newToken(token.GT, l.ch)
		}
	case ';':
		tok = newToken(token.SEMICOLON, l.ch)
	case ':':
		tok = newToken(token.COLON, l.ch)
	case ',':
		tok = newToken(token.COMMA, l.ch)
	case '.':
		if isLetter(l.peekChar()) {
			tok.Type = token.BRANCH
			tok.Literal = l.readBranchString()
		}
	case '@':
		if isLetter(l.peekChar()) {
			tok.Type = token.BRANCH
			tok.Literal = l.readAtBranchString()
		} else {
			tok = newToken(token.ILLEGAL, l.ch)
		}
	case '{':
		tok = newToken(token.LBRACE, l.ch)
	case '}':
		tok = newToken(token.RBRACE, l.ch)
	case '(':
		tok = newToken(token.LPAREN, l.ch)
	case ')':
		tok = newToken(token.RPAREN, l.ch)
	case '"':
		tok.Type = token.STRING
		tok.Literal = l.readString()
	case '[':
		tok = newToken(token.LBRACKET, l.ch)
	case ']':
		tok = newToken(token.RBRACKET, l.ch)
	case '&':
		if l.peekChar() == '&' {
			ch := l.ch
			l.readChar()
			tok = token.Token{Type: token.LAND, Literal: string(ch) + string(l.ch)}
		} else if isLetter(l.peekChar()) {
			// `&fn` — function-call sigil. Step off `&` onto the
			// first identifier byte, read the ident, return early
			// because readIdentifier has already advanced past the
			// last ident byte (mirrors the default-case identifier
			// path below — both return without the trailing
			// readChar() the switch usually does).
			l.readChar()
			tok.Literal = l.readIdentifier()
			tok.Type = token.AMP_IDENT
			l.LastToken = tok
			return tok
		} else {
			tok = newToken(token.ILLEGAL, l.ch)
		}
	case '|':
		if l.peekChar() == '|' {
			ch := l.ch
			l.readChar()
			tok = token.Token{Type: token.LOR, Literal: string(ch) + string(l.ch)}
		} else {
			tok = newToken(token.ILLEGAL, l.ch)
		}
	case 0:
		tok.Literal = ""
		tok.Type = token.EOF
	default:
		if isLetter(l.ch) {
			tok.Literal = l.readIdentifier()
			if tok.Literal == "b64" && l.ch == '"' {
				raw := l.readString()
				tok.Type = token.STRING
				tok.Literal = base64.StdEncoding.EncodeToString([]byte(raw))
				l.readChar()
				l.LastToken = tok
				return tok
			}
			tok.Type = token.LookupIdent(tok.Literal)

			l.LastToken = tok

			return tok
		} else if isDigit(l.ch) {
			tok.Literal = l.readNumber()
			if strings.Contains(tok.Literal, ".") {
				tok.Type = token.FLOAT
			} else {
				tok.Type = token.INT
			}

			l.LastToken = tok

			return tok
		} else {
			tok = newToken(token.ILLEGAL, l.ch)
		}
	}

	l.readChar()
	l.LastToken = tok
	return tok
}

func (l *Lexer) skipWhitespace() {
	for l.ch == ' ' || l.ch == '\t' || l.ch == '\n' || l.ch == '\r' {
		l.readChar()
	}
}

// skip comment (until the end of the line).
func (l *Lexer) skipComment() {
	for l.ch != '\n' && l.ch != 0 {
		l.readChar()
	}
	l.skipWhitespace()
}

func (l *Lexer) readChar() {
	if l.readPosition >= len(l.input) {
		l.ch = 0
	} else {
		l.ch = l.input[l.readPosition]
	}
	l.position = l.readPosition
	l.readPosition += 1
}

func (l *Lexer) peekChar() byte {
	if l.readPosition >= len(l.input) {
		return 0
	} else {
		return l.input[l.readPosition]
	}
}

func (l *Lexer) readIdentifier() string {
	position := l.position
	// First byte already verified as a letter by the caller. Subsequent
	// bytes may be letters or digits — this is what lets typed-string
	// prefixes like `b64"..."` lex as a single identifier rather than
	// `b` + `64`.
	for isLetter(l.ch) || ('0' <= l.ch && l.ch <= '9') {
		l.readChar()
	}
	return l.input[position:l.position]
}

func (l *Lexer) readNumber() string {
	position := l.position
	// todo peek for -
	for isDigit(l.ch) {
		l.readChar()
	}
	return l.input[position:l.position]
}

// readBranchString reads a `.`-led branch path. Called with l.ch == '.'
// and peekChar() a letter. Bare runs use isBranchChar (letters, digits,
// '.', '-'); a `."quoted segment"` lets a key carry characters the bare
// run can't — notably a literal '.' (escaped for gjson/sjson) or a
// space. On return l.ch is the last byte of the branch (the closing '"'
// when the path ends on a quoted segment); NextToken's trailing
// readChar steps off it.
func (l *Lexer) readBranchString() string {
	var b strings.Builder
	b.WriteByte(l.ch) // leading '.'
	l.readChar()      // onto the first identifier byte
	l.scanBranchRun(&b)
	return b.String()
}

// readAtBranchString handles the `@foo.bar` sugar. The caller has already
// verified `peekChar()` is a letter (the start of the identifier after `@`).
// Returns `._txc.` + the branch run, so downstream sees an ordinary branch.
// Quoted segments are supported here too (`@web.res.headers."content-type".0`).
func (l *Lexer) readAtBranchString() string {
	var b strings.Builder
	b.WriteString("._txc.")
	l.readChar() // step off `@` onto the first identifier byte
	l.scanBranchRun(&b)
	return b.String()
}

// scanBranchRun appends the branch body to b. l.ch must be the first
// body byte on entry. Bare bytes are consumed while isBranchChar; a `"`
// in peek position opens a quoted segment whose content is appended
// (with gjson/sjson specials escaped) instead of the quote chars
// themselves. On return l.ch is the last consumed byte.
func (l *Lexer) scanBranchRun(b *strings.Builder) {
	b.WriteByte(l.ch)
	for {
		next := l.peekChar()
		if next == '"' {
			l.readChar() // onto the opening quote
			l.appendQuotedSegment(b)
			continue
		}
		if !isBranchChar(next) {
			break
		}
		l.readChar()
		b.WriteByte(l.ch)
	}
}

// appendQuotedSegment is called with l.ch == the opening '"'. It writes
// the quoted content into b, backslash-escaping the gjson/sjson path
// metacharacters (`\ . * ?`) so a key like "content-type" stays one
// segment and "user.email" doesn't split. On return l.ch is the closing
// '"' (or 0 if the quote was unterminated).
func (l *Lexer) appendQuotedSegment(b *strings.Builder) {
	for {
		l.readChar()
		if l.ch == 0 || l.ch == '"' {
			break
		}
		switch l.ch {
		case '\\', '.', '*', '?':
			b.WriteByte('\\')
			b.WriteByte(l.ch)
		default:
			b.WriteByte(l.ch)
		}
	}
}

func (l *Lexer) readRegex() string {
	position := l.position + 1
	for l.ch != 0 {
		l.readChar()
		// check for escapes
		if l.ch == '\\' && l.peekChar() == '/' {
			l.readChar()
			l.readChar()
		}
		if l.ch == '/' {
			break
		}
	}
	// Exited on EOF rather than a closing '/' → the literal never
	// closed. Strict validation surfaces this; runtime ignores it.
	if l.ch != '/' {
		l.errs = append(l.errs, "unterminated regex literal")
	}
	s := l.input[position:l.position]
	return strings.Replace(s, "\\/", "/", -1)
}

// readString reads a double-quoted string literal and returns the
// unescaped content. Caller is positioned on the opening `"`; on return,
// l.ch is on the closing `"` (or 0 if unterminated). Recognized escapes:
// `\"`, `\\`, `\n`, `\r`, `\t`. Any other `\x` sequence preserves both
// bytes verbatim, so authors don't lose content to unrecognized escapes.
func (l *Lexer) readString() string {
	var b strings.Builder
	for {
		l.readChar()
		if l.ch == 0 {
			// Hit EOF before a closing quote → unterminated. The
			// content read so far is still returned (lenient runtime
			// behavior is unchanged); strict validation reports it.
			l.errs = append(l.errs, "unterminated string literal")
			break
		}
		if l.ch == '"' {
			break
		}
		if l.ch == '\\' {
			l.readChar()
			switch l.ch {
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 0:
				return b.String()
			default:
				b.WriteByte('\\')
				b.WriteByte(l.ch)
			}
			continue
		}
		b.WriteByte(l.ch)
	}
	return b.String()
}

func isBranchChar(ch byte) bool {
	// '-' is allowed mid-run so hyphenated keys (HTTP header names like
	// content-type, cache-control, x-request-id) are addressable
	// without quoting. A branch only ever starts after '.'+letter or
	// '@'+letter, and the DSL has no branch arithmetic, so a '-' here is
	// unambiguously part of the key.
	return isLetter(ch) || ('0' <= ch && ch <= '9') || ch == '.' || ch == '-'
}

func isLetter(ch byte) bool {
	return 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || ch == '_'
}

func isDigit(ch byte) bool {
	return ('0' <= ch && ch <= '9') || ch == '.' || ch == '-'
}

func isBinding(tokenType token.TokenType) bool {
	// =~ or !~ MATCHES
	return tokenType == token.MATCH || tokenType == token.NO_MATCH
}

func newToken(tokenType token.TokenType, ch byte) token.Token {
	return token.Token{Type: tokenType, Literal: string(ch)}
}
