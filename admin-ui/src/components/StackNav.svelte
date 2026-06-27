<script lang="ts">
    import OpTree from './OpTree.svelte'
    import { store } from '../lib/store.svelte'
    import type { Op } from '../lib/types'

    // Same surface as OpTree — StackNav wraps it with a stack search box
    // so a big tenant shows only its recent stacks by default and the rest
    // are one search away. When the query is empty we render the tree; when
    // it's not, we render matches across ALL stacks (not just the loaded
    // ones). Picking a result calls onSelectStack, which pins + loads it.
    interface Props {
        ops: Op[]
        selectedId: string
        selectedStack: string
        onSelectOp: (op: Op) => void
        onSelectStack: (stack: string) => void
    }
    let { ops, selectedId, selectedStack, onSelectOp, onSelectStack }: Props = $props()

    let query = $state('')
    const results = $derived(query.trim() ? store.searchStacks(query) : [])
    const total = $derived(store.state.stacks.length)
    const shown = $derived(store.state.visibleStacks.length)
    const hidden = $derived(Math.max(0, total - shown))
    const visible = $derived(new Set(store.state.visibleStacks))

    function pick(name: string) {
        onSelectStack(name) // pins + loads + selects (and closes mobile sidebar)
        query = '' // clear → back to the tree, now including the picked stack
    }

    function onKey(e: KeyboardEvent) {
        if (e.key === 'Escape') {
            query = ''
        } else if (e.key === 'Enter' && results.length > 0) {
            pick(results[0].name)
        }
    }
</script>

<div class="px-2 pt-2">
    <input
        type="search"
        bind:value={query}
        onkeydown={onKey}
        placeholder="Search"
        aria-label="Search stacks"
        class="w-full rounded border border-neutral-200 px-2 py-1 text-sm placeholder:text-neutral-400 focus:border-brand-cyan focus:outline-none"
    />
</div>

{#if query.trim()}
    <nav class="flex flex-col gap-0.5 px-2 py-2">
        {#if results.length === 0}
            <p class="px-1 py-2 text-sm italic text-neutral-400">
                no matches “{query.trim()}”
            </p>
        {:else}
            {#each results as s (s.name)}
                <button
                    type="button"
                    onclick={() => pick(s.name)}
                    class="flex items-center gap-2 truncate rounded px-2 py-1 text-left text-sm hover:bg-neutral-100 {s.name ===
                    selectedStack
                        ? 'bg-brand-cyan/10 text-neutral-900'
                        : 'text-neutral-700'}"
                >
                    <span class="truncate">{s.name}</span>
                </button>
            {/each}
        {/if}
    </nav>
{:else}
    <OpTree {ops} {selectedId} {selectedStack} {onSelectOp} {onSelectStack} />
{/if}
