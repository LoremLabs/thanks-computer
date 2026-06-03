// Coverage for the live-tail-driven op samples. On the NATS/R2 backend
// the trace archive list is empty, so the Sample tab can't be seeded from
// the archive (it would show "no recent request"). cacheLiveTrace folds
// each streamed trace's steps into the per-op sample maps instead — this
// guards that fold + the latest-wins behaviour, and that the cached event
// round-trips the fuel/bytes_out the live-stream wire now carries.

import { describe, it, expect, beforeEach } from 'vitest'
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
