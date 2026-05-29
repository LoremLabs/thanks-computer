<script lang="ts">
    import type { DemoStep as Step } from '../../lib/api'

    interface Props {
        tracks: string[]
        trackIndex: number
        onSelectTrack: (i: number) => void
        // All step titles for the currently-selected track. Rendered as a
        // clickable list so a click jumps straight to that step — Prev /
        // Next remain available as keyboard-friendly shortcuts at the
        // bottom of the sidebar.
        stepTitles: string[]
        // Currently active step (for highlight in the list + prose).
        step: Step
        index: number
        total: number
        onSelectStep: (i: number) => void
        onPrev: () => void
        onNext: () => void
    }

    let {
        tracks,
        trackIndex,
        onSelectTrack,
        stepTitles,
        step,
        index,
        total,
        onSelectStep,
        onPrev,
        onNext,
    }: Props = $props()

    // Row classes mirror admin-ui's SidebarNav vocabulary: same paddings,
    // same hover, same active highlight. The number gutter (text-right
    // w-6) keeps step titles flush regardless of digit count.
    function rowClass(i: number): string {
        const base =
            'flex w-full items-center gap-2 rounded px-2 py-1 text-left text-sm hover:bg-neutral-100'
        if (i === index) {
            return base + ' bg-brand-cyan/10 font-medium text-neutral-900'
        }
        return base + ' text-neutral-700'
    }
</script>

<div class="flex flex-col gap-3 px-2 py-3">
    <!-- Section header (matches admin's sidebar UPPERCASE label style). -->
    <div class="px-2 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">
        Walkthrough
    </div>

    <!-- Track picker. Compact select so the full sidebar width goes to
         the step list, which is what the user navigates day-to-day. -->
    <select
        value={trackIndex}
        onchange={(e) => onSelectTrack(Number((e.currentTarget as HTMLSelectElement).value))}
        class="mx-2 rounded border border-neutral-300 bg-white px-2 py-1 text-xs text-neutral-700 ring-neutral-300 focus:ring focus:ring-neutral-200 focus:outline-none"
    >
        {#each tracks as title, i (i)}
            <option value={i}>{title}</option>
        {/each}
    </select>

    <!-- Step list. Click-to-jump; current step highlighted. -->
    <nav class="flex flex-col">
        {#each stepTitles as title, i (i)}
            <button
                type="button"
                onclick={() => onSelectStep(i)}
                class={rowClass(i)}
                title={title}
            >
                <span class="w-6 shrink-0 text-right text-xs text-neutral-400">{i + 1}.</span>
                <span class="truncate">{title}</span>
            </button>
        {/each}
    </nav>

    <!-- Prev / Next as a compact footer — keyboard-friendly shortcuts
         for users who don't want to mouse over to the list. -->
    <div class="mx-2 flex gap-1 border-t border-neutral-200 pt-2">
        <button
            type="button"
            disabled={index === 0}
            onclick={onPrev}
            class="flex-1 rounded border border-neutral-300 bg-white px-2 py-1 text-xs text-neutral-700 hover:bg-neutral-50 disabled:opacity-40"
        >
            ‹ Prev
        </button>
        <button
            type="button"
            disabled={index >= total - 1}
            onclick={onNext}
            class="flex-1 rounded border border-neutral-300 bg-white px-2 py-1 text-xs text-neutral-700 hover:bg-neutral-50 disabled:opacity-40"
        >
            Next ›
        </button>
    </div>

    <!-- Step counter for orientation. Currently no prose surfaced — the
         step's `prose` field is available on `step` for a future
         in-sidebar or in-main hint when the design calls for it. -->
    <div class="mx-2 text-[11px] text-neutral-400">
        step {index + 1} / {total} · <span class="font-mono text-neutral-600">{step.title}</span>
    </div>
</div>
