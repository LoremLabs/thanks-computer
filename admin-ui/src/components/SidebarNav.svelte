<script lang="ts">
    interface Props {
        // Which top-level view is selected. Drives the highlight on
        // the active row. We pass it down rather than reading the
        // store here so the parent can hold the routing decisions
        // (clicking Ops also unsets traces and vice versa).
        current: 'ops' | 'traces' | 'secrets'
        onSelect: (view: 'ops' | 'traces' | 'secrets') => void
    }
    let { current, onSelect }: Props = $props()

    function rowClass(view: 'ops' | 'traces' | 'secrets'): string {
        const base =
            'flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-neutral-100'
        if (current === view) {
            return base + ' bg-brand-cyan/10 font-medium text-neutral-900'
        }
        return base + ' text-neutral-700'
    }
</script>

<nav class="px-2 pt-3 pb-2">
    <button type="button" class={rowClass('ops')} onclick={() => onSelect('ops')}>
        <span class="inline-block w-4 shrink-0 text-center font-semibold tracking-tight text-brand-cyan" aria-hidden="true">o</span>
        ops
    </button>
    <button type="button" class={rowClass('traces')} onclick={() => onSelect('traces')}>
        <span class="inline-block w-4 shrink-0 text-center font-semibold tracking-tight text-brand-magenta" aria-hidden="true">o</span>
        traces
    </button>
    <button type="button" class={rowClass('secrets')} onclick={() => onSelect('secrets')}>
        <span class="inline-block w-4 shrink-0 text-center font-semibold tracking-tight text-brand-yellow" aria-hidden="true">o</span>
        secrets
    </button>
</nav>
