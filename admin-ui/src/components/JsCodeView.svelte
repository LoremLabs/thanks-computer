<script lang="ts">
    // Read-only, highlighted JS/TS view (a compute's source). Loaded lazily
    // by ComputeSource so CodeMirror's JavaScript language stays out of the
    // main bundle. Same dark theme and highlight style as the txcl editor —
    // that style is keyed on the standard lezer tags both languages emit.
    import { onMount, onDestroy } from 'svelte'
    import { EditorView, lineNumbers } from '@codemirror/view'
    import { EditorState, Compartment } from '@codemirror/state'
    import { javascript } from '@codemirror/lang-javascript'
    import { txclTheme, txclHighlighting } from '../lib/txcl/theme'
    import CopyButton from './CopyButton.svelte'

    interface Props {
        value: string
        // The file's path; its extension picks TypeScript / JSX parsing.
        path: string
    }

    let { value, path }: Props = $props()

    let el: HTMLDivElement
    let view: EditorView | undefined
    const lang = new Compartment()

    function language(p: string) {
        return javascript({ typescript: /\.[mc]?tsx?$/.test(p), jsx: /x$/.test(p) })
    }

    onMount(() => {
        view = new EditorView({
            parent: el,
            state: EditorState.create({
                doc: value,
                extensions: [
                    lang.of(language(path)),
                    txclHighlighting,
                    txclTheme,
                    lineNumbers(),
                    EditorView.editable.of(false),
                    EditorState.readOnly.of(true),
                ],
            }),
        })
    })

    onDestroy(() => view?.destroy())

    $effect(() => {
        const v = value
        const p = path
        if (!view) return
        view.dispatch({
            changes: { from: 0, to: view.state.doc.length, insert: v },
            effects: lang.reconfigure(language(p)),
        })
    })
</script>

<div class="relative">
    <div class="absolute right-1 top-1 z-10">
        <CopyButton text={value} title="copy source" />
    </div>
    <div bind:this={el} class="overflow-hidden rounded"></div>
</div>
