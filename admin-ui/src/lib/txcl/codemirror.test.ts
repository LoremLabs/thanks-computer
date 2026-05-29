// Smoke test for the CM6 StreamLanguage adapter. Drives the parser
// through its public API and asserts the tag stream covers the
// expected token kinds. This is intentionally coarser than the Go-
// parity test on ./lexer.ts — here we only need the highlighter to
// produce something visually correct; the canonical token stream
// lives in ./lexer.ts and is tested separately.

import { describe, expect, it } from 'vitest'
import { txclLanguage } from './codemirror'

interface NodeSpan {
    from: number
    to: number
    name: string
    text: string
}

function highlightSpans(input: string): NodeSpan[] {
    const tree = txclLanguage.parser.parse(input)
    const out: NodeSpan[] = []
    const cursor = tree.cursor()
    do {
        // Skip the top-level container; only emit leaves with content.
        if (cursor.from !== cursor.to && cursor.name !== 'Document') {
            out.push({
                from: cursor.from,
                to: cursor.to,
                name: cursor.name,
                text: input.slice(cursor.from, cursor.to),
            })
        }
    } while (cursor.next())
    return out
}

describe('TXCL StreamLanguage highlighter', () => {
    it('tags keywords, strings, numbers, branches in the screenshot example', () => {
        const input = `# mcp-test
SELECT @web.req.url.query.question.0
    AS .question
    DEFAULT "What is react used for?"

EXEC "mcp+https://x" WITH timeout = "60s", debug = true
`
        const spans = highlightSpans(input)
        // Find spans by their text — robust to whitespace/comment span
        // placement which CM may merge differently across versions.
        const tagFor = (text: string) =>
            spans.find((s) => s.text === text)?.name

        expect(tagFor('SELECT')).toBe('keyword')
        expect(tagFor('AS')).toBe('keyword')
        expect(tagFor('DEFAULT')).toBe('keyword')
        expect(tagFor('EXEC')).toBe('keyword')
        expect(tagFor('WITH')).toBe('keyword')

        expect(tagFor('@web.req.url.query.question.0')).toBe('propertyName')
        expect(tagFor('.question')).toBe('propertyName')

        // String literals include the surrounding quotes in the span.
        expect(tagFor('"What is react used for?"')).toBe('string')
        expect(tagFor('"mcp+https://x"')).toBe('string')
        expect(tagFor('"60s"')).toBe('string')

        expect(tagFor('true')).toBe('bool')

        // Comment is detected (the leading `#` is part of the span).
        expect(spans.some((s) => s.name === 'comment' && s.text.includes('mcp-test'))).toBe(true)

        // `=` between `timeout` and `"60s"` is an operator.
        expect(tagFor('=')).toBe('operator')

        // `timeout` and `debug` are bare identifiers.
        expect(tagFor('timeout')).toBe('variableName')
        expect(tagFor('debug')).toBe('variableName')
    })

    it('tags regex literals only after =~ / !~', () => {
        const spans = highlightSpans('.x =~ /foo/ && .y / 2')
        const tagFor = (text: string) => spans.find((s) => s.text === text)?.name
        expect(tagFor('/foo/')).toBe('regexp')
        // The second `/` is plain division (no binding context).
        const slashes = spans.filter((s) => s.text === '/')
        expect(slashes.length).toBeGreaterThan(0)
        expect(slashes[0].name).toBe('operator')
    })

    it('tags `*` as controlOperator inside WHEN, operator otherwise', () => {
        const inWhen = highlightSpans('WHEN * SET .x = 1')
        const inExpr = highlightSpans('.x * 2')
        expect(inWhen.find((s) => s.text === '*')?.name).toBe('controlOperator')
        expect(inExpr.find((s) => s.text === '*')?.name).toBe('operator')
    })
})
