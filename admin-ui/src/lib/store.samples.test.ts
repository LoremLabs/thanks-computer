// Coverage for the live-tail-driven op samples. On the NATS/R2 backend
// the trace archive list is empty, so the Sample tab can't be seeded from
// the archive (it would show "no recent request"). cacheLiveTrace folds
// each streamed trace's steps into the per-op sample maps instead — this
// guards that fold + the latest-wins behaviour, and that the cached event
// round-trips the fuel/bytes_out the live-stream wire now carries.

import { describe, it, expect, beforeEach, vi } from 'vitest'
import * as api from './api'
import { store, type TraceCachedEvent } from './store.svelte'

function ev(
    rid: string,
    steps: unknown[],
    extra: Partial<TraceCachedEvent> = {},
): TraceCachedEvent {
    return { rid, cursor: rid, steps, ...extra } as TraceCachedEvent
}

describe('cacheLiveTrace folds op samples', () => {
    beforeEach(() => {
        for (const k of Object.keys(store.state.lastInputs)) delete store.state.lastInputs[k]
        for (const k of Object.keys(store.state.lastOutputs)) delete store.state.lastOutputs[k]
        for (const k of Object.keys(store.state.lastDurations)) delete store.state.lastDurations[k]
    })

    it('populates lastInputs/lastOutputs/lastDurations keyed by stack/scope/name', () => {
        store.cacheLiveTrace(
            ev('r1', [
                { stack: 'site', scope: 100, name: 'resonator', duration_ms: 7, in: { a: 1 }, out: { b: 2 } },
            ]),
        )
        expect(store.state.lastInputs['site/100/resonator']).toEqual({ a: 1 })
        expect(store.state.lastOutputs['site/100/resonator']).toEqual({ b: 2 })
        expect(store.state.lastDurations['site/100/resonator']).toBe(7)
    })

    it('latest streamed trace wins for the same op', () => {
        store.cacheLiveTrace(ev('r1', [{ stack: 'site', scope: 100, name: 'resonator', in: { v: 'old' } }]))
        store.cacheLiveTrace(ev('r2', [{ stack: 'site', scope: 100, name: 'resonator', in: { v: 'new' } }]))
        expect(store.state.lastInputs['site/100/resonator']).toEqual({ v: 'new' })
    })

    it('getCachedTrace round-trips fuel + bytes_out (live-path fuel fix)', () => {
        store.cacheLiveTrace(ev('rid-fuel', [], { fuel: 99, bytes_out: 42 }))
        const c = store.getCachedTrace('rid-fuel')
        expect(c?.fuel).toBe(99)
        expect(c?.bytes_out).toBe(42)
    })
})

// A long trace keeps its bodies out of its document, so its op has no sample
// inline. The Sample tab must not go blank on the ops that matter most.
describe('op samples from a trace whose bodies are not in its document', () => {
    beforeEach(() => {
        vi.restoreAllMocks()
        for (const m of [store.state.lastInputs, store.state.lastOutputs, store.state.lastBodyRefs]) {
            for (const k of Object.keys(m)) delete m[k]
        }
    })

    const spilled = (rid: string) =>
        ev(rid, [{ stack: 'loop', scope: 7101, name: 'work', duration_ms: 7, body_ref: `${rid}#0-9` }])

    it('remembers where the sample is, and fetches it when asked, once', async () => {
        const body = vi.spyOn(api, 'getTraceBody').mockResolvedValue({ in: { a: 1 }, out: { b: 2 } })
        store.cacheLiveTrace(spilled('r1'))
        expect(store.state.lastInputs['loop/7101/work']).toBeUndefined()

        await store.loadOpSample('loop/7101/work')
        expect(body).toHaveBeenCalledTimes(1)
        expect(body.mock.calls[0].slice(1)).toEqual(['r1', 'r1#0-9'])
        expect(store.state.lastInputs['loop/7101/work']).toEqual({ a: 1 })
        expect(store.state.lastOutputs['loop/7101/work']).toEqual({ b: 2 })

        await store.loadOpSample('loop/7101/work')
        expect(body).toHaveBeenCalledTimes(1)
    })

    it('the newest trace wins, whichever kind it is', async () => {
        const body = vi.spyOn(api, 'getTraceBody').mockResolvedValue({ in: { v: 'r2' } })
        store.cacheLiveTrace(spilled('r1'))
        store.cacheLiveTrace(spilled('r2'))
        await store.loadOpSample('loop/7101/work')
        expect(body.mock.calls[0].slice(1)).toEqual(['r2', 'r2#0-9'])

        store.cacheLiveTrace(spilled('r3'))
        store.cacheLiveTrace(ev('r4', [{ stack: 'loop', scope: 7101, name: 'work', in: { v: 'inline' } }]))
        await store.loadOpSample('loop/7101/work')
        expect(body).toHaveBeenCalledTimes(1)
        expect(store.state.lastInputs['loop/7101/work']).toEqual({ v: 'inline' })
    })

    it('a body that cannot be fetched leaves the tab as it was', async () => {
        vi.spyOn(api, 'getTraceBody').mockRejectedValue(new Error('r2 is down'))
        store.cacheLiveTrace(spilled('r1'))
        await store.loadOpSample('loop/7101/work')
        expect(store.state.lastInputs['loop/7101/work']).toBeUndefined()
    })
})
