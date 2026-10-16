import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest'
import { fireEvent, render, screen, within } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import StackView from './StackView.svelte'
import { opId, type Op } from '../lib/types'

// jsdom has no ResizeObserver; the diagram only needs it to track
// pane width, so a no-op stub is enough.
beforeAll(() => {
    vi.stubGlobal(
        'ResizeObserver',
        class {
            observe() {}
            disconnect() {}
        }
    )
})

afterEach(() => {
    vi.restoreAllMocks()
})

function op(scope: number, name: string, stack = 'web'): Op {
    return { stack, scope, name, txcl: '' }
}

// Scope 0 single, scope 10 parallel (two ops), scope 20 single.
const OPS = [op(0, 'auth'), op(10, 'fetch-a'), op(10, 'fetch-b'), op(20, 'respond')]

// 200 scopes, every 10th one 30 ops wide — the shape that used to
// exceed WebKit's canvas limits.
function bigStack(): Op[] {
    const big: Op[] = []
    for (let s = 0; s < 200; s++) {
        const n = s % 10 === 0 ? 30 : 1
        for (let i = 0; i < n; i++) big.push(op(s, `op-${s}-${i}`, 'www'))
    }
    return big
}

function diagramOf(stack: string) {
    return screen.getByRole('group', { name: `op stack diagram for ${stack}` })
}

describe('StackView', () => {
    it('draws one box per op, with ms labels where trace data exists', () => {
        render(StackView, {
            props: {
                stack: 'web',
                ops: OPS,
                lastDurations: { [opId(OPS[1])]: 42 },
                showInlineNewOp: false,
                onSelectOp: vi.fn(),
            },
        })
        const svg = diagramOf('web')
        for (const o of OPS) {
            expect(within(svg).getByRole('button', { name: o.name })).toBeInTheDocument()
        }
        expect(within(svg).getAllByRole('button')).toHaveLength(OPS.length)
        expect(screen.getByText('42 ms')).toBeInTheDocument()
    })

    it('selects an op on click and on Enter', async () => {
        const onSelectOp = vi.fn()
        render(StackView, {
            props: { stack: 'web', ops: OPS, lastDurations: {}, showInlineNewOp: false, onSelectOp },
        })
        await userEvent.click(screen.getByText('fetch-b'))
        expect(onSelectOp).toHaveBeenLastCalledWith(OPS[2])

        screen.getByRole('button', { name: 'respond' }).focus()
        await userEvent.keyboard('{Enter}')
        expect(onSelectOp).toHaveBeenLastCalledWith(OPS[3])
        expect(onSelectOp).toHaveBeenCalledTimes(2)
    })

    it('is read-only without onSelectOp', () => {
        render(StackView, {
            props: { stack: 'web', ops: OPS, lastDurations: {}, showInlineNewOp: false },
        })
        expect(screen.getByText('auth')).toBeInTheDocument()
        expect(within(diagramOf('web')).queryAllByRole('button')).toHaveLength(0)
    })

    it('renders a huge stack at full size, with no resolution clamp', () => {
        const big = bigStack()
        render(StackView, {
            props: { stack: 'www', ops: big, lastDurations: {}, showInlineNewOp: false, onSelectOp: vi.fn() },
        })
        const svg = diagramOf('www')
        expect(within(svg).getAllByRole('button')).toHaveLength(big.length)

        // jsdom reports a 0-wide pane → compact layout at 100%:
        // BOX_W 120, BOX_GAP_X 12, BOX_H 44, ROW_GAP 28, pads 44/44.
        expect(svg.getAttribute('width')).toBe(String(30 * 120 + 29 * 12 + 32))
        expect(svg.getAttribute('height')).toBe(String(44 + 200 * (44 + 28) - 28 + 44))
    })
})

describe('StackView zoom', () => {
    // A 900px pane → regular (non-compact) layout. The big stack's
    // widest row then needs 56 + 30×160 + 29×24 + 32 = 5584 px, so the
    // width fit is 900 / 5584 ≈ 16%.
    const PANE = 900
    const WIDE = 5584

    function renderBig() {
        vi.spyOn(Element.prototype, 'clientWidth', 'get').mockReturnValue(PANE)
        render(StackView, {
            props: { stack: 'www', ops: bigStack(), lastDurations: {}, showInlineNewOp: false, onSelectOp: vi.fn() },
        })
        return diagramOf('www')
    }

    function renderedWidth(svg: HTMLElement): number {
        return parseFloat(svg.getAttribute('width') ?? '')
    }

    it('opens a wide stack fitted to the pane width', () => {
        const svg = renderBig()
        expect(renderedWidth(svg)).toBeCloseTo(PANE)
        // Natural geometry is unchanged; only the rendered size scales.
        expect(parseFloat(svg.getAttribute('viewBox')?.split(' ')[2] ?? '')).toBeCloseTo(WIDE)
        expect(screen.getByText('16%')).toBeInTheDocument()
        expect(screen.getByRole('button', { name: 'Fit' })).toBeDisabled()
    })

    it('opens a small stack at 100%, never zoomed in', () => {
        vi.spyOn(Element.prototype, 'clientWidth', 'get').mockReturnValue(PANE)
        render(StackView, {
            props: { stack: 'web', ops: OPS, lastDurations: {}, showInlineNewOp: false },
        })
        expect(screen.getByText('100%')).toBeInTheDocument()
        expect(renderedWidth(diagramOf('web'))).toBe(PANE)
    })

    it('steps with +/−, resets to 100%, and Fit returns to the width fit', async () => {
        const svg = renderBig()
        await userEvent.click(screen.getByRole('button', { name: 'Zoom in' }))
        expect(screen.getByText('20%')).toBeInTheDocument()
        expect(renderedWidth(svg)).toBeCloseTo(PANE * 1.25)

        await userEvent.click(screen.getByRole('button', { name: 'Zoom out' }))
        expect(screen.getByText('16%')).toBeInTheDocument()

        await userEvent.click(screen.getByRole('button', { name: 'Reset zoom to 100%' }))
        expect(renderedWidth(svg)).toBeCloseTo(WIDE)

        const fit = screen.getByRole('button', { name: 'Fit' })
        expect(fit).toBeEnabled()
        await userEvent.click(fit)
        expect(renderedWidth(svg)).toBeCloseTo(PANE)
        expect(fit).toBeDisabled()
    })

    it('zooms on ctrl+wheel (pinch) but scrolls on a plain wheel', async () => {
        const svg = renderBig()
        const pane = svg.closest('.overflow-auto') as HTMLElement

        await fireEvent.wheel(pane, { deltaY: -50 })
        expect(screen.getByText('16%')).toBeInTheDocument()

        // −50 px → 2^(50/100) ≈ ×1.41
        await fireEvent.wheel(pane, { deltaY: -50, ctrlKey: true })
        expect(renderedWidth(svg)).toBeCloseTo(PANE * 2 ** 0.5)
    })
})
