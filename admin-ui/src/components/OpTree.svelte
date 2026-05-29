<script lang="ts">
    import { groupOps } from '../lib/tree'
    import { store } from '../lib/store.svelte'
    import { opId, type Op } from '../lib/types'
    import Button from './Button.svelte'

    interface Props {
        ops: Op[]
        selectedId: string
        selectedStack: string
        onSelectOp: (op: Op) => void
        onSelectStack: (stack: string) => void
    }

    let { ops, selectedId, selectedStack, onSelectOp, onSelectStack }: Props = $props()

    const groups = $derived(groupOps(ops))

    // Whether a stack's ops are shown. An explicit user choice
    // (persisted in localStorage via the store) wins; otherwise the
    // default opens only the FIRST stack when there's more than one (a
    // lone stack is always open).
    function stackOpen(stack: string, index: number): boolean {
        const override = store.state.stacksCollapsed[stack]
        if (override !== undefined) return !override
        return groups.length <= 1 || index === 0
    }

    // The summary has two click targets: the ▶ disclosure toggles
    // collapse/expand (persisted via the store); the stack name selects
    // the stack. We always preventDefault (suppress the native summary
    // toggle) and drive both explicitly — clicking the name never
    // collapses, clicking ▶ never changes the selection.
    function onSummaryClick(e: MouseEvent, stack: string, index: number) {
        e.preventDefault()
        if ((e.target as HTMLElement).closest('[data-disclosure]')) {
            // collapse if currently open, expand if currently closed
            store.setStackCollapsed(stack, stackOpen(stack, index))
            return
        }
        onSelectStack(stack)
    }
</script>

<nav class="flex flex-col gap-1 py-2">
    {#if groups.length === 0}
        <p class="px-3 py-2 text-sm italic text-neutral-400">no ops loaded</p>
    {/if}
    {#each groups as group, i}
        <details open={stackOpen(group.stack, i)} class="group px-2">
            <summary
                onclick={(e) => onSummaryClick(e, group.stack, i)}
                class="flex cursor-pointer list-none items-center rounded px-1 py-1 text-xs font-semibold uppercase tracking-wide hover:bg-neutral-100 {group.stack === selectedStack ? 'bg-brand-cyan/10 text-neutral-900' : 'text-neutral-500'}"
            >
                <span
                    data-disclosure
                    title="collapse / expand"
                    class="-ml-0.5 mr-0.5 inline-flex w-5 shrink-0 cursor-pointer justify-center rounded text-neutral-400 hover:text-neutral-700 group-open:rotate-90 transition-transform"
                >▶</span>
                <span class="truncate">{group.stack}</span>
            </summary>
            <div class="ml-3 mt-1 flex flex-col gap-1 border-l border-neutral-200 pl-2">
                {#each group.scopes as scope}
                    <div class="flex flex-col">
                        <div class="px-1 py-0.5 font-mono text-base font-bold text-neutral-700">{scope.scope}</div>
                        {#each scope.ops as op (opId(op))}
                            <Button
                                variant="ghost"
                                active={opId(op) === selectedId}
                                onclick={() => onSelectOp(op)}
                            >
                                <span class="font-mono text-xs text-neutral-400">·</span>
                                <span class="truncate">{op.name}</span>
                            </Button>
                        {/each}
                    </div>
                {/each}
            </div>
        </details>
    {/each}
</nav>
