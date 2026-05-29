import type { Op } from './types'

export interface ScopeGroup {
    scope: number
    ops: Op[]
}

export interface StackGroup {
    stack: string
    scopes: ScopeGroup[]
    total: number
}

// Group a flat op list into stack → scope → ops, sorted naturally:
// stacks alphabetically (with "boot" surfaced first since it's the
// well-known entry), scopes by number, ops alphabetically by name.
export function groupOps(ops: Op[]): StackGroup[] {
    const byStack = new Map<string, Map<number, Op[]>>()
    for (const op of ops) {
        let scopes = byStack.get(op.stack)
        if (!scopes) {
            scopes = new Map()
            byStack.set(op.stack, scopes)
        }
        const list = scopes.get(op.scope) ?? []
        list.push(op)
        scopes.set(op.scope, list)
    }

    const stacks: StackGroup[] = []
    for (const [stack, scopes] of byStack) {
        const groups: ScopeGroup[] = []
        for (const [scope, scopeOps] of scopes) {
            scopeOps.sort((a, b) => a.name.localeCompare(b.name))
            groups.push({ scope, ops: scopeOps })
        }
        groups.sort((a, b) => a.scope - b.scope)
        stacks.push({ stack, scopes: groups, total: scopes.size })
    }

    stacks.sort((a, b) => {
        if (a.stack === 'boot') return -1
        if (b.stack === 'boot') return 1
        return a.stack.localeCompare(b.stack)
    })
    return stacks
}
