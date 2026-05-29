import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import SecretDetail from './SecretDetail.svelte'
import { store } from '../lib/store.svelte'
import type { SecretMetadata } from '../lib/api'

const sampleSecret: SecretMetadata = {
    secret_id: 'sec_1',
    tenant_id: 'tnt_1',
    name: 'STRIPE_KEY',
    description: 'stripe',
    created_at: '2026-05-01T00:00:00Z',
    key_version: 1,
    version_no: 1,
}

// Seed the singleton store so the component renders against known
// state without hitting the network (secretsLoaded=true short-circuits
// the warm-on-mount effect).
function seedStore(caps: string[] | null) {
    store.state.secrets = [sampleSecret]
    store.state.secretsLoaded = true
    store.state.secretsUnavailable = false
    store.state.currentTenant = 'acme'
    store.state.session = caps === null ? null : { source: 'browser', capabilities: caps }
}

beforeEach(() => {
    seedStore(['secret:*:read', 'secret:*:write'])
    vi.restoreAllMocks()
})

describe('SecretDetail capability gating', () => {
    it('hides write actions for a read-only actor', () => {
        seedStore(['secret:*:read'])
        render(SecretDetail, { props: { name: 'STRIPE_KEY', onBack: vi.fn() } })

        expect(screen.queryByRole('button', { name: /rotate value/i })).toBeNull()
        expect(screen.queryByRole('button', { name: /revoke secret/i })).toBeNull()
        expect(screen.getByText(/can read secrets but not modify/i)).toBeInTheDocument()
    })

    it('shows write actions for a writer', () => {
        seedStore(['secret:*:read', 'secret:*:write'])
        render(SecretDetail, { props: { name: 'STRIPE_KEY', onBack: vi.fn() } })

        expect(screen.getByRole('button', { name: /rotate value/i })).toBeInTheDocument()
        expect(screen.getByRole('button', { name: /revoke secret/i })).toBeInTheDocument()
    })
})

describe('SecretDetail revoke confirm gate', () => {
    it('disables revoke until the exact name is typed, then calls the store', async () => {
        const user = userEvent.setup()
        const onBack = vi.fn()
        const revokeSpy = vi.spyOn(store, 'revokeSecret').mockResolvedValue(undefined)
        render(SecretDetail, { props: { name: 'STRIPE_KEY', onBack } })

        await user.click(screen.getByRole('button', { name: /revoke secret/i }))

        const confirmInput = screen.getByLabelText(/to confirm/i)
        const revokeBtn = screen.getByRole('button', { name: 'revoke' })
        expect(revokeBtn).toBeDisabled()

        await user.type(confirmInput, 'STRIPE_KEY')
        expect(revokeBtn).not.toBeDisabled()

        await user.click(revokeBtn)
        expect(revokeSpy).toHaveBeenCalledWith('STRIPE_KEY')
        await waitFor(() => expect(onBack).toHaveBeenCalled())
    })
})

describe('SecretDetail rotate confirm gate', () => {
    it('disables rotate until the name is typed, then calls store.rotateSecret', async () => {
        const user = userEvent.setup()
        const rotateSpy = vi.spyOn(store, 'rotateSecret').mockResolvedValue(undefined)
        render(SecretDetail, { props: { name: 'STRIPE_KEY', onBack: vi.fn() } })

        // Open the rotate panel (trigger reads "rotate value…").
        await user.click(screen.getByRole('button', { name: /rotate value/i }))

        const confirmInput = screen.getByLabelText(/to confirm/i) // type-name field
        const submitBtn = screen.getByRole('button', { name: 'rotate value' })
        expect(submitBtn).toBeDisabled()

        await user.type(confirmInput, 'STRIPE_KEY')
        expect(submitBtn).not.toBeDisabled()

        await user.type(screen.getByLabelText('new value'), 'newsecret')
        await user.click(submitBtn)
        expect(rotateSpy).toHaveBeenCalledWith('STRIPE_KEY', 'newsecret')
    })
})
