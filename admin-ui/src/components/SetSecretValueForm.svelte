<script lang="ts">
    import type { Snippet } from 'svelte'

    interface Props {
        // Receives the entered value. The form shows a spinner until it
        // resolves, then clears the value. Throwing renders the message
        // inline and keeps the value so the user can retry.
        onSubmit: (value: string) => Promise<void>
        submitLabel: string
        // Extra fields (name, description) the parent renders above the
        // value input.
        header?: Snippet
        // Blocks submit when true — e.g. an upstream type-the-name
        // confirm gate isn't satisfied yet.
        disabled?: boolean
        valueLabel?: string
        placeholder?: string
    }
    let {
        onSubmit,
        submitLabel,
        header,
        disabled = false,
        valueLabel = 'value',
        placeholder = '',
    }: Props = $props()

    // Cleartext lives ONLY in this component-local state, for as long
    // as a submit is in flight. It is never written to the store, to
    // localStorage, or logged. Cleared on success and on unmount.
    let value = $state('')
    let error = $state('')
    let submitting = $state(false)

    async function handleSubmit(e: Event) {
        e.preventDefault()
        if (submitting || disabled) return
        if (value === '') {
            error = 'value cannot be empty'
            return
        }
        submitting = true
        error = ''
        // Snapshot so the field can clear without racing the await.
        const v = value
        try {
            await onSubmit(v)
            // Clear on success — the value's lifetime is the request.
            // The parent usually closes the surface right after; clear
            // regardless so nothing lingers in the input.
            value = ''
        } catch (err) {
            error = err instanceof Error ? err.message : String(err)
        } finally {
            submitting = false
        }
    }

    // Belt-and-suspenders: wipe on unmount so a closed panel leaves no
    // value behind in the component instance.
    $effect(() => {
        return () => {
            value = ''
        }
    })
</script>

<form autocomplete="off" novalidate onsubmit={handleSubmit} class="flex flex-col gap-3">
    {@render header?.()}

    <label class="flex flex-col gap-1 text-xs text-neutral-500">
        {valueLabel}
        <!-- Visible (not masked): operators need to verify they pasted
             the value correctly, and a textarea handles multi-line
             secrets (PEM keys, JSON blobs). autocomplete/spellcheck/
             autocorrect off so the value isn't sent to a spellcheck
             service or mangled. Enter inserts a newline; submit is the
             button only. -->
        <textarea
            rows="3"
            autocomplete="off"
            spellcheck="false"
            autocapitalize="off"
            {placeholder}
            bind:value
            class="resize-y rounded border border-neutral-300 bg-white px-2 py-1 font-mono text-sm text-neutral-800 placeholder:text-neutral-400 focus:border-brand-cyan focus:outline-none"
        ></textarea>
    </label>

    {#if error}
        <p class="rounded border border-red-300 bg-red-50 px-2 py-1 text-xs text-red-800">
            {error}
        </p>
    {/if}

    <button
        type="submit"
        disabled={submitting || disabled}
        class="self-start rounded border border-brand-cyan/50 bg-brand-cyan/10 px-3 py-1 text-sm text-neutral-900 hover:bg-brand-cyan/20 disabled:cursor-not-allowed disabled:opacity-50"
    >
        {submitting ? 'saving…' : submitLabel}
    </button>
</form>
