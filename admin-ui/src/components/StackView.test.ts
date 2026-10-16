import { describe, it, expect, vi, beforeAll } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import StackView from './StackView.svelte'
import { opId, type Op } from '../lib/types'

// jsdom has no ResizeObserver; the diagram only needs it to track
// container width, so a no-op stub is enough.
beforeAll(() => {
    vi.stubGlobal(
        'ResizeObserver',
        class {
            observe() {}
            disconnect() {}
        }
    )
})

function op(scope: number, name: string, stack = 'web'): Op {
    return { stack, scope, name, txcl: '' }
}

// Scope 0 single, scope 10 parallel (two ops), scope 20 single.
const OPS = [op(0, 'auth'), op(10, 'fetch-a'), op(10, 'fetch-b'), op(20, 'respond')]

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
        for (const o of OPS) {
            expect(screen.getByRole('button', { name: o.name })).toBeInTheDocument()
        }
        expect(screen.getAllByRole('button')).toHaveLength(OPS.length)
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
        expect(screen.queryAllByRole('button')).toHaveLength(0)
    })

    it('renders a huge stack at full size, with no resolution clamp', () => {
        // 200 scopes, every 10th one 30 ops wide — the shape that used
        // to exceed WebKit's canvas limits.
        const big: Op[] = []
        for (let s = 0; s < 200; s++) {
            const n = s % 10 === 0 ? 30 : 1
            for (let i = 0; i < n; i++) big.push(op(s, `op-${s}-${i}`, 'www'))
        }
        render(StackView, {
            props: { stack: 'www', ops: big, lastDurations: {}, showInlineNewOp: false, onSelectOp: vi.fn() },
        })
        expect(screen.getAllByRole('button')).toHaveLength(big.length)

        // jsdom reports a 0-wide container → compact layout:
        // BOX_W 120, BOX_GAP_X 12, BOX_H 44, ROW_GAP 28, pads 44/44.
        const svg = screen.getByRole('group', { name: 'op stack diagram for www' })
        expect(svg.getAttribute('width')).toBe(String(30 * 120 + 29 * 12 + 32))
        expect(svg.getAttribute('height')).toBe(String(44 + 200 * (44 + 28) - 28 + 44))
    })
})
