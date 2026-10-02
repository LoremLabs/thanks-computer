// Compute refs in a CodeMirror document become marked, clickable ranges: a
// plain click opens one in a read-only view; while editing it takes
// Cmd/Ctrl-click, so a plain click still places the cursor.

import { describe, it, expect, vi, afterEach } from 'vitest'
import { EditorView } from '@codemirror/view'
import { EditorState } from '@codemirror/state'
import { computeRefs, digestAt } from './computeRefs'

const DIGEST = '3cee8f8dc76d5e1c04156da5ec03fe2dc8f347f2b742386e19e02d55235683d2'
const DOC = `# plan the pull\nEXEC "compute://sha256/${DIGEST}"\n`

let views: EditorView[] = []
afterEach(() => {
    views.forEach((v) => v.destroy())
    views = []
})

function mount(editable: boolean, onOpen: (d: string) => void) {
    const parent = document.createElement('div')
    document.body.appendChild(parent)
    const view = new EditorView({
        parent,
        state: EditorState.create({
            doc: DOC,
            extensions: [computeRefs(onOpen), EditorView.editable.of(editable)],
        }),
    })
    views.push(view)
    return view
}

function ref(view: EditorView): HTMLElement {
    const el = view.dom.querySelector('.cm-compute-ref')
    if (!(el instanceof HTMLElement)) throw new Error('no compute ref marked')
    return el
}

describe('computeRefs', () => {
    it('marks the ref with its digest', () => {
        const view = mount(false, vi.fn())
        expect(ref(view).dataset.digest).toBe(DIGEST)
        expect(digestAt(ref(view))).toBe(DIGEST)
        expect(digestAt(view.dom)).toBe('')
    })

    it('a plain click opens it in a read-only view', () => {
        const onOpen = vi.fn()
        ref(mount(false, onOpen)).dispatchEvent(new MouseEvent('click', { bubbles: true }))
        expect(onOpen).toHaveBeenCalledWith(DIGEST)
    })

    it('while editing, only Cmd/Ctrl-click opens it', () => {
        const onOpen = vi.fn()
        const el = ref(mount(true, onOpen))
        el.dispatchEvent(new MouseEvent('click', { bubbles: true }))
        expect(onOpen).not.toHaveBeenCalled()
        el.dispatchEvent(new MouseEvent('click', { bubbles: true, metaKey: true }))
        expect(onOpen).toHaveBeenCalledWith(DIGEST)
    })
})
