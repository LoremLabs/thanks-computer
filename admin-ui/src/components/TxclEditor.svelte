<script lang="ts">
    import { onMount, onDestroy } from 'svelte'
    import { EditorView, lineNumbers, highlightActiveLine, keymap, drawSelection } from '@codemirror/view'
    import { EditorState, Compartment } from '@codemirror/state'
    import { defaultKeymap, history, historyKeymap } from '@codemirror/commands'
    import { BaseHashMismatchError, type ValidateError } from '../lib/api'
    import { txcl } from '../lib/txcl/codemirror'
    import { txclTheme, txclHighlighting } from '../lib/txcl/theme'
    import CopyButton from './CopyButton.svelte'

    interface Props {
        value: string
        // Click-to-edit is disabled when readonly (active / superseded
        // version). The editor renders as a static, highlighted view.
        readonly: boolean
        // Per-file validation errors to render inline below the
        // editor. Caller filters from state.lastValidation.
        errors?: ValidateError[]
        // Called on Save. Should PATCH the file via the store; the
        // store handles cache invalidation + auto-validate. Returning
        // a BaseHashMismatchError surfaces the yellow stale banner;
        // any other error renders as red below the buttons.
        onSave?: (content: string) => Promise<void>
        // Called when the user clicks "Reload" on the stale banner.
        onReload?: () => void | Promise<void>
    }

    let { value, readonly, errors = [], onSave, onReload }: Props = $props()

    let editing = $state(false)
    let saving = $state(false)
    let saveError = $state('')
    let stale = $state(false)

    let editorEl: HTMLDivElement
    let view: EditorView | undefined

    // Compartments let us flip editability + the line-number gutter
    // without rebuilding the editor (and losing scroll/history). One
    // set per component instance — compartments are state-scoped.
    const editableComp = new Compartment()
    const readOnlyComp = new Compartment()
    const gutterComp = new Compartment()

    // Tracks the last server-acknowledged value so the value-sync
    // effect can tell an external update (save landed / op switched)
    // apart from the user's own in-progress edits. Plain variable, not
    // $state — it's bookkeeping, not reactive UI.
    let lastSeenValue = ''

    function canEdit(): boolean {
        return editing && !readonly
    }

    function modeEffects() {
        const edit = canEdit()
        return [
            editableComp.reconfigure(EditorView.editable.of(edit)),
            readOnlyComp.reconfigure(EditorState.readOnly.of(!edit)),
            // Line numbers + active-line only while editing; read mode
            // stays as clean as the old <pre>.
            gutterComp.reconfigure(edit ? [lineNumbers(), highlightActiveLine()] : []),
        ]
    }

    function baseExtensions() {
        return [
            txcl(),
            txclHighlighting,
            txclTheme,
            history(),
            drawSelection(),
            keymap.of([
                {
                    key: 'Mod-s',
                    run: () => {
                        if (canEdit() && onSave) void save()
                        // Swallow the browser save dialog whenever the
                        // editor has focus.
                        return true
                    },
                },
                {
                    key: 'Escape',
                    run: () => {
                        if (editing) {
                            cancel()
                            return true
                        }
                        return false
                    },
                },
                ...defaultKeymap,
                ...historyKeymap,
            ]),
            editableComp.of(EditorView.editable.of(false)),
            readOnlyComp.of(EditorState.readOnly.of(true)),
            gutterComp.of([]),
        ]
    }

    onMount(() => {
        view = new EditorView({
            parent: editorEl,
            state: EditorState.create({ doc: value, extensions: baseExtensions() }),
        })
        lastSeenValue = value
    })

    onDestroy(() => view?.destroy())

    // Pull external value changes into the doc, but never clobber the
    // user's unsaved edits. Mirrors the old prop-reactivity block:
    // overwrite only when not editing, or when the current doc still
    // matches the last value we saw (i.e. no local edits pending).
    $effect(() => {
        const v = value
        if (!view) return
        const current = view.state.doc.toString()
        if (v !== current && (!editing || current === lastSeenValue)) {
            view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: v } })
        }
        lastSeenValue = v
    })

    function enterEdit() {
        if (readonly || editing) return
        editing = true
        saveError = ''
        stale = false
        view?.dispatch({ effects: modeEffects() })
        view?.focus()
    }

    function cancel() {
        editing = false
        saveError = ''
        // Discard local edits: reset the doc to the saved value, then
        // drop back to read mode.
        view?.dispatch({
            changes: { from: 0, to: view.state.doc.length, insert: value },
            effects: modeEffects(),
        })
    }

    async function save() {
        if (!onSave || !view) return
        saving = true
        saveError = ''
        stale = false
        try {
            await onSave(view.state.doc.toString())
            // The store refreshes `value` via prop reactivity. Stay in
            // edit mode for further iteration.
        } catch (e) {
            if (e instanceof BaseHashMismatchError) {
                stale = true
            } else {
                saveError = e instanceof Error ? e.message : String(e)
            }
        } finally {
            saving = false
        }
    }

    async function reload() {
        stale = false
        if (onReload) await onReload()
    }

    function onWrapperKeydown(e: KeyboardEvent) {
        if (!readonly && !editing && (e.key === 'Enter' || e.key === ' ')) {
            e.preventDefault()
            enterEdit()
        }
    }
