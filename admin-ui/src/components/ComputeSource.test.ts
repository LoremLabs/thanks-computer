// The compute-source view: the entry file first, a tab per imported file,
// and a plain explanation when the stack stores no source for the digest.

import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import ComputeSource from './ComputeSource.svelte'
import { store } from '../lib/store.svelte'

const DIGEST = '3cee8f8dc76d5e1c04156da5ec03fe2dc8f347f2b742386e19e02d55235683d2'

function respond(status: number, body: unknown) {
    const f = vi.fn().mockResolvedValue({
        ok: status >= 200 && status < 300,
        status,
        statusText: '',
        json: async () => body,
    } as Response)
    vi.stubGlobal('fetch', f)
    return f
}

beforeEach(() => {
    store.state.currentTenant = 'acme'
})

afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
})

describe('ComputeSource', () => {
    it('shows the entry first, with a tab per imported file', async () => {
        const f = respond(200, {
            digest: DIGEST,
            stack: '_cron',
            version: 88,
            entry: 'pull_plan.js',
            files: [
                { path: '../lib/time.js', content: 'export const now = () => 0' },
                { path: 'pull_plan.js', content: 'import { now } from "../lib/time.js"' },
            ],
        })
        render(ComputeSource, { props: { stack: '_cron', digest: DIGEST, onBack: vi.fn() } })
        expect(await screen.findByRole('heading', { name: 'pull_plan.js' })).toBeInTheDocument()
        expect(f.mock.calls[0][0]).toBe(`/v1/tenants/acme/stacks/_cron/computes/sha256/${DIGEST}`)
        const tabs = screen.getAllByRole('button').map((b) => b.textContent?.trim())
        expect(tabs.indexOf('pull_plan.js')).toBeLessThan(tabs.indexOf('../lib/time.js'))
        expect(screen.getByText('v88')).toBeInTheDocument()
    })

    it('explains a missing source instead of failing', async () => {
        respond(404, { error: 'no_source' })
        render(ComputeSource, { props: { stack: '_cron', digest: DIGEST, onBack: vi.fn() } })
        expect(await screen.findByText('No source is stored for this compute.')).toBeInTheDocument()
    })
})
