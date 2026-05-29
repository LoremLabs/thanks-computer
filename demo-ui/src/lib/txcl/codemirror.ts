// CodeMirror 6 language adapter for TXCL.
//
// Design note — why a StreamLanguage callback instead of reusing the
// parity-tested `Lexer` from ./lexer.ts: the Lexer operates on a full
// document and produces a flat token list with byte offsets; CM6's
// StreamLanguage walks line-by-line and expects per-call tag emission
// against a per-line `StringStream`. Bridging the two cleanly would
// require either re-lexing on every edit (slow) or maintaining a
// shadow lexer state that tracks CM's per-line view (fragile). The
// recognizer below mirrors the Go lexer's switch table directly on
// the StringStream API — same decisions, different stream model.
//
// The Lexer in ./lexer.ts remains the canonical port and is used by
// downstream features (validation diagnostics in Phase 4) that need a
// flat token list with positions. Both implementations import the
// same keyword table from ./tokens.ts so verb additions stay in
// lockstep.

import {
    StreamLanguage,
    LanguageSupport,
    LanguageDescription,
    type StreamParser,
    type StringStream,
} from '@codemirror/language'
import { tags as t } from '@lezer/highlight'
import { keywords, TokenType } from './tokens'

// State carried between token() calls. CM6 keeps one State per line
// boundary; for TXCL the only cross-line states that matter are:
//
//  - `lastType`: drives the same `*` → STAR vs ASTERISK, `/` → REGEX
//    vs SLASH, `-` → MINUS vs INT/FLOAT context-sensitive decisions
//    the Go lexer makes via `LastToken`. Persists across lines so a
//    `WHEN` on one line and a `*` on the next still resolves to STAR.
//
//  - `inString`: a `"..."` literal that wasn't closed before EOL. The
//    Go lexer's readString consumes raw newlines until the closing
//    quote, so multi-line strings are legal. We mirror that.
interface State {
    lastType: string | null
    inString: boolean
}

function startState(): State {
    return { lastType: null, inString: false }
}

function copyState(s: State): State {
    return { lastType: s.lastType, inString: s.inString }
}

// Tag names map directly to @lezer/highlight tags via the theme's
// HighlightStyle (see ./theme.ts). Keeping the names as opaque
// strings here lets the theme rename them without touching this file.
const TAG = {
    keyword: 'keyword',
    bool: 'bool',
    null: 'null',
    string: 'string',
    regexp: 'regexp',
    number: 'number',
    comment: 'comment',
    operator: 'operator',
    propertyName: 'propertyName', // BRANCH (.foo, @web.req...)
    variableName: 'variableName', // bare IDENT
    function: 'function', // AMP_IDENT (&fn function-call sigil)
    punctuation: 'punctuation',
    controlOperator: 'controlOperator', // STAR after WHEN
    invalid: 'invalid',
} as const

// Reverse lookup: which CM tag should a TokenType render as. Drives
// the `lastType`-aware decisions; the actual coloring happens in the
// theme via @lezer/highlight tag mapping.

