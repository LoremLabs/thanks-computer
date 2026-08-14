// Coverage for browser-history semantics: user-initiated navigation
// pushes a history entry (so Back/Forward walk the in-app views),
// while corrective writes replace in place. The reader half — Back
// firing hashchange → syncFromHash — is simulated the way
// store.demo.test.ts does: set window.location.hash directly, then
// call syncFromHash (store tests have no App-level listener).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { store } from './store.svelte'

const ORIGINAL_FETCH = global.fetch

beforeEach(() => {
    // Pre-pin the stacks these tests navigate between so pinStack
    // no-ops (no fetches) — and mock fetch defensively anyway.
    global.fetch = vi.fn().mockResolvedValue(
        new Response('{}', { status: 200, headers: { 'content-type': 'application/json' } })
    ) as unknown as typeof global.fetch
    store.state.visibleStacks = ['_inspect', 'foo']
    window.location.hash = ''
    store.state.selectedId = ''
    store.state.selectedStack = ''
    store.state.showVersionsList = ''
    store.state.showTraces = ''
    store.state.showSecrets = ''
    store.state.showInspect = false
    store.state.showDemo = false
})

afterEach(() => {
    global.fetch = ORIGINAL_FETCH
})

const op = { stack: '_inspect', scope: 200, name: 'card', txcl: '' }

describe('navigation pushes history entries', () => {
    it('selectStack then selectOp adds one entry each', () => {
        const before = history.length
        store.selectStack('_inspect')
        expect(window.location.hash).toBe('#stack/_inspect')
        store.selectOp(op)
        expect(window.location.hash).toBe('#ops/_inspect/200/card')
        expect(history.length).toBe(before + 2)
    })

    it('re-selecting the current stack adds no entry', () => {
        store.selectStack('foo')
        const before = history.length
        store.selectStack('foo')
        expect(history.length).toBe(before)
        expect(window.location.hash).toBe('#stack/foo')
    })

    it('traces / secrets / inspect / versions push too', () => {
        const before = history.length
        store.showTraces()
        expect(window.location.hash).toBe('#traces')
        store.showSecrets()
        expect(window.location.hash).toBe('#secrets')
        store.showInspect()
        expect(window.location.hash).toBe('#inspect')
        store.showVersions('foo')
        expect(window.location.hash).toBe('#stack/foo/versions')
        expect(history.length).toBe(before + 4)
    })
})

describe('corrective writes replace instead of pushing', () => {
    it('selectStack with history:replace updates the hash without a new entry', () => {
        store.selectStack('foo')
        const before = history.length
        store.selectStack('_inspect', { history: 'replace' })
        expect(window.location.hash).toBe('#stack/_inspect')
        expect(history.length).toBe(before)
    })
})

describe('Back restores the previous view (reader side)', () => {
    it('op detail → Back → stack view (the reported scenario)', () => {
        store.selectStack('_inspect')
        store.selectOp(op)
        expect(store.state.selectedId).toBe('_inspect/200/card')

        // Simulate the browser Back button: the URL reverts to the
        // pushed previous entry and hashchange fires syncFromHash.
        window.location.hash = 'stack/_inspect'
        store.syncFromHash()

        expect(store.state.selectedStack).toBe('_inspect')
        expect(store.state.selectedId).toBe('')
        expect(store.state.showTraces).toBe('')
        expect(store.state.showSecrets).toBe('')
        expect(store.state.showInspect).toBe(false)
        expect(store.state.showVersionsList).toBe('')
    })

    it('traces → Back → stack view', () => {
        store.selectStack('foo')
        store.showTraces()
        expect(store.state.showTraces).toBe('__list__')

        window.location.hash = 'stack/foo'
        store.syncFromHash()

        expect(store.state.selectedStack).toBe('foo')
        expect(store.state.showTraces).toBe('')
    })
})
