<script lang="ts">
    import type { Snippet } from 'svelte'

    type Variant = 'default' | 'ghost' | 'icon'

    interface Props {
        variant?: Variant
        active?: boolean
        onclick?: (e: MouseEvent) => void
        title?: string
        children: Snippet
        class?: string
    }

    let {
        variant = 'default',
        active = false,
        onclick,
        title,
        children,
        class: klass = '',
    }: Props = $props()

    const VARIANTS: Record<Variant, string> = {
        default:
            'inline-flex items-center gap-1.5 rounded border border-neutral-300 bg-white px-3 py-1.5 text-sm font-medium text-neutral-700 hover:bg-neutral-50 active:bg-neutral-100',
        ghost:
            'inline-flex w-full items-center gap-1.5 rounded px-2 py-1 text-left text-sm text-neutral-700 hover:bg-neutral-100',
        icon:
            'inline-flex h-7 w-7 items-center justify-center rounded text-neutral-600 hover:bg-neutral-100 hover:text-neutral-900',
    }

    const activeRing = $derived(active ? 'bg-brand-cyan/10 text-neutral-900' : '')
</script>

<button
    type="button"
    {title}
    {onclick}
    class="{VARIANTS[variant]} {activeRing} {klass}"
>
    {@render children()}
</button>
