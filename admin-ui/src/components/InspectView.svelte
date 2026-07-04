<script lang="ts">
    // Inspect panel — ask a stack to explain its current state. The form
    // POSTs to /v1/tenants/{t}/inspect; the tenant's _inspect ops answer
    // with a structured card ({title, sections: [{title, rows: [[label,
    // value]]}], raw?}) rendered below. Where traces answer "what just
    // happened?", this answers "what is the current state, and why?".
    //
    // Form state is local (TraceDetail precedent): the route is just
    // #inspect, so nothing here needs to survive in the store.
    import { inspectTenant, type InspectCard } from '../lib/api'
    import { store } from '../lib/store.svelte'
    import JsonPre from './JsonPre.svelte'

    let stack = $state('')
    let noun = $state('')
    let id = $state('')

    let loading = $state(false)
    let error = $state('')
    let card = $state<InspectCard | null>(null)
    // Distinguishes "never ran" from "ran, no inspector answered".
    let ran = $state(false)

    const stackNames = $derived(store.state.stacks.map((s) => s.name))

    async function run(e?: Event) {
        e?.preventDefault()
        const s = stack.trim()
        if (!s || loading) return
        loading = true
        error = ''
        card = null
        try {
            card = await inspectTenant(store.state.currentTenant, {
                stack: s,
                noun: noun.trim() || undefined,
                id: id.trim() || undefined,
            })
            ran = true
        } catch (err) {
            error = err instanceof Error ? err.message : String(err)
        } finally {
            loading = false
        }
    }

    // Row values are stack-authored JSON: strings render bare, everything
    // else (numbers, bools, nested objects) as compact JSON text.
    function cell(v: unknown): string {
        if (typeof v === 'string') return v
        if (v === null || v === undefined) return '-'
        try {
            return JSON.stringify(v)
        } catch {
            return String(v)
        }
    }
</script>

<div class="flex h-full flex-col p-4">
    <header class="mb-3">
        <h2 class="font-mono text-base font-semibold text-neutral-900">inspect</h2>
        <p class="text-xs text-neutral-500">
            ask a stack to explain its current state — trace answers "what just
            happened?", inspect answers "what is the current state, and why?".
        </p>
    </header>

    <form class="mb-4 flex flex-wrap items-end gap-2" onsubmit={run}>
        <label class="flex flex-col gap-1">
            <span class="text-xs text-neutral-500">stack</span>
            <input
                class="w-44 rounded border border-neutral-300 bg-white px-2 py-1 font-mono text-sm focus:border-brand-cyan focus:outline-none"
                bind:value={stack}
                list="inspect-stacks"
                placeholder="marketing"
            />
        </label>
        <datalist id="inspect-stacks">
            {#each stackNames as name (name)}
                <option value={name}></option>
            {/each}
        </datalist>
        <label class="flex flex-col gap-1">
            <span class="text-xs text-neutral-500">noun</span>
            <input
                class="w-32 rounded border border-neutral-300 bg-white px-2 py-1 font-mono text-sm focus:border-brand-cyan focus:outline-none"
                bind:value={noun}
                placeholder="user"
            />
        </label>
        <label class="flex flex-col gap-1">
            <span class="text-xs text-neutral-500">id</span>
            <input
                class="w-64 rounded border border-neutral-300 bg-white px-2 py-1 font-mono text-sm focus:border-brand-cyan focus:outline-none"
                bind:value={id}
                placeholder="matt@example.com"
            />
        </label>
        <button
            type="submit"
            disabled={loading || !stack.trim()}
            class="rounded border border-brand-cyan/50 bg-brand-cyan/10 px-3 py-1 font-mono text-sm text-neutral-900 hover:bg-brand-cyan/20 focus:border-brand-cyan focus:outline-none disabled:cursor-not-allowed disabled:opacity-40"
        >
            {loading ? 'inspecting…' : 'inspect'}
        </button>
    </form>

    {#if error}
        <p class="rounded border border-red-300 bg-red-50 p-2 text-xs text-red-800">{error}</p>
    {:else if loading}
        <p class="text-sm italic text-neutral-400">inspecting…</p>
    {:else if card}
        <div class="overflow-auto rounded border border-neutral-200 bg-white p-4">
            {#if card.title}
                <h3 class="mb-3 font-mono text-sm font-semibold text-neutral-900">
                    {card.title}
                </h3>
            {/if}
            {#each card.sections ?? [] as section, i (i)}
                <section class="mb-4 last:mb-0">
                    {#if section.title}
                        <h4 class="mb-1 text-xs font-semibold tracking-wide text-neutral-500 uppercase">
                            {section.title}
                        </h4>
                    {/if}
                    <dl class="grid grid-cols-[12rem_1fr] gap-x-4 gap-y-1 text-sm">
                        {#each section.rows ?? [] as row, j (j)}
                            <dt class="text-neutral-500">{row[0]}</dt>
                            <dd class="font-mono break-all text-neutral-800">{cell(row[1])}</dd>
                        {/each}
                    </dl>
                </section>
            {/each}
            {#if card.raw !== undefined && card.raw !== null}
                <details class="mt-3">
                    <summary class="cursor-pointer text-xs text-neutral-500 select-none">raw</summary>
                    <div class="mt-2">
                        <JsonPre value={card.raw} />
                    </div>
                </details>
            {/if}
        </div>
        <details class="mt-3">
            <summary class="cursor-pointer text-xs text-neutral-500 select-none">card JSON</summary>
            <div class="mt-2">
                <JsonPre value={card} />
            </div>
        </details>
    {:else if ran}
        <p class="rounded border border-neutral-200 bg-white p-4 text-sm italic text-neutral-400">
            no inspector answered — does this tenant have an
            <span class="font-mono not-italic text-neutral-600">_inspect</span>
            stack with an op matching this stack/noun?
        </p>
    {:else}
        <p class="rounded border border-neutral-200 bg-white p-4 text-sm italic text-neutral-400">
            pick a stack (and optionally a noun + id), then inspect. From the CLI:
            <span class="font-mono not-italic text-neutral-600"
                >txco inspect &lt;stack&gt; [noun] [id]</span
            >.
        </p>
    {/if}
</div>
