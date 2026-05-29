// CodeMirror 6 theme + syntax-highlight style for TXCL.
//
// Two extensions are exported:
//
//   - `txclTheme`         — the editor chrome (background, cursor,
//                           selection, gutter, line-wrap). Sized to
//                           match the existing TxclEditor.svelte
//                           `pre.bg-neutral-900` look so the swap
//                           into CM6 doesn't shift the page.
//
//   - `txclHighlighting`  — the syntax-highlighting extension that
//                           applies a HighlightStyle to tags emitted
//                           by ./codemirror.ts.
//
// Palette: shades from Tailwind v4 chosen to read on the existing
// `bg-neutral-900` background. Strings/numbers/booleans match the
// JsonView.svelte semantic mapping (emerald/amber/purple) so the two
// renderers feel like one family, just inverted for the dark bg.
// Keywords use the project's brand-cyan token so the editor visually
// belongs to thanks.computer (matches focus rings + save button).

import { EditorView } from '@codemirror/view'
import { HighlightStyle, syntaxHighlighting } from '@codemirror/language'
import { tags as t } from '@lezer/highlight'

const COLORS = {
    // Editor chrome — neutral-900 base / neutral-100 fg, matching the
    // pre block this replaces.
    background: '#171717', // neutral-900
    foreground: '#f5f5f5', // neutral-100
    caret: '#22d3ee', // cyan-400 — visible cursor on dark bg
    selection: 'rgba(34, 211, 238, 0.20)', // cyan-400 @ 20%
    selectionMatch: 'rgba(34, 211, 238, 0.12)',
    lineHighlight: 'rgba(255, 255, 255, 0.03)',
    gutterBg: '#171717',
    gutterFg: '#525252', // neutral-600
    gutterActive: '#a3a3a3', // neutral-400

    // Tokens — bright enough to read on neutral-900.
    keyword: 'oklch(0.79 0.17 195)', // brand-cyan — SELECT/EXEC/AS/...
    operator: '#a3a3a3', // neutral-400
    string: '#34d399', // emerald-400
    regex: 'oklch(0.85 0.18 90)', // brand-yellow — distinct from strings
    number: '#fbbf24', // amber-400
    bool: '#c084fc', // purple-400
    nullValue: '#c084fc', // purple-400 (matches JsonView)
    propertyName: '#7dd3fc', // sky-300 — branches `.foo` / `@web.req...`
    variableName: '#e5e5e5', // neutral-200 — bare idents
    function: '#f472b6', // pink-400 — `&fn` function-call sigil
    punctuation: '#737373', // neutral-500 — commas / colons / brackets
    comment: '#737373', // neutral-500 italic
    invalid: '#f87171', // red-400 — illegal/ungrammatical input
} as const

export const txclTheme = EditorView.theme(
    {
        '&': {
            color: COLORS.foreground,
            backgroundColor: COLORS.background,
            fontSize: '0.875rem', // text-sm — matches outgoing pre
            // The editor lives in a rounded card; let the wrapper own
            // the radius so consumers can size/round freely.
            borderRadius: '0.25rem', // rounded
        },
        '.cm-content': {
            // Same vertical padding the pre had (p-3 ≈ 0.75rem).
            padding: '0.75rem',
            fontFamily:
                'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
            // leading-relaxed from TxclEditor.
            lineHeight: '1.625',
            caretColor: COLORS.caret,
        },
        '.cm-cursor, .cm-dropCursor': {
            borderLeftColor: COLORS.caret,
        },
        '&.cm-focused > .cm-scroller > .cm-selectionLayer .cm-selectionBackground, .cm-selectionBackground, .cm-content ::selection':
            {
                backgroundColor: COLORS.selection,
            },
        '.cm-selectionMatch': {
            backgroundColor: COLORS.selectionMatch,
        },
        '.cm-activeLine': {
            backgroundColor: COLORS.lineHighlight,
        },
        '.cm-gutters': {
            backgroundColor: COLORS.gutterBg,
            color: COLORS.gutterFg,
            border: 'none',
            // Soft right divider so the gutter doesn't feel pasted on.
            boxShadow: '1px 0 0 rgba(255, 255, 255, 0.04)',
        },
        '.cm-activeLineGutter': {
            backgroundColor: 'transparent',
            color: COLORS.gutterActive,
        },
        // Line-number column: subtle, never compete with the code.
        '.cm-lineNumbers .cm-gutterElement': {
            padding: '0 0.5rem 0 0.25rem',
            minWidth: '1.75rem',
        },
        '.cm-scroller': {
            // Keep the same monospace family as .cm-content; CM defaults
            // it to a sans-serif which would leak into the gutter.
            fontFamily: 'inherit',
        },
        // Tooltips (used by lint hover messages in Phase 4 — set up the
        // colors now so we don't have to re-theme then).
        '.cm-tooltip': {
            backgroundColor: '#262626', // neutral-800
            color: COLORS.foreground,
            border: '1px solid #404040', // neutral-700
            borderRadius: '0.25rem',
        },
        '.cm-tooltip .cm-tooltip-arrow:before': { borderTopColor: '#404040' },
        '.cm-tooltip .cm-tooltip-arrow:after': { borderTopColor: '#262626' },
    },
    { dark: true },
)

// Map @lezer/highlight tags to colors. Tag names match those emitted
// by ./codemirror.ts. CM6 uses the standard `tags.X` set from
// @lezer/highlight; the TXCL stream parser returns matching tag
// names which CM6 resolves through this style.
export const txclHighlightStyle = HighlightStyle.define([
    { tag: t.keyword, color: COLORS.keyword, fontWeight: '600' },
    { tag: t.controlOperator, color: COLORS.keyword, fontWeight: '600' },
    { tag: t.bool, color: COLORS.bool },
    { tag: t.null, color: COLORS.nullValue },
    { tag: t.string, color: COLORS.string },
    { tag: t.regexp, color: COLORS.regex },
    { tag: t.number, color: COLORS.number },
    { tag: t.operator, color: COLORS.operator },
    { tag: t.propertyName, color: COLORS.propertyName },
    { tag: t.variableName, color: COLORS.variableName },
    { tag: t.function(t.variableName), color: COLORS.function },
    { tag: t.punctuation, color: COLORS.punctuation },
    { tag: t.comment, color: COLORS.comment, fontStyle: 'italic' },
    { tag: t.invalid, color: COLORS.invalid, textDecoration: 'underline wavy' },
])

export const txclHighlighting = syntaxHighlighting(txclHighlightStyle)
