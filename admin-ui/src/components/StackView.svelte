<script lang="ts">
    import { onDestroy, onMount } from 'svelte'
    import { store } from '../lib/store.svelte'
    import { groupOps, type StackGroup } from '../lib/tree'
    import { opId, type Op } from '../lib/types'
    import NewOpForm from './NewOpForm.svelte'

    interface Props {
        stack: string
        ops: Op[]
        lastDurations: Record<string, number>
        // Time spent in this stack on the most recent trace that
        // touched it — i.e. the sum of step.duration_ms across this
        // stack's steps in that trace, NOT the total trace duration.
        // For a stack that just EXECs into another stack, this
        // captures when execution actually left the stack rather
        // than waiting for the whole downstream chain to finish.
        // Rendered next to the "output" label. Undefined when no
        // trace data is available.
        stackTotalMs?: number
        // Focused version + draft status for the +Add op form. Auto-
        // draft semantics live in store.createOp; this just tells the
        // form whether to show the "creates a new draft" hint.
        version?: number
        isDraft?: boolean
        // Optional click handler. When omitted (e.g. read-only diagrams
        // in the demo's boot-stack view), canvas clicks no-op silently.
        onSelectOp?: (op: Op) => void
        // Gate the inline `+ Add op` button + form. Defaults to true so
        // admin's normal ops view stays unchanged; the demo's Runner
        // passes `false` to hide author-only affordances.
        showInlineNewOp?: boolean
        // Accepted for parity with the demo-ui port (visual selection
        // indicator). Currently unused in this canvas renderer; click
        // semantics drive selection via `onSelectOp` instead.
        selected?: string
    }

    let {
        stack,
        ops,
        lastDurations,
        stackTotalMs,
        version,
        isDraft = false,
        onSelectOp,
        showInlineNewOp = true,
        selected: _selected,
    }: Props = $props()

    // Inline +Add op form toggle. Local component state — App.svelte
    // doesn't need to know whether we're mid-add.
    let adding = $state(false)

    // "12 ms" formatting. Sub-millisecond values get the "<1 ms" label
    // (the trace recorder rounds to milliseconds anyway, but this keeps
    // a tidy display if a future timer reports a finer value).
    function formatMs(n: number): string {
        if (!isFinite(n) || n < 0) return ''
        if (n < 1) return '<1 ms'
        if (n < 10) return n.toFixed(1) + ' ms'
        return Math.round(n) + ' ms'
    }

    const myGroup = $derived<StackGroup | undefined>(
        groupOps(ops).find((g) => g.stack === stack)
    )

    // Layout dimensions flex with container width: a single "compact"
    // tier kicks in below 640px (Tailwind's sm breakpoint). On
    // compact the scope-number column is dropped entirely, boxes
    // shrink, paddings tighten, and the duration bar narrows so it
    // still reads inside the smaller card.
    interface Layout {
        LABEL_X: number
        LABEL_W: number
        BOX_W: number
        BOX_H: number
        BOX_GAP_X: number
        ROW_GAP: number
        TOP_PAD: number
        BOTTOM_PAD: number
        SPINE_EXTRA: number
        BRANCH_OFFSET: number
        BAR_PAD: number
        BAR_W: number
        BAR_H: number
        BAR_FULL_SCALE_MS: number
        SHOW_LABELS: boolean
        NAME_FONT_PX: number
        PAGE_PAD: number
    }

    const COMPACT_BREAKPOINT = 640

    function layoutFor(width: number): Layout {
        const compact = width < COMPACT_BREAKPOINT
        return {
            LABEL_X: compact ? 0 : 28,
            LABEL_W: compact ? 0 : 56,
            BOX_W: compact ? 120 : 160,
            BOX_H: compact ? 44 : 52,
            BOX_GAP_X: compact ? 12 : 24,
            ROW_GAP: compact ? 28 : 40,
            TOP_PAD: compact ? 44 : 72,
            BOTTOM_PAD: compact ? 44 : 72,
            SPINE_EXTRA: compact ? 28 : 40,
            BRANCH_OFFSET: compact ? 10 : 14,
            BAR_PAD: compact ? 6 : 10,
            BAR_W: compact ? 60 : 92,
            BAR_H: 5,
            BAR_FULL_SCALE_MS: 1000,
            SHOW_LABELS: !compact,
            NAME_FONT_PX: compact ? 12 : 13,
            PAGE_PAD: compact ? 8 : 32,
        }
    }

    // Rendered as SVG rather than one canvas: the browser rasterizes
    // SVG in tiles near the viewport at device resolution, so a huge
    // stack (www ≈ 5584×17328 CSS px) stays sharp without a single
    // backing bitmap that would blow WebKit's canvas size limits.
    let containerEl: HTMLDivElement | undefined = $state()
    let containerW = $state(800)

    function diagramHeight(L: Layout): number {
        const rows = myGroup?.scopes.length ?? 0
        if (rows === 0) return 200
        // Reserve a little extra height when we'll be drawing the
        // total-ms line under "output" so it doesn't clip the diagram.
        const extra = typeof stackTotalMs === 'number' ? 20 : 0
        return L.TOP_PAD + rows * (L.BOX_H + L.ROW_GAP) - L.ROW_GAP + L.BOTTOM_PAD + extra
    }

    // Widest row in CSS pixels. Used so the diagram can grow to fit a
    // wide parallel-op row even when the container is narrower (the
    // outer div has overflow-auto so it'll scroll horizontally).
    function maxRowWidth(L: Layout): number {
        if (!myGroup) return 0
        let max = 0
        for (const row of myGroup.scopes) {
            const w = row.ops.length * L.BOX_W + (row.ops.length - 1) * L.BOX_GAP_X
            if (w > max) max = w
        }
        return max
    }

    interface BoxGeom {
        op: Op
        id: string
        x: number
        // Log-scale bar fill fraction + label; undefined when no
        // trace data is available for this op.
        dur?: { frac: number; label: string }
    }

    interface RowGeom {
        scope: number
        y: number
        // Branch/merge bars + legs for parallel rows; '' for single-op
        // rows, which sit on the spine instead.
        branchD: string
        boxes: BoxGeom[]
    }

    interface Diagram {
        L: Layout
        width: number
        height: number
        spineX: number
        spineTop: number
        spineBottom: number
        spineD: string
        rows: RowGeom[]
    }

    // Log-scale bar fill — log(ms+1) / log(MAX+1) — so each ~10× jump
    // in ms produces roughly equal growth in bar width. That keeps
    // single-millisecond ops visible while still letting a 1-second op
    // fill the whole bar. The +1 shift makes ms=0 map to 0 cleanly
    // without log(0) = -∞.
    function barFrac(ms: number, L: Layout): number {
        const raw = Math.log(Math.max(ms, 0) + 1) / Math.log(L.BAR_FULL_SCALE_MS + 1)
        return Math.min(1, Math.max(0, raw))
    }

    // All geometry, in CSS pixels. Re-derives on any reactive read
    // change (stack, ops, container width, last-durations data
    // arriving, or stack total updating).
    const diagram = $derived.by<Diagram | undefined>(() => {
        if (!myGroup) return undefined
        const L = layoutFor(containerW)
        // Width = max(container, single-box min, widest row). The
        // widest-row term means a parallel multi-op row that wouldn't
        // otherwise fit grows the diagram; the outer `overflow-auto`
        // div then provides horizontal scroll.
        const width = Math.max(
            containerW,
            L.LABEL_W + L.BOX_W + 32,
            L.LABEL_W + maxRowWidth(L) + 32
        )
        const height = diagramHeight(L)
        const spineX = (L.LABEL_X + L.LABEL_W + width) / 2

        const rows: RowGeom[] = myGroup.scopes.map((row, idx) => {
            const y = L.TOP_PAD + idx * (L.BOX_H + L.ROW_GAP)
            const n = row.ops.length
            const totalW = n * L.BOX_W + (n - 1) * L.BOX_GAP_X
            const startX = spineX - totalW / 2
            const boxes: BoxGeom[] = row.ops.map((op, i) => {
                const ms = lastDurations[opId(op)]
                return {
                    op,
                    id: opId(op),
                    x: startX + i * (L.BOX_W + L.BOX_GAP_X),
                    dur:
                        typeof ms === 'number'
                            ? { frac: barFrac(ms, L), label: formatMs(ms) }
                            : undefined,
                }
            })

            // Branching / merging bars for parallel rows: a top bar
            // across the box centers with a leg down to each box, and
            // the mirror image below.
            let branchD = ''
            if (n > 1) {
                const branchY = y - L.BRANCH_OFFSET
                const mergeY = y + L.BOX_H + L.BRANCH_OFFSET
                const centers = boxes.map((b) => b.x + L.BOX_W / 2)
                branchD = `M${centers[0]} ${branchY}H${centers[n - 1]}`
                for (const cx of centers) {
                    branchD += `M${cx} ${branchY}V${y}M${cx} ${y + L.BOX_H}V${mergeY}`
                }
                branchD += `M${centers[0]} ${mergeY}H${centers[n - 1]}`
            }
            return { scope: row.scope, y, branchD, boxes }
        })

        // Spine extents — derived from the row stack, NOT from the
        // height, so adding label room below doesn't stretch the
        // spine's bottom neck.
        const rowsEndY =
            rows.length === 0
                ? L.TOP_PAD
                : L.TOP_PAD + rows.length * (L.BOX_H + L.ROW_GAP) - L.ROW_GAP
        const spineTop = L.TOP_PAD - L.SPINE_EXTRA
        const spineBottom = rowsEndY + L.SPINE_EXTRA

        // Spine: segmented so it *skips* multi-op rows — the branch/
        // merge bars around those rows take over the visual flow there,
        // and a through-line would lie inside an empty gap between
        // siblings.
        let spineD = ''
        let cursor = spineTop
        for (const r of rows) {
            if (r.boxes.length < 2) continue
            spineD += `M${spineX} ${cursor}V${r.y - L.BRANCH_OFFSET}`
            cursor = r.y + L.BOX_H + L.BRANCH_OFFSET
        }
        spineD += `M${spineX} ${cursor}V${spineBottom}`

        return { L, width, height, spineX, spineTop, spineBottom, spineD, rows }
    })

    function handleBoxKey(e: KeyboardEvent, op: Op) {
        if (e.key !== 'Enter' && e.key !== ' ') return
        e.preventDefault()
        onSelectOp?.(op)
    }

    let ro: ResizeObserver | undefined
    onMount(() => {
        if (!containerEl) return
        ro = new ResizeObserver((entries) => {
            for (const e of entries) {
                containerW = e.contentRect.width
            }
        })
        ro.observe(containerEl)
        containerW = containerEl.getBoundingClientRect().width
    })
    onDestroy(() => ro?.disconnect())
