<script lang="ts">
    import { diffLines } from 'diff'

    interface Props {
        from: string
        to: string
    }
    let { from, to }: Props = $props()

    // `diffLines` returns an ordered chunk list: each chunk is either
    // unchanged context, an addition, or a removal. We render each as
    // a span so multi-line chunks keep their newlines and the
    // `whitespace-pre-wrap` parent preserves indentation.
    const chunks = $derived(diffLines(from ?? '', to ?? ''))
</script>

<pre class="overflow-auto rounded border border-neutral-200 bg-white p-2 font-mono text-[11px] leading-snug whitespace-pre-wrap">{#each chunks as c, i (i)}<span
            class={c.added
                ? 'bg-emerald-50 text-emerald-900'
                : c.removed
                  ? 'bg-red-50 text-red-900 line-through'
                  : 'text-neutral-600'}>{c.value}</span>{/each}</pre>
