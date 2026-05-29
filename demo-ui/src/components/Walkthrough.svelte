<script lang="ts">
    import type { Step } from '../lib/tutorial'

    interface Props {
        tracks: string[]
        trackIndex: number
        onSelectTrack: (i: number) => void
        step: Step
        index: number
        total: number
        onPrev: () => void
        onNext: () => void
    }

    let { tracks, trackIndex, onSelectTrack, step, index, total, onPrev, onNext }: Props =
        $props()
</script>

<div class="flex items-start gap-4 border-b border-neutral-200 bg-brand-cyan/5 px-6 py-3">
    <div class="min-w-0 flex-1">
        <div class="mb-0.5 flex flex-wrap items-center gap-2 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">
            <span>walkthrough</span>
            <span class="text-neutral-300">·</span>
            <select
                value={trackIndex}
                onchange={(e) => onSelectTrack(Number(e.currentTarget.value))}
                class="rounded border border-neutral-300 bg-white px-1.5 py-0.5 text-[11px] font-semibold uppercase tracking-wide text-neutral-700"
            >
                {#each tracks as title, i (i)}
                    <option value={i}>{title}</option>
                {/each}
            </select>
            <span class="text-neutral-400">step {index + 1} / {total}</span>
            <span class="text-neutral-300">·</span>
            <span class="font-mono normal-case text-neutral-700">{step.title}</span>
        </div>
        <p class="text-sm text-neutral-600">{step.prose}</p>
    </div>
    <div class="flex shrink-0 items-center gap-2">
        <button
            type="button"
            disabled={index === 0}
            onclick={onPrev}
            class="rounded border border-neutral-300 bg-white px-3 py-1.5 text-sm text-neutral-700 hover:bg-neutral-50 disabled:opacity-40"
        >
            ‹ Prev
        </button>
        <button
            type="button"
            disabled={index >= total - 1}
            onclick={onNext}
            class="rounded border border-neutral-300 bg-white px-3 py-1.5 text-sm text-neutral-700 hover:bg-neutral-50 disabled:opacity-40"
        >
            Next ›
        </button>
    </div>
</div>
