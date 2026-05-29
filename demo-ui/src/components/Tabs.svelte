<script lang="ts">
    interface Props {
        tabs: string[]
        active: string
        onSelect: (tab: string) => void
    }

    let { tabs, active, onSelect }: Props = $props()
</script>

<div class="flex items-center gap-1 overflow-x-auto whitespace-nowrap border-b border-neutral-200">
    {#each tabs as tab}
        <button
            type="button"
            class="shrink-0 -mb-px border-b-2 px-3 py-1.5 text-sm font-medium transition-colors {tab === active
                ? 'border-brand-cyan text-neutral-900'
                : 'border-transparent text-neutral-500 hover:text-neutral-800'}"
            onclick={() => onSelect(tab)}
        >
            {tab}
        </button>
    {/each}
</div>
