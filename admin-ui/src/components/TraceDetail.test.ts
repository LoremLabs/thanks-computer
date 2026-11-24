// A trace step's name and its operation link to the op it ran
// (#ops/<stack>/<scope>/<name>), for the tenant's own stacks only; a
// compute's operation opens its source instead. Clicking a link navigates
// without toggling the step's in/out row.

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import TraceDetail from './TraceDetail.svelte'
import * as api from '../lib/api'
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

    it("links a tenant step's operation to its op definition", () => {
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        expect(screen.getByRole('link', { name: 'txco://route' })).toHaveAttribute('href', '#ops/_inspect/396/route')
    })

    it('leaves boot and other non-tenant steps as plain text', () => {
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        expect(screen.getByText('0000-detect')).toBeInTheDocument()
        expect(screen.queryByRole('link', { name: '0000-detect' })).toBeNull()
        expect(screen.getByText('txco://detect-tenant')).toBeInTheDocument()
        expect(screen.queryByRole('link', { name: 'txco://detect-tenant' })).toBeNull()
    })

    it('clicking a link does not expand the step', async () => {
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        await userEvent.click(screen.getByRole('link', { name: '0396-route' }))
        await userEvent.click(screen.getByRole('link', { name: 'txco://route' }))
        expect(screen.queryByText('in')).toBeNull()
        await userEvent.click(screen.getByRole('link', { name: 'txco://route' }).closest('td')!)
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

describe('TraceDetail links a step to the rule that ran', () => {
    it('opens the version the step recorded, not the active one', () => {
        store.state.stacks = [{ name: 'loop', created_at: '2026-06-01T00:00:00Z', active_version: 120 }]
        store.cacheLiveTrace({
            rid: 'CbVersioned',
            cursor: 'CbVersioned',
            steps: [
                { stack: 'loop', scope: 7100, name: 'chat', operation: 'ai://chat', stack_version: 116, started_at: '2026-11-06T00:00:00.100Z' },
                { stack: 'loop', scope: 7200, name: 'old', started_at: '2026-11-06T00:00:00.200Z' },
            ],
        } as unknown as TraceCachedEvent)
        render(TraceDetail, { props: { rid: 'CbVersioned', onBack: vi.fn() } })
        expect(screen.getByRole('link', { name: '7100-chat' })).toHaveAttribute('href', '#ops/loop/v116/7100/chat')
        expect(screen.getByRole('link', { name: '7200-old' })).toHaveAttribute('href', '#ops/loop/7200/old')
    })
})

describe('TraceDetail, a run seen from its resume trace', () => {
    const LONG = 'resume-run_x-loop_502'

    function seedRun() {
        store.state.stacks = []
        store.cacheLiveTrace({
            rid: LONG,
            cursor: LONG,
            stack: 'loop/502',
            tags: { pony: 'eager.moss', run: 'run_abc' },
            steps: [
                { stack: 'pony-agent', scope: 600, name: 'decide', operation: 'cap://ai.chat', transport: 'cap', started_at: '2026-11-06T00:00:00.100Z' },
                { stack: 'loop', scope: 7101, name: 'work', operation: 'ai://chat', transport: 'ai', started_at: '2026-11-06T00:00:00.200Z',
                  model: { kind: 'chat', provider: 'openai', model: 'gpt-5.4', tokens_in: 13973, tokens_out: 33, latency_ms: 1948 }, body_ref: 'k#0-10', input_bytes: 100 },
                { stack: 'loop', scope: 7103, name: 'judge', operation: 'ai://decide', transport: 'ai', started_at: '2026-11-06T00:00:00.300Z',
                  model: { kind: 'decide', model: 'jev', tokens_in: 989, tokens_out: 101, retries: 1 } },
                { stack: 'loop', scope: 7110, name: 'refused', operation: 'ai://chat', transport: 'ai', started_at: '2026-11-06T00:00:00.400Z',
                  model: { kind: 'chat', error_code: 'txco_chat_no_key' } },
                { stack: 'loop', scope: 7200, name: 'kept', operation: 'txco://kv/get', transport: 'txco', started_at: '2026-11-06T00:00:00.500Z',
                  input_bytes: 2048, input_truncated: true, output_bytes: 10, output_truncated: true },
                { stack: 'loop', scope: 7300, name: 'gone', operation: 'txco://kv/get', transport: 'txco', started_at: '2026-11-06T00:00:00.600Z', body_ref: 'k#10-10' },
            ],
        } as unknown as TraceCachedEvent)
    }

    it('names the run and adds up its model calls', () => {
        seedRun()
        render(TraceDetail, { props: { rid: LONG, onBack: vi.fn() } })
        expect(screen.getByText('3 model calls,', { exact: false })).toBeInTheDocument()
        expect(screen.getByText('14,962 → 134')).toBeInTheDocument()
        expect(screen.getByText('eager.moss')).toBeInTheDocument()
        expect(screen.getByText('gpt-5.4 13,973 → 33')).toBeInTheDocument()
        expect(screen.getByText('txco_chat_no_key', { exact: false })).toBeInTheDocument()
    })

    it('shows only the model calls, numbered, and back', async () => {
        seedRun()
        const user = userEvent.setup()
        render(TraceDetail, { props: { rid: LONG, onBack: vi.fn() } })
        expect(screen.getByText('0600-decide')).toBeInTheDocument()

        await user.click(screen.getByRole('button', { name: 'show model calls' }))
        expect(screen.queryByText('0600-decide')).not.toBeInTheDocument()
        expect(screen.getByText('7101-work')).toBeInTheDocument()
        expect(screen.getByText('3.')).toBeInTheDocument()

        await user.click(screen.getByRole('button', { name: 'show all steps' }))
        expect(screen.getByText('0600-decide')).toBeInTheDocument()
    })

    it("fetches a step's bodies when it is opened, once", async () => {
        seedRun()
        const body = vi.spyOn(api, 'getTraceBody').mockResolvedValue({ in: { asked: 'what' }, out: { said: 'that' } })
        const user = userEvent.setup()
        render(TraceDetail, { props: { rid: LONG, onBack: vi.fn() } })

        const row = screen.getByText('7101-work')
        await user.click(row)
        expect(body).toHaveBeenCalledWith(store.state.currentTenant, LONG, 'k#0-10')
        expect(await screen.findByText(/asked/)).toBeInTheDocument()

        await user.click(row)
        await user.click(row)
        expect(body).toHaveBeenCalledTimes(1)
    })

    it('says so when a body was not kept, and when it cannot be fetched', async () => {
        seedRun()
        vi.spyOn(api, 'getTraceBody').mockResolvedValue(null)
        const user = userEvent.setup()
        render(TraceDetail, { props: { rid: LONG, onBack: vi.fn() } })

        await user.click(screen.getByText('7200-kept'))
        expect(screen.getByText('not kept, 2.0 KB')).toBeInTheDocument()

        await user.click(screen.getByText('7300-gone'))
        expect((await screen.findAllByText('not available: this body was not stored')).length).toBe(2)
    })
})

describe('TraceDetail steps through with the arrow keys', () => {
    const original = Element.prototype.scrollIntoView
    let scrolled: Element[] = []
    beforeEach(() => {
        scrolled = []
        Element.prototype.scrollIntoView = function (this: Element) {
            scrolled.push(this)
        }
    })
    afterEach(() => {
        Element.prototype.scrollIntoView = original
    })

    const row = (label: string) => screen.getByText(label).closest('tr')!
    const current = () => document.querySelector('tr[aria-current="true"]')
    const openRows = () => screen.queryAllByText('in').length

    it('opens the next step, closes the one it leaves, and scrolls to it', async () => {
        const user = userEvent.setup()
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })

        await user.keyboard('{ArrowRight}')
        expect(current()).toBe(row('0000-detect'))
        expect(openRows()).toBe(1)

        await user.keyboard('{ArrowRight}')
        expect(current()).toBe(row('0396-route'))
        expect(openRows()).toBe(1)
        expect(scrolled[scrolled.length - 1]).toBe(row('0396-route'))

        await user.keyboard('{ArrowLeft}')
        expect(current()).toBe(row('0000-detect'))
        expect(openRows()).toBe(1)
    })

    it('starts at the last step with ←, holds at the ends, and moves on from a clicked step', async () => {
        const user = userEvent.setup()
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })

        await user.keyboard('{ArrowLeft}')
        expect(current()).toBe(row('0005-calc'))
        await user.keyboard('{ArrowRight}')
        expect(current()).toBe(row('0005-calc'))
        expect(openRows()).toBe(1)

        await user.click(row('0396-route'))
        expect(current()).toBe(row('0396-route'))
        await user.keyboard('{ArrowRight}')
        expect(current()).toBe(row('0100-close'))
    })

    it('leaves the keys alone while typing, and with a modifier held', async () => {
        const user = userEvent.setup()
        render(TraceDetail, { props: { rid: RID, onBack: vi.fn() } })
        const input = document.createElement('input')
        document.body.append(input)
        try {
            input.focus()
            await user.keyboard('{ArrowRight}')
            expect(current()).toBeNull()

            input.blur()
            await user.keyboard('{Alt>}{ArrowRight}{/Alt}')
            await user.keyboard('{Meta>}{ArrowRight}{/Meta}')
            expect(current()).toBeNull()
            expect(openRows()).toBe(0)
        } finally {
            input.remove()
        }
    })

    it('steps through only the model calls when they are shown, fetching their bodies', async () => {
        store.cacheLiveTrace({
            rid: 'CbStepModels',
            cursor: 'CbStepModels',
            steps: [
                { stack: 'loop', scope: 7000, name: 'prep', operation: 'txco://kv/get', started_at: '2026-11-06T00:00:00.100Z' },
                { stack: 'loop', scope: 7101, name: 'work', operation: 'ai://chat', started_at: '2026-11-06T00:00:00.200Z',
                  model: { kind: 'chat', model: 'gpt-5.4', tokens_in: 10, tokens_out: 2 }, body_ref: 'k#0-10' },
                { stack: 'loop', scope: 7102, name: 'save', operation: 'txco://kv/set', started_at: '2026-11-06T00:00:00.300Z' },
                { stack: 'loop', scope: 7103, name: 'judge', operation: 'ai://decide', started_at: '2026-11-06T00:00:00.400Z',
                  model: { kind: 'decide', model: 'jev', tokens_in: 5, tokens_out: 1 } },
            ],
        } as unknown as TraceCachedEvent)
        const body = vi.spyOn(api, 'getTraceBody').mockResolvedValue(null)
        const user = userEvent.setup()
        render(TraceDetail, { props: { rid: 'CbStepModels', onBack: vi.fn() } })

        await user.click(screen.getByRole('button', { name: 'show model calls' }))
        await user.keyboard('{ArrowRight}')
        expect(current()).toBe(row('7101-work'))
        expect(body).toHaveBeenCalledWith(store.state.currentTenant, 'CbStepModels', 'k#0-10')

        await user.keyboard('{ArrowRight}')
        expect(current()).toBe(row('7103-judge'))
    })
})
