import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import SetSecretValueForm from './SetSecretValueForm.svelte'

describe('SetSecretValueForm', () => {
    it('submits the typed value and clears the field on success', async () => {
        const user = userEvent.setup()
        const onSubmit = vi.fn().mockResolvedValue(undefined)
        render(SetSecretValueForm, { props: { onSubmit, submitLabel: 'create' } })

        const input = screen.getByLabelText('value') as HTMLInputElement
        await user.type(input, 'sk_live_sentinel')
        await user.click(screen.getByRole('button', { name: 'create' }))

        expect(onSubmit).toHaveBeenCalledWith('sk_live_sentinel')
        // Load-bearing: nothing lingers in the field after a successful write.
        await waitFor(() => expect(input.value).toBe(''))
    })

    it('retains the value and shows the error when submit throws', async () => {
        const user = userEvent.setup()
        const onSubmit = vi
            .fn()
            .mockRejectedValue(new Error('a secret with that name already exists'))
        render(SetSecretValueForm, { props: { onSubmit, submitLabel: 'create' } })

        const input = screen.getByLabelText('value') as HTMLInputElement
        await user.type(input, 'dup')
        await user.click(screen.getByRole('button', { name: 'create' }))

        await screen.findByText(/already exists/)
        expect(input.value).toBe('dup') // retained for retry
    })

    it('refuses to submit an empty value', async () => {
        const user = userEvent.setup()
        const onSubmit = vi.fn()
        render(SetSecretValueForm, { props: { onSubmit, submitLabel: 'create' } })

        await user.click(screen.getByRole('button', { name: 'create' }))
        expect(onSubmit).not.toHaveBeenCalled()
        await screen.findByText(/cannot be empty/)
    })

    it('blocks submit when disabled', async () => {
        const user = userEvent.setup()
        const onSubmit = vi.fn()
        render(SetSecretValueForm, {
            props: { onSubmit, submitLabel: 'rotate', disabled: true },
        })

        await user.type(screen.getByLabelText('value'), 'x')
        await user.click(screen.getByRole('button', { name: 'rotate' }))
        expect(onSubmit).not.toHaveBeenCalled()
    })

    it('renders a visible (non-masked) textarea with autocomplete/spellcheck off', () => {
        render(SetSecretValueForm, { props: { onSubmit: vi.fn(), submitLabel: 'go' } })
        const field = screen.getByLabelText('value')
        // A textarea, so the operator can verify the pasted value — not a
        // masked password input.
        expect(field.tagName).toBe('TEXTAREA')
        expect(field).not.toHaveAttribute('type', 'password')
        expect(field).toHaveAttribute('spellcheck', 'false')
        expect(field).toHaveAttribute('autocomplete', 'off')
    })
})
