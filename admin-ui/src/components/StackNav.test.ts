import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen } from '@testing-library/svelte'
import userEvent from '@testing-library/user-event'
import StackNav from './StackNav.svelte'
import { store } from '../lib/store.svelte'
import type { Stack } from '../lib/api'

function stack(name: string): Stack {
    return { name, created_at: '2026-06-01T00:00:00Z', active_version: 1 }
}

// 12 "recent" (loaded) stacks + 2 that are only reachable via search.
const RECENT = Array.from({ length: 12 }, (_, i) => stack(`recent/stack-${i}`))
const RARE_A = stack('publications/the-count-of-monte-cristo')
const RARE_B = stack('publications/the-wealth-of-nations')

function seed() {
    store.state.stacks = [...RECENT, RARE_A, RARE_B]
    store.state.visibleStacks = RECENT.map((s) => s.name)
    store.state.ops = []
    store.state.selectedStack = ''
    store.state.selectedId = ''
}

function props(onSelectStack = vi.fn()) {
    return {
        ops: [],
        selectedId: '',
        selectedStack: '',
        onSelectOp: vi.fn(),
        onSelectStack,
    }
}

beforeEach(() => {
    vi.restoreAllMocks()
    seed()
})

describe('StackNav', () => {
    it('shows the "N of M · search to find the rest" hint when stacks are hidden', () => {
        render(StackNav, { props: props() })
        expect(
            screen.getByText(/showing 12 of 14 stacks · search to find the rest/i)
        ).toBeInTheDocument()
    })

    it('search spans ALL stacks, surfacing one not in the loaded set', async () => {
        render(StackNav, { props: props() })
        await userEvent.type(screen.getByLabelText(/search stacks/i), 'wealth')
        // RARE_B is not in visibleStacks, yet search finds it.
        expect(
            screen.getByRole('button', { name: /the-wealth-of-nations/i })
        ).toBeInTheDocument()
        // A non-matching recent stack is not shown.
        expect(screen.queryByText('recent/stack-0')).toBeNull()
    })

    it('clicking a result calls onSelectStack with that stack (which pins + loads it)', async () => {
        const onSelectStack = vi.fn()
        render(StackNav, { props: props(onSelectStack) })
        await userEvent.type(screen.getByLabelText(/search stacks/i), 'monte')
        await userEvent.click(
            screen.getByRole('button', { name: /the-count-of-monte-cristo/i })
        )
        expect(onSelectStack).toHaveBeenCalledWith('publications/the-count-of-monte-cristo')
    })

    it('renders an empty state for a query that matches nothing', async () => {
        render(StackNav, { props: props() })
        await userEvent.type(screen.getByLabelText(/search stacks/i), 'no-such-stack-xyz')
        expect(screen.getByText(/no stack matches/i)).toBeInTheDocument()
    })
})
