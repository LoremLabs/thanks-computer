<script lang="ts">
    import { store } from '../lib/store.svelte'
    import Self from './JsonView.svelte'

    interface Props {
        value: unknown
        // Dot-path identifying this node (e.g. "._txc.web"). The
        // top-level call uses "" — children append their key/index.
        path?: string
    }

    let { value, path = '' }: Props = $props()

    type Kind = 'object' | 'array' | 'string' | 'number' | 'boolean' | 'null'

    function kindOf(v: unknown): Kind {
        if (v === null) return 'null'
        if (Array.isArray(v)) return 'array'
        const t = typeof v
        if (t === 'string' || t === 'number' || t === 'boolean') return t
        return 'object'
    }

    const kind = $derived(kindOf(value))
    const collapsed = $derived(store.state.jsonCollapsed[path] === true)

    function entriesOf(v: unknown, k: Kind): Array<[string, unknown]> {
        if (k === 'array') return (v as unknown[]).map((x, i) => [String(i), x])
        if (k === 'object') return Object.entries(v as Record<string, unknown>)
        return []
    }

    // JSON-escape a string and strip the surrounding quotes —
    // produces the textual content of a JSON string literal which
    // we then wrap with literal `"`. Handles escapes correctly.
    function escapeStr(s: string): string {
        return JSON.stringify(s).slice(1, -1)
    }
</script>

{#if kind === 'string'}<span class="text-emerald-700">"{escapeStr(value as string)}"</span>{:else if kind === 'number'}<span class="text-amber-600">{value}</span>{:else if kind === 'boolean'}<span class="text-purple-700">{String(value)}</span>{:else if kind === 'null'}<span class="text-purple-700">null</span>{:else}
    {@const isArr = kind === 'array'}
    {@const open = isArr ? '[' : '{'}
    {@const close = isArr ? ']' : '}'}
    {@const items = entriesOf(value, kind)}
    {#if items.length === 0}<span class="text-neutral-500">{open}{close}</span>{:else if collapsed}<button type="button" class="mr-0.5 text-neutral-400 hover:text-neutral-900" onclick={() => store.toggleJsonCollapse(path)}>▶</button><span class="text-neutral-500">{open}</span><span class="text-neutral-400">…</span><span class="text-neutral-500">{close}</span>{:else}<button type="button" class="mr-0.5 text-neutral-400 hover:text-neutral-900" onclick={() => store.toggleJsonCollapse(path)}>▼</button><span class="text-neutral-500">{open}</span>
        <div class="pl-4">
            {#each items as [key, child], i (key)}
                <div>{#if !isArr}<span class="text-sky-700">"{escapeStr(key)}"</span><span class="text-neutral-500">: </span>{/if}<Self
                        value={child}
                        path={path + '.' + key}
                    />{#if i < items.length - 1}<span class="text-neutral-500">,</span>{/if}</div>
            {/each}
        </div>
        <span class="text-neutral-500">{close}</span>
    {/if}
{/if}