</script>

<!-- One op box: rounded card, name, and (when trace data exists) the
     duration strip — a left-aligned track + cyan fill with the
     formatted ms flush right at the same vertical center. -->
{#snippet boxBody(b: BoxGeom, y: number, L: Layout)}
    <rect
        x={b.x}
        {y}
        width={L.BOX_W}
        height={L.BOX_H}
        rx="8"
        class="fill-white stroke-neutral-400 group-focus-visible:stroke-brand-cyan group-focus-visible:stroke-2"
    />
    <!-- Op name. Centered when we have no duration to show; otherwise
         nudged up so the ms can sit beneath it. -->
    <text
        x={b.x + L.BOX_W / 2}
        y={b.dur ? y + L.BOX_H * 0.34 : y + L.BOX_H / 2}
        text-anchor="middle"
        dominant-baseline="central"
        font-size={L.NAME_FONT_PX}
        class="fill-neutral-800 font-sans">{b.op.name}</text
    >
    {#if b.dur}
        {@const barY = y + L.BOX_H - 14}
        <rect
            x={b.x + L.BAR_PAD}
            y={barY}
            width={L.BAR_W}
            height={L.BAR_H}
            class="fill-neutral-200"
        />
        {#if b.dur.frac > 0}
            <!-- Fill — brand cyan to tie back to the chassis banner colors. -->
            <rect
                x={b.x + L.BAR_PAD}
                y={barY}
                width={L.BAR_W * b.dur.frac}
                height={L.BAR_H}
                class="fill-cyan-400"
            />
        {/if}
        <text
            x={b.x + L.BOX_W - L.BAR_PAD}
            y={barY + L.BAR_H / 2}
            text-anchor="end"
            dominant-baseline="central"
            font-size="10"
            class="fill-neutral-600 font-mono">{b.dur.label}</text
        >
    {/if}
{/snippet}

<div bind:this={containerEl} class="h-full w-full overflow-auto">
    <div class="px-1 py-2 sm:px-2 sm:py-3">
        <header class="mb-2 flex items-center justify-between gap-2 px-1 sm:px-2">
            <h2 class="font-mono text-sm font-semibold text-neutral-900 sm:text-base">
                {stack}
            </h2>
            {#if showInlineNewOp}
                <button
                    type="button"
                    class="rounded border border-brand-cyan/40 bg-brand-cyan/10 px-2 py-0.5 text-xs text-neutral-900 hover:bg-brand-cyan/20"
                    onclick={() => (adding = !adding)}
                >
                    {adding ? 'Cancel' : '+ Add op'}
                </button>
            {/if}
        </header>
        {#if adding && showInlineNewOp}
            <div class="mb-2 px-1 sm:px-2">
                <NewOpForm
                    {stack}
                    {isDraft}
                    existingOps={ops}
                    onSubmit={async (scope, name, content) => {
                        if (typeof version !== 'number') {
                            throw new Error('no focused version')
                        }
                        await store.createOp(stack, version, scope, name, content)
                        adding = false
                    }}
                    onCancel={() => (adding = false)}
                />
            </div>
        {/if}
        {#if diagram}
            {@const L = diagram.L}
            <svg
                width={diagram.width}
                height={diagram.height}
                viewBox="0 0 {diagram.width} {diagram.height}"
                class="block"
                role="group"
                aria-label="op stack diagram for {stack}"
            >
                <path d={diagram.spineD} fill="none" class="stroke-neutral-300" />
                <!-- "input" label — sits just above the spine's upper end. -->
                <text
                    x={diagram.spineX}
                    y={diagram.spineTop - 8}
                    text-anchor="middle"
                    font-size="12"
                    class="fill-neutral-500 font-sans">input</text
                >
                {#each diagram.rows as row (row.scope)}
                    <!-- Scope label (left, lighter grey, mono). Skipped
                         in compact mode to reclaim that column. -->
                    {#if L.SHOW_LABELS}
                        <text
                            x={L.LABEL_X}
                            y={row.y + L.BOX_H / 2}
                            dominant-baseline="central"
                            font-size="13"
                            class="fill-neutral-400 font-mono font-semibold">{row.scope}</text
                        >
                    {/if}
                    {#if row.branchD}
                        <path d={row.branchD} fill="none" class="stroke-neutral-300" />
                    {/if}
                    <!-- Boxes after the bars so their white fill masks
                         any line beneath. -->
                    {#each row.boxes as b (b.id)}
                        {#if onSelectOp}
                            <g
                                role="button"
                                tabindex="0"
                                aria-label={b.op.name}
                                class="group cursor-pointer outline-none"
                                onclick={() => onSelectOp?.(b.op)}
                                onkeydown={(e) => handleBoxKey(e, b.op)}
                            >
                                {@render boxBody(b, row.y, L)}
                            </g>
                        {:else}
                            <g>{@render boxBody(b, row.y, L)}</g>
                        {/if}
                    {/each}
                {/each}
                <!-- "output" label — just below the spine's lower end
                     (12 px text, top edge 8 px below), plus the last-run
                     total in the lighter mono style when we have one. -->
                <text
                    x={diagram.spineX}
                    y={diagram.spineBottom + 8 + 6}
                    text-anchor="middle"
                    dominant-baseline="central"
                    font-size="12"
                    class="fill-neutral-500 font-sans">output</text
                >
                {#if typeof stackTotalMs === 'number'}
                    <text
                        x={diagram.spineX}
                        y={diagram.spineBottom + 8 + 16 + 5.5}
                        text-anchor="middle"
                        dominant-baseline="central"
                        font-size="11"
                        class="fill-neutral-400 font-mono">{formatMs(stackTotalMs)}</text
                    >
                {/if}
            </svg>
        {:else}
            <p class="mt-2 px-1 text-sm italic text-neutral-400 sm:px-2">
                no ops yet — use + Add op
            </p>
        {/if}
    </div>
</div>
