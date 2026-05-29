// TXCL token types — mirrors chassis/txcl/token/token.go. The string
// values match the Go constants exactly so JSON fixtures generated
// from the Go side compare byte-for-byte with the TS lexer output.

export const TokenType = {
    ILLEGAL: 'ILLEGAL',
    EOF: 'EOF',

    // Identifiers + literals
    IDENT: 'IDENT',
    INT: 'INT',
    FLOAT: 'FLOAT',
    STRING: 'STRING',
    NULL: 'NULL',

    // Operators
    ASSIGN: '=',
    PLUS: '+',
    MINUS: '-',
    BANG: '!',
    ASTERISK: '*',
    SLASH: '/',
    CONCAT: '.',
    MATCH: '=~',
    NO_MATCH: '!~',
    LAND: '&&',
    LOR: '||',

    // Comparisons
    LT: '<',
    LT_EQ: '<=',
    GT: '>',
    GT_EQ: '>=',
    EQ: '==',
    NOT_EQ: '!=',

    // Delimiters
    COMMA: ',',
    SEMICOLON: ';',
    COLON: ':',
    LPAREN: '(',
    RPAREN: ')',
    LBRACE: '{',
    RBRACE: '}',
    LBRACKET: '[',
    RBRACKET: ']',

    // Keywords
    FUNCTION: 'FUNCTION',
    LET: 'LET',
    SET: 'SET',
    TRUE: 'TRUE',
    FALSE: 'FALSE',
    IF: 'IF',
    ELSE: 'ELSE',
    RETURN: 'RETURN',
    WHEN: 'WHEN',
    SELECT: 'SELECT',
    AS: 'AS',
    DEFAULT: 'DEFAULT',
    WITH: 'WITH',
    PRIORITY: 'PRIORITY',
    EXEC: 'EXEC',
    EMIT: 'EMIT',

    // Filtering / context-sensitive
    BRANCH: 'BRANCH',
    STAR: 'STAR',
    REGEX: 'REGEX',

    // Function-call sigil: `&fn(args...)`. Literal carries the identifier
    // (no leading `&`). Mirrors token.go:AMP_IDENT.
    AMP_IDENT: 'AMP_IDENT',
} as const

export type TokenType = (typeof TokenType)[keyof typeof TokenType]

export interface Token {
    type: TokenType
    literal: string
    // Byte offsets into the source. `start` is inclusive, `end` is
    // exclusive. Tokens without source span (EOF) carry start === end.
    // Positions are tracked for CodeMirror highlighting; the Go fixture
    // parity check ignores them.
    start: number
    end: number
}

// Keyword table — matches Go's `keywords` map verbatim, including the
// upper/lower-case duplicates that the Go map relies on for case-
// insensitive verb lookup. Anything else is IDENT.
export const keywords: Record<string, TokenType> = {
    fn: TokenType.FUNCTION,
    let: TokenType.LET,
    true: TokenType.TRUE,
    false: TokenType.FALSE,
    if: TokenType.IF,
    else: TokenType.ELSE,
    return: TokenType.RETURN,
    RETURN: TokenType.RETURN,
    select: TokenType.SELECT,
    SELECT: TokenType.SELECT,
    as: TokenType.AS,
    AS: TokenType.AS,
    default: TokenType.DEFAULT,
    DEFAULT: TokenType.DEFAULT,
    with: TokenType.WITH,
    WITH: TokenType.WITH,
    when: TokenType.WHEN,
    WHEN: TokenType.WHEN,
    priority: TokenType.PRIORITY,
    PRIORITY: TokenType.PRIORITY,
    exec: TokenType.EXEC,
    EXEC: TokenType.EXEC,
    emit: TokenType.EMIT,
    EMIT: TokenType.EMIT,
    set: TokenType.SET,
    SET: TokenType.SET,
    null: TokenType.NULL,
    NULL: TokenType.NULL,
}

export function lookupIdent(ident: string): TokenType {
    const t = keywords[ident]
    return t !== undefined ? t : TokenType.IDENT
}
