// Coverage for the `#demo` hash route + the demoMode probe. The demo
// SPA was merged into admin-ui — this test guards the route plumbing so
// future refactors don't silently drop the `txco demo` entry point.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { store } from './store.svelte'

const ORIGINAL_FETCH = global.fetch

beforeEach(() => {
    // Each test starts from a clean hash + state so the assertion
    // doesn't pick up leftovers from a sibling test.
    window.location.hash = ''
    store.state.showDemo = false
    store.state.demoMode = false
})

afterEach(() => {
    global.fetch = ORIGINAL_FETCH
})

describe('demo hash route', () => {
    it('syncFromHash maps #demo → state.showDemo', () => {
        window.location.hash = 'demo'
        store.syncFromHash()
        expect(store.state.showDemo).toBe(true)
    })

    it('switching away from #demo clears showDemo', () => {
        window.location.hash = 'demo'
        store.syncFromHash()
        expect(store.state.showDemo).toBe(true)

        window.location.hash = 'traces'
        store.syncFromHash()
        expect(store.state.showDemo).toBe(false)
        expect(store.state.showTraces).toBe('__list__')
    })
})

describe('probeDemoMode', () => {
    it('sets demoMode=true on a successful /v1/demo/info response', async () => {
        global.fetch = vi.fn().mockResolvedValue(
            new Response(JSON.stringify({ web_addr: '127.0.0.1:8080', web_port: '8080' }), {
                status: 200,
                headers: { 'content-type': 'application/json' },
            })
        ) as unknown as typeof global.fetch
        await store.probeDemoMode()
        expect(store.state.demoMode).toBe(true)
    })

    it('leaves demoMode=false on 404 (chassis not in demo mode)', async () => {
        global.fetch = vi.fn().mockResolvedValue(
            new Response('', { status: 404 })
        ) as unknown as typeof global.fetch
        await store.probeDemoMode()
        expect(store.state.demoMode).toBe(false)
    })

    it('leaves demoMode=false on network failure', async () => {
        global.fetch = vi.fn().mockRejectedValue(new Error('network down')) as unknown as typeof global.fetch
        await store.probeDemoMode()
        expect(store.state.demoMode).toBe(false)
    })
})
