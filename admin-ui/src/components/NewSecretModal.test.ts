import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import NewSecretModal from './NewSecretModal.svelte'
import { store } from '../lib/store.svelte'
import { SecretExistsError } from '../lib/api'

beforeEach(() => {
    vi.restoreAllMocks()
})

describe('NewSecretModal name handling', () => {
    it('does NOT force the name to uppercase', async () => {
        const user = userEvent.setup()
        render(NewSecretModal, { props: { onClose: vi.fn() } })

        const nameInput = screen.getByLabelText('name') as HTMLInputElement
        await user.type(nameInput, 'stripe_key')
        expect(nameInput.value).toBe('stripe_key')
    })

    it('accepts mixed case and enables submit', async () => {
        const user = userEvent.setup()
        render(NewSecretModal, { props: { onClose: vi.fn() } })

        await user.type(screen.getByLabelText('name'), 'Stripe_Key')
        await user.type(screen.getByLabelText('value'), 'sk_live_x')
        expect(screen.getByRole('button', { name: 'create secret' })).not.toBeDisabled()
    })

    it('disables submit and shows an error for a leading digit', async () => {
        const user = userEvent.setup()
        render(NewSecretModal, { props: { onClose: vi.fn() } })

        await user.type(screen.getByLabelText('name'), '1bad')
        expect(screen.getByRole('button', { name: 'create secret' })).toBeDisabled()
        await screen.findByText(/start with a letter/i)
    })

    it('disables submit with an empty name', () => {
        render(NewSecretModal, { props: { onClose: vi.fn() } })
        expect(screen.getByRole('button', { name: 'create secret' })).toBeDisabled()
    })
})

describe('NewSecretModal submit', () => {
    it('calls store.createSecret with the typed values and closes', async () => {
        const user = userEvent.setup()
        const onClose = vi.fn()
        const spy = vi.spyOn(store, 'createSecret').mockResolvedValue(undefined)
        render(NewSecretModal, { props: { onClose } })

        await user.type(screen.getByLabelText('name'), 'Stripe_Key')
        await user.type(screen.getByLabelText('value'), 'sk_live_x')
        await user.click(screen.getByRole('button', { name: 'create secret' }))

        expect(spy).toHaveBeenCalledWith('Stripe_Key', 'sk_live_x', '')
        await waitFor(() => expect(onClose).toHaveBeenCalled())
    })

    it('surfaces a duplicate-name error inline and stays open', async () => {
        const user = userEvent.setup()
        const onClose = vi.fn()
        vi.spyOn(store, 'createSecret').mockRejectedValue(new SecretExistsError('a secret with that name already exists'))
        render(NewSecretModal, { props: { onClose } })

        await user.type(screen.getByLabelText('name'), 'DUP')
        await user.type(screen.getByLabelText('value'), 'v')
        await user.click(screen.getByRole('button', { name: 'create secret' }))

        await screen.findByText(/already exists/i)
        expect(onClose).not.toHaveBeenCalled()
    })
})
