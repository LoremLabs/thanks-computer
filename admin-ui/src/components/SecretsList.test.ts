import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import SecretsList from './SecretsList.svelte'
import { store } from '../lib/store.svelte'
import type { SecretMetadata } from '../lib/api'

const sample: SecretMetadata = {
    secret_id: 'sec_1',
    tenant_id: 'tnt_1',
    name: 'STRIPE_KEY',
    description: 'stripe',
    created_at: '2026-05-01T00:00:00Z',
    key_version: 1,
    version_no: 1,
}

function seed({
    secrets = [] as SecretMetadata[],
    loaded = true,
    unavailable = false,
    caps = ['secret:*:read', 'secret:*:write'] as string[] | null,
}) {
    store.state.secrets = secrets
    store.state.secretsLoaded = loaded
    store.state.secretsUnavailable = unavailable
    store.state.currentTenant = 'acme'
    store.state.session = caps === null ? null : { source: 'browser', capabilities: caps }
}

beforeEach(() => {
    vi.restoreAllMocks()
    // The list refreshes on mount; stub it so tests don't hit the network.
    vi.spyOn(store, 'refreshSecrets').mockResolvedValue(undefined)
    seed({})
})

describe('SecretsList', () => {
    it('shows the empty state with a CLI hint when there are no secrets', () => {
        seed({ secrets: [] })
        render(SecretsList, { props: { onSelectSecret: vi.fn() } })
        expect(screen.getByText(/no secrets yet/i)).toBeInTheDocument()
    })

    it('shows a not-configured state when the store is unavailable, with no "+ new"', () => {
        seed({ unavailable: true })
        render(SecretsList, { props: { onSelectSecret: vi.fn() } })
        expect(screen.getByText(/isn't configured/i)).toBeInTheDocument()
        expect(screen.queryByRole('button', { name: '+ new' })).toBeNull()
    })

    it('gates the "+ new" button on secret:*:write', () => {
        seed({ caps: ['secret:*:read'] })
        const { unmount } = render(SecretsList, { props: { onSelectSecret: vi.fn() } })
        expect(screen.queryByRole('button', { name: '+ new' })).toBeNull()
        unmount()

        seed({ caps: ['secret:*:read', 'secret:*:write'] })
        render(SecretsList, { props: { onSelectSecret: vi.fn() } })
        expect(screen.getByRole('button', { name: '+ new' })).toBeInTheDocument()
    })

    it('renders rows, selects on click, and has no refresh button', async () => {
        const user = userEvent.setup()
        const onSelectSecret = vi.fn()
        seed({ secrets: [sample] })
        render(SecretsList, { props: { onSelectSecret } })

        expect(screen.queryByRole('button', { name: 'refresh' })).toBeNull()
        await user.click(screen.getByText('STRIPE_KEY'))
        expect(onSelectSecret).toHaveBeenCalledWith('STRIPE_KEY')
    })
})
