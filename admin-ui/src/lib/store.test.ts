import { describe, it, expect } from 'vitest'
import { hasCapability } from './store.svelte'
import type { SessionInfo } from './api'

describe('hasCapability', () => {
    it('grants everything in open-dev mode', () => {
        const s: SessionInfo = { source: 'open', open_dev: true }
        expect(hasCapability(s, 'secret:*:write')).toBe(true)
    })

    it('honors the capability list in signed mode', () => {
        const s: SessionInfo = { source: 'browser', capabilities: ['secret:*:read'] }
        expect(hasCapability(s, 'secret:*:read')).toBe(true)
        expect(hasCapability(s, 'secret:*:write')).toBe(false)
    })

    it('treats admin:all as a wildcard that grants everything', () => {
        // An admin's browser session carries admin:all — this is the
        // case a literal includes() got wrong.
        const s: SessionInfo = { source: 'browser', capabilities: ['admin:all'] }
        expect(hasCapability(s, 'secret:*:write')).toBe(true)
        expect(hasCapability(s, 'secret:*:read')).toBe(true)
        expect(hasCapability(s, 'opstack:*:update')).toBe(true)
    })

    it('expands bare * to *:*:* as well', () => {
        const s: SessionInfo = { source: 'browser', capabilities: ['*'] }
        expect(hasCapability(s, 'secret:*:write')).toBe(true)
    })

    it('matches a literal grant but not a sibling action', () => {
        const s: SessionInfo = { source: 'browser', capabilities: ['secret:*:write'] }
        expect(hasCapability(s, 'secret:*:write')).toBe(true)
        expect(hasCapability(s, 'opstack:*:update')).toBe(false)
    })

    it('denies a null session', () => {
        expect(hasCapability(null, 'secret:*:read')).toBe(false)
    })

    it('denies when capabilities is absent and not open-dev', () => {
        const s: SessionInfo = { source: 'browser' }
        expect(hasCapability(s, 'secret:*:read')).toBe(false)
    })
})
