<script lang="ts">
    // Svelte 5 port of semicolons.com/www/src/lib/components/Ago.svelte —
    // wraps timeago.js's format() and lets the user click to toggle
    // between relative ("3 minutes ago") and absolute ("2026-05-14
    // 10:24:13") forms. The absolute side uses toLocaleString so it
    // respects the browser locale; the title attribute holds whichever
    // representation is *not* currently shown.

    import { format } from 'timeago.js'

    interface Props {
        at?: string | Date
    }
    let { at }: Props = $props()

    let showAbsolute = $state(false)
    // Drives periodic re-render so the relative label stays fresh.
    // 20s matches the upstream Ago.svelte cadence.
    let tick = $state(0)
    $effect(() => {
        const id = setInterval(() => {
            tick++
        }, 20000)
        return () => clearInterval(id)
    })

    // Normalize once. ISO string keeps the <time datetime> attribute
    // machine-readable regardless of whether the caller passed a Date
    // or a string.
    const iso = $derived.by(() => {
        if (!at) return ''
        const d = at instanceof Date ? at : new Date(at)
        return isNaN(d.getTime()) ? String(at) : d.toISOString()
    })
    const relative = $derived.by(() => {
        if (!at) return ''
        tick // touch so $derived re-runs each tick
        const d = at instanceof Date ? at : new Date(at)
        if (isNaN(d.getTime())) return String(at)
        return format(d)
    })
    const absolute = $derived.by(() => {
        if (!at) return ''
        const d = at instanceof Date ? at : new Date(at)
        if (isNaN(d.getTime())) return String(at)
        return d.toLocaleString()
    })
</script>

{#if at}
    <button
        type="button"
        class="cursor-pointer text-left hover:underline"
        onclick={() => (showAbsolute = !showAbsolute)}
        title={showAbsolute ? relative : absolute}
    >
        <time datetime={iso}>{showAbsolute ? absolute : relative}</time>
    </button>
{:else}
    <span class="text-neutral-400">—</span>
{/if}
