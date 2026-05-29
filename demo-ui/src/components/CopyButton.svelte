<script lang="ts">
    // Small icon-only button that copies `text` to the clipboard.
    // Caller is expected to position it (typically absolute,
    // top-right of a code/JSON block). On success the icon flips to
    // a checkmark for ~1.5s, then reverts.
    //
    // Silently ignores clipboard errors (e.g. non-secure context):
    // the user gets no feedback, but nothing else breaks. If we ever
    // need to surface failures, route through a toast-style channel
    // rather than alerting from here.
    interface Props {
        text: string
        title?: string
        class?: string
    }

    let { text, title = 'copy', class: klass = '' }: Props = $props()

    let copied = $state(false)
    let timer: ReturnType<typeof setTimeout> | undefined

    async function onCopy(e: MouseEvent) {
        e.stopPropagation()
        try {
            await navigator.clipboard.writeText(text)
            copied = true
            if (timer) clearTimeout(timer)
            timer = setTimeout(() => {
                copied = false
            }, 1500)
        } catch {
            // best-effort — clipboard unavailable
        }
    }
</script>

<button
    type="button"
    {title}
    aria-label={title}
    onclick={onCopy}
    class="inline-flex h-6 w-6 items-center justify-center rounded text-neutral-400 transition-colors hover:bg-white/10 hover:text-neutral-100 {klass}"
>
    {#if copied}
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <polyline points="20 6 9 17 4 12" />
        </svg>
    {:else}
        <svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
            <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
        </svg>
    {/if}
</button>
