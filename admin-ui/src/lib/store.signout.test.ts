// Coverage for signOut()'s verification step.
//
// deleteSession() deliberately treats 401 as success, because an
// already-expired cookie 401s and that genuinely is signed out. But a
// CSRF-origin rejection also 401s — the middleware rejects the DELETE
// before the handler runs, leaving the session completely live. The two
// are indistinguishable from the response alone, so signOut() re-probes
// GET /auth/browser/session and believes the chassis rather than the
// DELETE's status code.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { store } from './store.svelte'

type Call = { url: string; method: string }

// mockAuthFetch answers the two endpoints signOut() touches: the DELETE
// and the follow-up session probe. `probe` is the body the probe returns,
// or null for a 401 (no session).
function mockAuthFetch(deleteStatus: number, probe: unknown | null) {
    const calls: Call[] = []
    const f = vi.fn(async (url: string, init?: RequestInit) => {
        const method = init?.method ?? 'GET'
        calls.push({ url, method })
        if (method === 'DELETE') {
            return {
                ok: deleteStatus >= 200 && deleteStatus < 300,
                status: deleteStatus,
                statusText: '',
                json: async () => ({ deleted: true }),
            } as Response
        }
        // GET /auth/browser/session
        if (probe === null) {
            return { ok: false, status: 401, statusText: '', json: async () => ({}) } as Response
        }
        return { ok: true, status: 200, statusText: '', json: async () => probe } as Response
    })
    vi.stubGlobal('fetch', f)
    return calls
}

beforeEach(() => {
    window.location.hash = ''
    store.state.session = { source: 'browser', actor_id: 'actor_test' }
    store.state.sessionLoaded = true
    store.state.signOutError = null
})

afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
})

describe('signOut', () => {
    it('clears the session and routes to #login when the probe says 401', async () => {
        const calls = mockAuthFetch(200, null)

        await store.signOut()

        expect(calls.map((c) => c.method)).toEqual(['DELETE', 'GET'])
        expect(store.state.session).toBeNull()
        expect(store.state.sessionLoaded).toBe(true)
        expect(store.state.signOutError).toBeNull()
        expect(window.location.hash).toBe('#login')
    })

    // The regression this whole change exists for: the server 401s the
    // DELETE (CSRF origin mismatch) but the session is still live. The old
    // code reported a clean sign-out and bounced to #login anyway.
    it('stays signed in and reports an error when the session survives', async () => {
        mockAuthFetch(401, { source: 'browser', actor_id: 'actor_test' })

        await store.signOut()

        expect(store.state.session).not.toBeNull()
        expect(store.state.session?.source).toBe('browser')
        expect(store.state.signOutError).toMatch(/sign out failed/i)
        expect(window.location.hash).not.toBe('#login')
    })

    // A 200 DELETE whose session somehow survives is the same failure —
    // trust the probe, not the status code.
    it('trusts the probe over a 200 DELETE', async () => {
        mockAuthFetch(200, { source: 'browser', actor_id: 'actor_test' })

        await store.signOut()

        expect(store.state.signOutError).toMatch(/sign out failed/i)
        expect(store.state.session?.source).toBe('browser')
    })

    // Only a surviving `browser` session means the DELETE failed. A chassis
    // answering as open-dev has genuinely dropped the browser session; we're
    // just seeing its other auth path.
    it('treats a non-browser probe result as signed out', async () => {
        mockAuthFetch(200, { source: 'open', open_dev: true })

        await store.signOut()

        expect(store.state.session).toBeNull()
        expect(store.state.signOutError).toBeNull()
        expect(window.location.hash).toBe('#login')
    })

    it('signs out when the probe itself fails rather than trapping the user', async () => {
        const f = vi.fn(async (_url: string, init?: RequestInit) => {
            if ((init?.method ?? 'GET') === 'DELETE') {
                return { ok: true, status: 200, statusText: '', json: async () => ({}) } as Response
            }
            throw new Error('network down')
        })
        vi.stubGlobal('fetch', f)

        await store.signOut()

        expect(store.state.session).toBeNull()
        expect(window.location.hash).toBe('#login')
    })

    it('still probes when the DELETE throws outright', async () => {
        const calls: string[] = []
        const f = vi.fn(async (_url: string, init?: RequestInit) => {
            const method = init?.method ?? 'GET'
            calls.push(method)
            if (method === 'DELETE') throw new Error('network down')
            return { ok: false, status: 401, statusText: '', json: async () => ({}) } as Response
        })
        vi.stubGlobal('fetch', f)

        await store.signOut()

        // The revoke may have landed before the response failed, so the
        // probe is what decides.
        expect(calls).toEqual(['DELETE', 'GET'])
        expect(store.state.session).toBeNull()
    })
})
