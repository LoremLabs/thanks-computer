<script lang="ts">
    import type { Op } from '../lib/types'

    interface Props {
        stack: string
        isDraft: boolean
        // The current ops list — used for duplicate-name checks and to
        // pre-fill scope with the highest existing one (most users add
        // ops to an existing scope rather than creating a new one).
        existingOps: Op[]
        onSubmit: (scope: number, name: string, content: string) => Promise<void>
        onCancel: () => void
    }
    let { stack, isDraft, existingOps, onSubmit, onCancel }: Props = $props()

    const stackOps = $derived(existingOps.filter((o) => o.stack === stack))
    const defaultScope = $derived(
        stackOps.length ? Math.max(...stackOps.map((o) => o.scope)) : 100
    )

    let name = $state('')
    // Initialized to 0; the $effect below pulls in defaultScope on
    // first run (and continues syncing while the user hasn't touched
    // the input). Initialing from the derived defaultScope directly
    // would only capture its initial value.
    let scope = $state<number>(0)
    let scopeTouched = $state(false)
    let content = $state('')
    let contentTouched = $state(false)
    let submitting = $state(false)
    let error = $state('')

    // Re-sync scope with the default while the user hasn't touched it.
    // Keeps the form sensible when existingOps loads in late.
    $effect(() => {
        if (!scopeTouched) scope = defaultScope
    })

    // Auto-template the txcl until the user types into it. The name
    // flows into the comment line so the seed feels personalized.
    function defaultContent(n: string): string {
        return `# ${n || 'new op'}\nEMIT . = .\n`
    }
    $effect(() => {
        if (!contentTouched) content = defaultContent(name)
    })

    const validName = $derived(/^[a-zA-Z][a-zA-Z0-9_-]*$/.test(name))
    const duplicate = $derived(
        stackOps.some((o) => o.scope === scope && o.name === name)
    )
    const disabled = $derived(!validName || duplicate || submitting)

    async function submit() {
        if (disabled) return
        submitting = true
        error = ''
        try {
            await onSubmit(scope, name, content)
        } catch (e) {
            error = e instanceof Error ? e.message : String(e)
        } finally {
            submitting = false
        }
    }
</script>

<form
    class="mt-2 rounded border border-neutral-200 bg-neutral-50 p-3"
    onsubmit={(e) => {
        e.preventDefault()
        submit()
    }}
>
    <div class="flex flex-wrap items-end gap-3">
        <label class="flex flex-col text-xs text-neutral-600">
            scope
            <input
                type="number"
                bind:value={scope}
                min="0"
                oninput={() => (scopeTouched = true)}
                class="mt-0.5 w-24 rounded border border-neutral-300 px-2 py-1 font-mono text-sm focus:border-brand-cyan focus:outline-none focus:ring-1 focus:ring-brand-cyan/60"
            />
        </label>
        <label class="flex flex-1 flex-col text-xs text-neutral-600">
            name
            <input
                type="text"
                bind:value={name}
                placeholder="my-new-op"
                autocomplete="off"
                spellcheck="false"
                class="mt-0.5 rounded border border-neutral-300 px-2 py-1 font-mono text-sm focus:border-brand-cyan focus:outline-none focus:ring-1 focus:ring-brand-cyan/60"
            />
        </label>
    </div>
    <label class="mt-2 flex flex-col text-xs text-neutral-600">
        resonator (txcl)
        <textarea
            bind:value={content}
            oninput={() => (contentTouched = true)}
            rows={Math.max(4, content.split('\n').length + 1)}
            spellcheck="false"
            class="mt-0.5 rounded border border-neutral-200 bg-neutral-900 p-2 font-mono text-xs leading-snug text-neutral-100 focus:outline-none focus:ring-2 focus:ring-brand-cyan/60"
        ></textarea>
    </label>
    <div class="mt-2 flex flex-wrap items-center gap-2">
        <button
            type="submit"
            {disabled}
            class="rounded border border-brand-cyan/50 bg-brand-cyan/10 px-3 py-1 text-xs font-medium text-neutral-900 hover:bg-brand-cyan/20 disabled:cursor-not-allowed disabled:opacity-50"
        >
            {submitting ? 'saving…' : 'Save'}
        </button>
        <button
            type="button"
            onclick={onCancel}
            disabled={submitting}
            class="rounded border border-neutral-300 bg-white px-3 py-1 text-xs text-neutral-700 hover:bg-neutral-50 disabled:opacity-50"
        >
            Cancel
        </button>
        {#if !isDraft}
            <span class="text-xs italic text-neutral-500">creates a new draft</span>
        {/if}
        {#if name && !validName}
            <span class="text-xs text-red-600">name must start with a letter; only A–Z, 0–9, _ and - allowed</span>
        {:else if duplicate}
            <span class="text-xs text-red-600">already exists at scope {scope}</span>
        {:else if error}
            <span class="text-xs text-red-600">{error}</span>
        {/if}
    </div>
</form>
