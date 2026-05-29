// Subset of admin-ui/src/lib/types.ts — just what the vendored
// StackView needs to render the op-stack diagram.

export interface Op {
    stack: string
    scope: number
    name: string
    txcl: string
}

export function opId(op: Op): string {
    return `${op.stack}/${op.scope}/${op.name}`
}
