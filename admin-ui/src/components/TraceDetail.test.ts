// A trace step's name links to the op it ran (#ops/<stack>/<scope>/<name>),
// for the tenant's own stacks only; clicking it navigates without toggling
// the step's in/out row.

import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import TraceDetail from './TraceDetail.svelte'
import { store, type TraceCachedEvent } from '../lib/store.svelte'

const RID = 'CfdKKV8Cwk9P2Ac8TduDA'
const DIGEST = '3cee8f8dc76d5e1c04156da5ec03fe2dc8f347f2b742386e19e02d55235683d2'

function seed() {
    store.state.stacks = [
        { name: '_inspect', created_at: '2026-06-01T00:00:00Z', active_version: 1 },
        { name: 'node-demo/_websocket', created_at: '2026-06-01T00:00:00Z', active_version: 1 },
    ]
    store.cacheLiveTrace({
        rid: RID,
        cursor: RID,
        stack: 'boot/0',
        route: '_inspect/0',
        steps: [
            { stack: 'boot', scope: 0, name: 'detect', operation: 'txco://detect-tenant', started_at: '2026-10-02T00:00:00.001Z' },
            { stack: '_inspect', scope: 396, name: 'route', operation: 'txco://route', started_at: '2026-10-02T00:00:00.002Z', in: { a: 1 } },
            { stack: 'node-demo/_websocket', scope: 100, name: 'close', started_at: '2026-10-02T00:00:00.003Z' },
            { stack: '_inspect', scope: 104, name: 'plan', operation: `compute://sha256/${DIGEST}`, started_at: '2026-10-02T00:00:00.004Z' },
            { stack: 'boot', scope: 5, name: 'calc', operation: `compute://sha256/${DIGEST}`, started_at: '2026-10-02T00:00:00.005Z' },
        ],
    } as unknown as TraceCachedEvent)
}

beforeEach(() => {
    vi.restoreAllMocks()
    seed()
})

describe('TraceDetail step links', () => {
    it('links a tenant step to its op definition', () => {
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        expect(screen.getByRole('link', { name: '0396-route' })).toHaveAttribute('href', '#ops/_inspect/396/route')
        expect(screen.getByRole('link', { name: '0100-close' })).toHaveAttribute(
            'href',
            '#ops/node-demo/_websocket/100/close',
        )
    })

    it('leaves boot and other non-tenant steps as plain text', () => {
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        expect(screen.getByText('0000-detect')).toBeInTheDocument()
        expect(screen.queryByRole('link', { name: '0000-detect' })).toBeNull()
    })

    it('clicking the link does not expand the step', async () => {
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        await userEvent.click(screen.getByRole('link', { name: '0396-route' }))
        expect(screen.queryByText('in')).toBeNull()
        await userEvent.click(screen.getByText('txco://route'))
        expect(screen.getByText('in')).toBeInTheDocument()
    })
})

describe('TraceDetail compute operations', () => {
    it("links a tenant step's compute ref to its source, through that step's stack", () => {
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        const links = screen.getAllByRole('link', { name: `compute://sha256/${DIGEST}` })
        expect(links).toHaveLength(1)
        expect(links[0]).toHaveAttribute('href', `#compute/_inspect/${DIGEST}`)
    })
})