</script>

<div class="relative">
    {#if !editing}
        <!-- Copy button only in read mode, matching the old layout
             (avoids overlapping the line-number gutter while editing). -->
        <div class="absolute right-1 top-1 z-10">
            <CopyButton text={value} title="copy resonator" />
        </div>
    {/if}

    <!-- CodeMirror mounts into this div. In read mode (editable=false)
         CM doesn't capture focus, so the wrapper handles click/Enter to
         enter edit mode. The hover ring hints editability on drafts. -->
    <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
    <div
        bind:this={editorEl}
        class="overflow-hidden rounded {!readonly && !editing
            ? 'cursor-text hover:ring-1 hover:ring-brand-cyan/40'
            : ''} {editing ? 'ring-2 ring-brand-cyan/60' : ''}"
        role={!readonly && !editing ? 'button' : undefined}
        tabindex={!readonly && !editing ? 0 : undefined}
        onclick={!readonly && !editing ? enterEdit : undefined}
        onkeydown={onWrapperKeydown}
    ></div>

    {#if editing}
        <div class="mt-2 flex items-center gap-2">
            <button
                type="button"
                disabled={saving}
                onclick={save}
                class="rounded border border-brand-cyan/50 bg-brand-cyan/10 px-3 py-1 text-xs font-medium text-neutral-900 hover:bg-brand-cyan/20 disabled:opacity-50"
            >
                {saving ? 'saving…' : 'save'}
            </button>
            <button
                type="button"
                disabled={saving}
                onclick={cancel}
                class="rounded border border-neutral-300 bg-white px-3 py-1 text-xs text-neutral-700 hover:bg-neutral-50 disabled:opacity-50"
            >
                cancel
            </button>
            <span class="text-[11px] text-neutral-400">⌘S save · esc cancel</span>
        </div>
    {/if}

    {#if stale}
        <div class="mt-2 rounded border border-amber-400 bg-amber-50 p-2 text-xs text-amber-900">
            This file changed on the server since you started editing. Reload to see the latest content.
            <button
                type="button"
                onclick={reload}
                class="ml-2 rounded border border-amber-500 bg-white px-2 py-0.5 font-medium text-amber-900 hover:bg-amber-100"
            >
                reload
            </button>
        </div>
    {/if}

    {#if saveError}
        <div class="mt-2 rounded border border-red-400 bg-red-50 p-2 text-xs text-red-800">
            {saveError}
        </div>
    {/if}

    {#if errors.length > 0}
        <div class="mt-2 rounded border border-red-300 bg-red-50 p-2 text-xs text-red-800">
            <div class="mb-1 font-semibold">
                validation: {errors.length} error{errors.length === 1 ? '' : 's'}
            </div>
            <ul class="space-y-0.5">
                {#each errors as e}
                    <li>
                        <span class="font-mono">{e.path}</span>: {e.err}
                    </li>
                {/each}
            </ul>
        </div>
    {/if}
</div>
