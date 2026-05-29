<script lang="ts">
    import CopyButton from './CopyButton.svelte'
    import JsonView from './JsonView.svelte'

    interface Props {
        value: unknown
        emptyLabel?: string
    }

    let { value, emptyLabel = 'no value' }: Props = $props()

    const isEmpty = $derived(value === null || value === undefined)

    // Flat pretty-printed JSON for the copy-to-clipboard path. The
    // visible tree is collapsible via JsonView; copy always yields
    // the full expanded form regardless of how the user has it
    // folded in the UI.
    const copyText = $derived.by(() => {
        if (isEmpty) return ''
        try {
            return JSON.stringify(value, null, 2)
        } catch {
            return String(value)
        }
    })
</script>

{#if isEmpty}
    <p class="text-sm italic text-neutral-400">{emptyLabel}</p>
{:else}
    <div class="relative h-full overflow-auto rounded border border-neutral-200 bg-neutral-50 p-2 font-mono text-[11px] leading-snug text-neutral-700">
        <div class="absolute right-1 top-1 z-10">
            <CopyButton
                text={copyText}
                title="copy JSON"
                class="!text-neutral-400 hover:!bg-neutral-200 hover:!text-neutral-800"
            />
        </div>
        <JsonView {value} />
    </div>
{/if}