const parser: StreamParser<State> = {
    name: 'txcl',
    startState,
    copyState,

    // CM6 maps string return values from token() to @lezer/highlight
    // tags via a built-in table that knows the base tag names
    // (keyword, string, variableName, etc.). Modifier tags like
    // `t.function(...)` aren't in that table, so we wire them here.
    tokenTable: {
        function: t.function(t.variableName),
    },

    token(stream, state) {
        // Continuation of an unterminated string from a previous line.
        if (state.inString) {
            return consumeStringTail(stream, state)
        }

        if (stream.eatSpace()) return null

        // Single-line comment runs to EOL.
        if (stream.peek() === '#') {
            stream.skipToEnd()
            return TAG.comment
        }

        const ch = stream.next()
        if (!ch) return null

        // Strings — handled before the operator switch because the
        // opening `"` could otherwise be misread.
        if (ch === '"') {
            return readString(stream, state)
        }

        // Identifiers, keywords, and the `b64"..."` typed-string
        // prefix. isLetter mirrors the Go lexer.
        if (isLetterChar(ch)) {
            return readIdentifier(stream, state, ch)
        }

        // Numbers (positive). Negative-led numbers are handled inside
        // the `-` case so unary-minus and number-of-negative don't
        // collide.
        if (isDigitChar(ch)) {
            return readNumber(stream, state, ch)
        }

        switch (ch) {
            case '=':
                if (stream.eat('=')) return remember(state, TokenType.EQ, TAG.operator)
                if (stream.eat('~')) return remember(state, TokenType.MATCH, TAG.operator)
                return remember(state, TokenType.ASSIGN, TAG.operator)

            case '+':
                return remember(state, TokenType.PLUS, TAG.operator)

            case '-': {
                // `-3` → NEGATIVE-led INT/FLOAT; `.x - .y` → MINUS.
                const p = stream.peek()
                if (p && isDigitChar(p)) {
                    return readNumber(stream, state, '-')
                }
                return remember(state, TokenType.MINUS, TAG.operator)
            }

            case '!':
                if (stream.eat('=')) return remember(state, TokenType.NOT_EQ, TAG.operator)
                if (stream.eat('~')) return remember(state, TokenType.NO_MATCH, TAG.operator)
                return remember(state, TokenType.BANG, TAG.operator)

            case '/':
                // `/.../` is a regex literal only when it follows
                // `=~`/`!~` (a binding context). Otherwise it's plain
                // division. Mirrors lexer.go:isBinding().
                if (state.lastType === TokenType.MATCH || state.lastType === TokenType.NO_MATCH) {
                    return readRegex(stream, state)
                }
                return remember(state, TokenType.SLASH, TAG.operator)

            case '*':
                // `WHEN *` → STAR; everything else → ASTERISK.
                if (state.lastType === TokenType.WHEN) {
                    return remember(state, TokenType.STAR, TAG.controlOperator)
                }
                return remember(state, TokenType.ASTERISK, TAG.operator)

            case '<':
                if (stream.eat('=')) return remember(state, TokenType.LT_EQ, TAG.operator)
                return remember(state, TokenType.LT, TAG.operator)

            case '>':
                if (stream.eat('=')) return remember(state, TokenType.GT_EQ, TAG.operator)
                return remember(state, TokenType.GT, TAG.operator)

            case ';':
                return remember(state, TokenType.SEMICOLON, TAG.punctuation)
            case ':':
                return remember(state, TokenType.COLON, TAG.punctuation)
            case ',':
                return remember(state, TokenType.COMMA, TAG.punctuation)
            case '(':
                return remember(state, TokenType.LPAREN, TAG.punctuation)
            case ')':
                return remember(state, TokenType.RPAREN, TAG.punctuation)
            case '{':
                return remember(state, TokenType.LBRACE, TAG.punctuation)
            case '}':
                return remember(state, TokenType.RBRACE, TAG.punctuation)
            case '[':
                return remember(state, TokenType.LBRACKET, TAG.punctuation)
            case ']':
                return remember(state, TokenType.RBRACKET, TAG.punctuation)

            case '&': {
                if (stream.eat('&')) return remember(state, TokenType.LAND, TAG.operator)
                // `&fn` function-call sigil. After the `&`, the
                // identifier is read by the same rules as a bare
                // ident — letters then letters/digits.
                const p = stream.peek()
                if (p && isLetterChar(p)) {
                    while (!stream.eol()) {
                        const c = stream.peek()
                        if (!c) break
                        if (isLetterChar(c) || isDigitChar(c)) {
                            stream.next()
                        } else {
                            break
                        }
                    }
                    return remember(state, TokenType.AMP_IDENT, TAG.function)
                }
                return remember(state, TokenType.ILLEGAL, TAG.invalid)
            }
            case '|':
                if (stream.eat('|')) return remember(state, TokenType.LOR, TAG.operator)
                return remember(state, TokenType.ILLEGAL, TAG.invalid)

            case '.': {
                // `.foo` BRANCH iff the next char is a letter. Bare
                // `.` produces no token in Go; we surface it as
                // invalid here so the editor flags it visibly.
                const p = stream.peek()
                if (p && isLetterChar(p)) return readBranch(stream, state)
                return remember(state, TokenType.ILLEGAL, TAG.invalid)
            }

            case '@': {
                const p = stream.peek()
                if (p && isLetterChar(p)) return readBranch(stream, state)
                return remember(state, TokenType.ILLEGAL, TAG.invalid)
            }

            default:
                return TAG.invalid
        }
    },
}

export const txclLanguage = StreamLanguage.define(parser)

// Convenience export so callers can drop one extension into an
// EditorState without thinking about LanguageSupport vs raw Language.
// Matches the @codemirror/lang-* package convention.
export function txcl(): LanguageSupport {
    return new LanguageSupport(txclLanguage)
}

// Language description registered on the global LanguageDescription
// list — lets future features look TXCL up by name without importing
// this module directly.
export const txclLanguageDescription = LanguageDescription.of({
    name: 'TXCL',
    alias: ['txcl'],
    load: async () => txcl(),
})

// ──────────────────────────────────────────────────────────────────
// Tokenizer helpers — mirror the named helpers in
// chassis/txcl/lexer/lexer.go.

