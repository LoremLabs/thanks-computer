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

    let canvasEl: HTMLCanvasElement | undefined = $state()
    let containerEl: HTMLDivElement | undefined = $state()
    let containerW = $state(800)

    // Hit boxes computed during render; consumed by the click handler.
    // Not reactive — read on click, written on render.
    let hits: Array<{ op: Op; x: number; y: number; w: number; h: number }> = []

    function canvasHeight(L: Layout): number {
        const rows = myGroup?.scopes.length ?? 0
        if (rows === 0) return 200
        // Reserve a little extra height when we'll be drawing the
        // total-ms line under "output" so it doesn't clip the canvas.
        const extra = typeof stackTotalMs === 'number' ? 20 : 0
        return L.TOP_PAD + rows * (L.BOX_H + L.ROW_GAP) - L.ROW_GAP + L.BOTTOM_PAD + extra
    }

    // Widest row in CSS pixels. Used so the canvas can grow to fit a
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

    function render() {
        if (!canvasEl || !containerEl || !myGroup) return
        const L = layoutFor(containerW)
        // Canvas width = max(container, single-box min, widest row).
        // The widest-row term means a parallel multi-op row that
        // wouldn't otherwise fit grows the canvas; the outer
        // `overflow-auto` div then provides horizontal scroll.
        const widestRow = maxRowWidth(L)
        const cssW = Math.max(
            containerW,
            L.LABEL_W + L.BOX_W + 32,
            L.LABEL_W + widestRow + 32
        )
        const cssH = canvasHeight(L)
        const dpr = window.devicePixelRatio || 1
        canvasEl.width = Math.floor(cssW * dpr)
        canvasEl.height = Math.floor(cssH * dpr)
        canvasEl.style.width = cssW + 'px'
        canvasEl.style.height = cssH + 'px'

        const ctx = canvasEl.getContext('2d')
        if (!ctx) return
        ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
        ctx.clearRect(0, 0, cssW, cssH)

        const spineX = (L.LABEL_X + L.LABEL_W + cssW) / 2

        // Pre-compute row geometry so the spine can be drawn in
        // segments that *skip* multi-op rows — the branch/merge bars
        // around those rows take over the visual flow there, and a
        // through-line would lie inside an empty gap between siblings.
        const rowInfo = myGroup.scopes.map((row, idx) => ({
            row,
            y: L.TOP_PAD + idx * (L.BOX_H + L.ROW_GAP),
            multi: row.ops.length > 1,
        }))
        // Spine extents — derived from the row stack, NOT from cssH,
        // so adding label room below the canvas doesn't stretch the
        // spine's bottom neck.
        const rowsEndY =
            rowInfo.length === 0
                ? L.TOP_PAD
                : L.TOP_PAD + rowInfo.length * (L.BOX_H + L.ROW_GAP) - L.ROW_GAP
        const spineTop = L.TOP_PAD - L.SPINE_EXTRA
        const spineBottom = rowsEndY + L.SPINE_EXTRA

        // Spine: segmented around multi-op rows.
        ctx.strokeStyle = '#d4d4d4'
        ctx.lineWidth = 1
        ctx.beginPath()
        let cursor = spineTop
        for (const ri of rowInfo) {
            if (!ri.multi) continue
            ctx.moveTo(spineX, cursor)
            ctx.lineTo(spineX, ri.y - L.BRANCH_OFFSET)
            cursor = ri.y + L.BOX_H + L.BRANCH_OFFSET
        }
        ctx.moveTo(spineX, cursor)
        ctx.lineTo(spineX, spineBottom)
        ctx.stroke()

        // "input" label — sits just above the spine's upper end.
        ctx.fillStyle = '#737373'
        ctx.font = '12px ui-sans-serif, system-ui, sans-serif'
        ctx.textAlign = 'center'
        ctx.textBaseline = 'alphabetic'
        ctx.fillText('input', spineX, spineTop - 8)

        // Rows + boxes
        hits = []
        let y = L.TOP_PAD
        for (const row of myGroup.scopes) {
            // Scope label (left, lighter grey, Roboto Mono via font-mono
            // token). Skipped in compact mode to reclaim that column.
            if (L.SHOW_LABELS) {
                ctx.fillStyle = '#a3a3a3'
                ctx.font = '600 13px "Roboto Mono", ui-monospace, monospace'
                ctx.textAlign = 'left'
                ctx.textBaseline = 'middle'
                ctx.fillText(String(row.scope), L.LABEL_X, y + L.BOX_H / 2)
            }

            // Compute box centers up-front so the branch/merge bars can
            // span them and each box gets a vertical leg drawn.
            const n = row.ops.length
            const totalW = n * L.BOX_W + (n - 1) * L.BOX_GAP_X
            const startX = spineX - totalW / 2
            const boxCenters: number[] = []
            for (let i = 0; i < n; i++) {
                boxCenters.push(startX + i * (L.BOX_W + L.BOX_GAP_X) + L.BOX_W / 2)
            }

            // Branching / merging bars for parallel rows. Single-op
            // rows sit on the spine and don't need the extra fan-out —
            // the continuous spine line already plays the same role.
            if (n > 1) {
                const branchY = y - L.BRANCH_OFFSET
                const mergeY = y + L.BOX_H + L.BRANCH_OFFSET
                ctx.strokeStyle = '#d4d4d4'
                ctx.lineWidth = 1
                ctx.beginPath()
                // Top horizontal bar
                ctx.moveTo(boxCenters[0], branchY)
                ctx.lineTo(boxCenters[n - 1], branchY)
                // Vertical legs from bar down to each box top
                for (const cx of boxCenters) {
                    ctx.moveTo(cx, branchY)
                    ctx.lineTo(cx, y)
                }
                // Vertical legs from each box bottom down to the merge bar
                for (const cx of boxCenters) {
                    ctx.moveTo(cx, y + L.BOX_H)
                    ctx.lineTo(cx, mergeY)
                }
                // Bottom horizontal bar
                ctx.moveTo(boxCenters[0], mergeY)
                ctx.lineTo(boxCenters[n - 1], mergeY)
                ctx.stroke()
            }

            // Boxes (drawn last so their white fill masks any line beneath).
            for (let i = 0; i < n; i++) {
                const op = row.ops[i]
                const bx = startX + i * (L.BOX_W + L.BOX_GAP_X)
                ctx.fillStyle = '#ffffff'
                ctx.strokeStyle = '#a3a3a3'
                ctx.lineWidth = 1
                roundRect(ctx, bx, y, L.BOX_W, L.BOX_H, 8)
                ctx.fill()
                ctx.stroke()

                // Op name. Centered when we have no duration to show;
                // otherwise nudged up so the ms can sit beneath it.
                const dur = lastDurations[opId(op)]
                const hasDur = typeof dur === 'number'
                ctx.fillStyle = '#262626'
                ctx.font = `${L.NAME_FONT_PX}px ui-sans-serif, system-ui, sans-serif`
                ctx.textAlign = 'center'
                ctx.textBaseline = 'middle'
                ctx.fillText(op.name, bx + L.BOX_W / 2, hasDur ? y + L.BOX_H * 0.34 : y + L.BOX_H / 2)

                if (hasDur) drawDurationBar(ctx, bx, y, dur, L)

                hits.push({ op, x: bx, y, w: L.BOX_W, h: L.BOX_H })
            }
            y += L.BOX_H + L.ROW_GAP
        }

        // "output" label — sits just below the spine's lower end.
        // When we have a "last run total" for this stack, append it
        // in the same lighter mono style used on op-box bars. No
        // visual bar here — just the number.
        ctx.fillStyle = '#737373'
        ctx.font = '12px ui-sans-serif, system-ui, sans-serif'
        ctx.textAlign = 'center'
        ctx.textBaseline = 'top'
        const outputLabelY = spineBottom + 8
        ctx.fillText('output', spineX, outputLabelY)
        if (typeof stackTotalMs === 'number') {
            ctx.fillStyle = '#a3a3a3'
            ctx.font = '11px "Roboto Mono", ui-monospace, monospace'
            ctx.fillText(formatMs(stackTotalMs), spineX, outputLabelY + 16)
        }
    }

    // drawDurationBar renders the bottom strip of an op box: a left-
    // aligned track + cyan fill, with the formatted ms label flush
    // right at the same vertical center.
    //
    // Scale is logarithmic — log(ms+1) / log(MAX+1) — so each ~10×
    // jump in ms produces roughly equal growth in bar width. That
    // keeps single-millisecond ops visible while still letting a
    // 1-second op fill the whole bar. The +1 shift makes ms=0 map to
    // 0 cleanly without log(0) = -∞.
    function drawDurationBar(
        ctx: CanvasRenderingContext2D,
        bx: number,
        boxTopY: number,
        ms: number,
        L: Layout
    ) {
        const barX = bx + L.BAR_PAD
        const barY = boxTopY + L.BOX_H - 14
        const raw = Math.log(Math.max(ms, 0) + 1) / Math.log(L.BAR_FULL_SCALE_MS + 1)
        const frac = Math.min(1, Math.max(0, raw))

        // Track (full extent — visual reference for the 1s scale).
        ctx.fillStyle = '#e5e5e5'
        ctx.fillRect(barX, barY, L.BAR_W, L.BAR_H)

        // Fill — brand cyan to tie back to the chassis banner colors.
        if (frac > 0) {
            ctx.fillStyle = '#22d3ee'
            ctx.fillRect(barX, barY, L.BAR_W * frac, L.BAR_H)
        }

        // Label, right-aligned to the box inner edge.
        ctx.fillStyle = '#525252'
        ctx.font = '10px "Roboto Mono", ui-monospace, monospace'
        ctx.textAlign = 'right'
        ctx.textBaseline = 'middle'
        ctx.fillText(formatMs(ms), bx + L.BOX_W - L.BAR_PAD, barY + L.BAR_H / 2)
    }

    function roundRect(
        ctx: CanvasRenderingContext2D,
        x: number,
        y: number,
        w: number,
        h: number,
        r: number
    ) {
        ctx.beginPath()
        ctx.moveTo(x + r, y)
        ctx.lineTo(x + w - r, y)
        ctx.quadraticCurveTo(x + w, y, x + w, y + r)
        ctx.lineTo(x + w, y + h - r)
        ctx.quadraticCurveTo(x + w, y + h, x + w - r, y + h)
        ctx.lineTo(x + r, y + h)
        ctx.quadraticCurveTo(x, y + h, x, y + h - r)
        ctx.lineTo(x, y + r)
        ctx.quadraticCurveTo(x, y, x + r, y)
    }

    function handleClick(e: MouseEvent) {
        if (!canvasEl) return
        const rect = canvasEl.getBoundingClientRect()
        const px = e.clientX - rect.left
        const py = e.clientY - rect.top
        for (const h of hits) {
            if (px >= h.x && px <= h.x + h.w && py >= h.y && py <= h.y + h.h) {
                if (onSelectOp) onSelectOp(h.op)
                return
            }
        }
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

    // Re-render on any reactive read change (stack, ops, container
    // width, last-durations data arriving, or stack total updating).
    $effect(() => {
        // Touch reactive sources so the effect re-runs when they change.
        stack
        ops
        containerW
        myGroup
        lastDurations
        stackTotalMs
        if (canvasEl && containerEl) render()
    })
</script>

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
        {#if myGroup}
            <canvas
                bind:this={canvasEl}
                onclick={handleClick}
                class="block cursor-pointer"
                aria-label="op stack diagram for {stack}"
            ></canvas>
        {:else}
            <p class="mt-2 px-1 text-sm italic text-neutral-400 sm:px-2">
                no ops yet — use + Add op
            </p>
        {/if}
    </div>
</div>
