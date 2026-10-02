// CodeMirror extension: every `compute://sha256/<digest>` in the document
// becomes a link to that compute's source. In a read-only view a plain click
// opens it (underlined on hover); while editing, a plain click still places
// the cursor and Cmd/Ctrl-click opens it.

import {
    Decoration,
    EditorView,
    MatchDecorator,
    ViewPlugin,
    type DecorationSet,
    type ViewUpdate,
} from '@codemirror/view'

const matcher = new MatchDecorator({
    regexp: /compute:\/\/sha256\/([0-9a-f]{64})/g,
    decoration: (m) =>
        Decoration.mark({
            class: 'cm-compute-ref',
            attributes: { 'data-digest': m[1], title: "open this compute's source" },
        }),
})

const marks = ViewPlugin.fromClass(
    class {
        decorations: DecorationSet
        constructor(view: EditorView) {
            this.decorations = matcher.createDeco(view)
        }
        update(u: ViewUpdate) {
            this.decorations = matcher.updateDeco(u, this.decorations)
        }
    },
    { decorations: (v) => v.decorations }
)

const style = EditorView.baseTheme({
    '.cm-content[contenteditable=false] .cm-compute-ref:hover': {
        textDecoration: 'underline',
        cursor: 'pointer',
    },
})

// digestAt returns the digest of the compute ref an event landed on, if any.
export function digestAt(target: EventTarget | null): string {
    const el = target instanceof Element ? target.closest('.cm-compute-ref') : null
    return el instanceof HTMLElement ? (el.dataset.digest ?? '') : ''
}

export function computeRefs(onOpen: (digest: string) => void) {
    return [
        marks,
        style,
        EditorView.domEventHandlers({
            click(e, view) {
                const digest = digestAt(e.target)
                if (!digest) return false
                if (view.state.facet(EditorView.editable) && !(e.metaKey || e.ctrlKey)) return false
                // Stop here: a draft's read view wraps the editor in a
                // click-to-edit handler that must not fire too.
                e.preventDefault()
                e.stopPropagation()
                onOpen(digest)
                return true
            },
        }),
    ]
}
