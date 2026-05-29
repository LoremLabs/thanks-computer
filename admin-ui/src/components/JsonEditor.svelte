<script lang="ts">
    import { BaseHashMismatchError } from '../lib/api'
    import JsonView from './JsonView.svelte'
    import CopyButton from './CopyButton.svelte'

    interface Props {
        value: unknown
        readonly: boolean
        emptyLabel?: string
        // Optional hint rendered next to the Save button while editing.
        // Used by the Mock tab to surface "creates a new draft" when the
        // caller's onSave will auto-clone before persisting (see
        // store.patchMockFile). Empty string or undefined hides it.
        autoDraftHint?: string
        // Save callback. Receives the parsed value (not the raw text)
        // so the caller does the PATCH with a normalized form. The
        // raw text re-serializes via JSON.stringify(parsed, null, 2)
        // before save, matching what we PATCH.
        onSave?: (parsed: unknown, rawText: string) => Promise<void>
        onReload?: () => void | Promise<void>
    }

    let {
        value,
        readonly,
        emptyLabel = 'no value',
        autoDraftHint = '',
        onSave,
        onReload,
    }: Props = $props()

    const isEmpty = $derived(value === null || value === undefined)

    let editing = $state(false)
    let draft = $state('')
    let saving = $state(false)
    let parseError = $state('')
    let saveError = $state('')
    let stale = $state(false)

    // When the upstream value changes (after a successful save or on
    // op switch), refresh the draft buffer so the next edit sees the
    // latest content.
    let lastJSON = $state('')
    $effect(() => {
        const next = serialize(value)
        if (next !== lastJSON) {
            if (draft === lastJSON || !editing) {
                draft = next
            }
            lastJSON = next
        }
    })

    function serialize(v: unknown): string {
        if (v === null || v === undefined) return ''
        try {
            return JSON.stringify(v, null, 2)
        } catch {
            return String(v)
        }
    }

    function enterEdit() {
        if (readonly || editing) return
        draft = serialize(value)
        editing = true
        parseError = ''
        saveError = ''
        stale = false
    }

    function cancel() {
        editing = false
        draft = serialize(value)
        parseError = ''
        saveError = ''
    }

    async function save() {
        if (!onSave) return
        parseError = ''
        saveError = ''
        stale = false
        let parsed: unknown
        // Allow saving an empty draft as null — useful for clearing a
        // mock without a separate DELETE affordance.
        if (draft.trim() === '') {
            parsed = null
        } else {
            try {
                parsed = JSON.parse(draft)
            } catch (e) {
                parseError = e instanceof Error ? e.message : String(e)
                return
            }
        }
        saving = true
        try {
            await onSave(parsed, draft.trim() === '' ? '' : JSON.stringify(parsed, null, 2))
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
</script>

{#if !editing}
    {#if isEmpty}
        {#if readonly}
            <p class="text-sm italic text-neutral-400">{emptyLabel}</p>
        {:else}
            <button
                type="button"
                onclick={enterEdit}
                class="rounded border border-dashed border-neutral-300 bg-white px-3 py-2 text-left text-sm italic text-neutral-500 hover:bg-neutral-50"
            >
                {emptyLabel} — click to add
            </button>
        {/if}
    {:else}
        <!-- Read mode: defer to JsonView for the syntax-highlighted
             collapsible tree. Wrap in a clickable container on
             editable drafts so the user can drop into edit mode by
             clicking anywhere inside the pane. -->
        <!-- svelte-ignore a11y_no_noninteractive_tabindex -->
        <div
            class="relative h-full overflow-auto rounded border border-neutral-200 bg-neutral-50 p-2 font-mono text-[11px] leading-snug text-neutral-700 {readonly
                ? ''
                : 'cursor-text hover:ring-1 hover:ring-brand-cyan/40'}"
            onclick={enterEdit}
            role={readonly ? undefined : 'button'}
            tabindex={readonly ? undefined : 0}
            onkeydown={(e) => {
                if (!readonly && (e.key === 'Enter' || e.key === ' ')) {
                    e.preventDefault()
                    enterEdit()
                }
            }}
        >
            <div class="absolute right-1 top-1 z-10" onclick={(e) => e.stopPropagation()} role="presentation">
                <CopyButton
                    text={serialize(value)}
                    title="copy JSON"
                    class="!text-neutral-400 hover:!bg-neutral-200 hover:!text-neutral-800"
                />
            </div>
            <JsonView {value} />
        </div>
    {/if}
{:else}
    <div>
        <textarea
            bind:value={draft}
            class="block w-full resize-y overflow-auto rounded border border-neutral-200 bg-neutral-50 p-2 font-mono text-[11px] leading-snug text-neutral-700 focus:outline-none focus:ring-2 focus:ring-brand-cyan/60"
            rows={Math.max(6, draft.split('\n').length + 1)}
            spellcheck="false"
        ></textarea>
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
            {#if autoDraftHint}
                <span class="text-xs italic text-neutral-500">{autoDraftHint}</span>
            {/if}
        </div>

        {#if parseError}
            <div class="mt-2 rounded border border-red-400 bg-red-50 p-2 text-xs text-red-800">
                <div class="font-semibold">JSON parse error</div>
                <div class="mt-0.5">{parseError}</div>
            </div>
        {/if}
        {#if stale}
            <div class="mt-2 rounded border border-amber-400 bg-amber-50 p-2 text-xs text-amber-900">
                This file changed on the server since you started editing. Reload to see the latest.
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
            <div class="mt-2 rounded border border-red-400 bg-red-50 p-2 text-xs text-red-800">{saveError}</div>
        {/if}
    </div>
{/if}