function readString(stream: StringStream, state: State): string {
    // Caller has already consumed the opening `"`. Read until the
    // matching `"` or EOL (in which case state.inString flags a
    // continuation onto the next line).
    while (!stream.eol()) {
        const c = stream.next()
        if (!c) break
        if (c === '\\') {
            // Skip the escaped byte unconditionally so escaped quotes
            // don't prematurely terminate.
            stream.next()
            continue
        }
        if (c === '"') {
            state.lastType = TokenType.STRING
            state.inString = false
            return TAG.string
        }
    }
    // Hit EOL before closing quote → continue on next line.
    state.inString = true
    return TAG.string
}

function consumeStringTail(stream: StringStream, state: State): string {
    while (!stream.eol()) {
        const c = stream.next()
        if (!c) break
        if (c === '\\') {
            stream.next()
            continue
        }
        if (c === '"') {
            state.lastType = TokenType.STRING
            state.inString = false
            return TAG.string
        }
    }
    return TAG.string
}

function readIdentifier(stream: StringStream, state: State, first: string): string {
    // First byte already consumed by stream.next(). Identifier bytes:
    // letters and digits (per Go's readIdentifier loop).
    let lit = first
    while (!stream.eol()) {
        const p = stream.peek()
        if (!p) break
        if (isLetterChar(p) || isDigitChar(p)) {
            lit += stream.next()
        } else {
            break
        }
    }

    // `b64"..."` typed-string: identifier is exactly "b64" and the
    // very next char is `"`. Consume the string and tag as string.
    if (lit === 'b64' && stream.peek() === '"') {
        stream.next() // opening "
        return readString(stream, state)
    }

    const kw = keywords[lit]
    if (kw !== undefined) {
        // Map keyword bucket to a visual category.
        if (kw === TokenType.TRUE || kw === TokenType.FALSE) {
            state.lastType = kw
            return TAG.bool
        }
        if (kw === TokenType.NULL) {
            state.lastType = kw
            return TAG.null
        }
        state.lastType = kw
        return TAG.keyword
    }
    state.lastType = TokenType.IDENT
    return TAG.variableName
}

function readNumber(stream: StringStream, state: State, first: string): string {
    // Go's isDigit accepts `0-9`, `.`, and `-` so a number reads as
    // a contiguous run until a non-isDigit char. We mirror.
    let lit = first
    let sawDot = first === '.'
    while (!stream.eol()) {
        const p = stream.peek()
        if (!p) break
        if (p >= '0' && p <= '9') {
            lit += stream.next()
            continue
        }
        if (p === '.' && !sawDot) {
            sawDot = true
            lit += stream.next()
            continue
        }
        break
    }
    state.lastType = lit.includes('.') ? TokenType.FLOAT : TokenType.INT
    return TAG.number
}

function readRegex(stream: StringStream, state: State): string {
    // Caller has already consumed the opening `/`. Read until the
    // closing `/`, honoring `\/` escapes.
    while (!stream.eol()) {
        const c = stream.next()
        if (!c) break
        if (c === '\\') {
            // Skip the next char (the escaped one).
            stream.next()
            continue
        }
        if (c === '/') break
    }
    state.lastType = TokenType.REGEX
    return TAG.regexp
}

function readBranch(stream: StringStream, state: State): string {
    // Caller has consumed the leading `.` or `@`. Greedily consume
    // bare branch chars; if a `"` follows, consume a quoted segment
    // and continue (mirrors Go's scanBranchRun + appendQuotedSegment).
    for (;;) {
        if (stream.eol()) break
        const p = stream.peek()
        if (!p) break
        if (p === '"') {
            stream.next() // opening quote
            while (!stream.eol()) {
                const c = stream.next()
                if (!c) break
                if (c === '\\') {
                    stream.next()
                    continue
                }
                if (c === '"') break
            }
            continue
        }
        if (!isBranchChar(p)) break
        stream.next()
    }
    state.lastType = TokenType.BRANCH
    return TAG.propertyName
}

// ──────────────────────────────────────────────────────────────────
// Char-class helpers. The Go lexer operates on bytes; for ASCII these
// are identical to single-char string checks.

function isLetterChar(c: string): boolean {
    return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c === '_'
}
function isDigitChar(c: string): boolean {
    return c >= '0' && c <= '9'
}
function isBranchChar(c: string): boolean {
    return isLetterChar(c) || isDigitChar(c) || c === '.' || c === '-'
}

function remember(state: State, lastType: string, tag: string): string {
    state.lastType = lastType
    return tag
}
