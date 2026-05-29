<script lang="ts">
    import { onMount, onDestroy } from 'svelte'
    import { EditorView, lineNumbers, keymap, drawSelection } from '@codemirror/view'
    import { EditorState } from '@codemirror/state'
    import { defaultKeymap, history, historyKeymap } from '@codemirror/commands'
    import { txcl } from '../../lib/txcl/codemirror'
    import { txclTheme, txclHighlighting } from '../../lib/txcl/theme'

    interface Props {
        value: string
        onChange: (v: string) => void
    }

    let { value, onChange }: Props = $props()

    let editorEl: HTMLDivElement
    let view: EditorView | undefined

    // Tracks the last value we pushed into the doc so the value-sync
    // effect can tell external prop changes apart from the user's own
    // edits (which we don't want to clobber). Plain bookkeeping, not
    // reactive state.
    let lastSeen = ''

    onMount(() => {
        view = new EditorView({
            parent: editorEl,
            state: EditorState.create({
                doc: value,
                extensions: [
                    txcl(),
                    txclHighlighting,
                    txclTheme,
                    lineNumbers(),
                    history(),
                    drawSelection(),
                    keymap.of([...defaultKeymap, ...historyKeymap]),
                    EditorView.updateListener.of((u) => {
                        if (u.docChanged) {
                            const v = u.state.doc.toString()
                            lastSeen = v
                            onChange(v)
                        }
                    }),
                ],
            }),
        })
        lastSeen = value
    })

    onDestroy(() => view?.destroy())

    // Pull external value changes into the doc without stomping the
    // user's in-progress edits: only overwrite when the prop differs
    // from both the current doc and the last value we emitted.
    $effect(() => {
        const v = value
        if (!view) return
        const current = view.state.doc.toString()
        if (v !== current && v !== lastSeen) {
            view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: v } })
            lastSeen = v
        }
    })
</script>

<div bind:this={editorEl} class="overflow-hidden rounded ring-1 ring-neutral-300"></div>
