<script lang="ts">
    import type { DiffEntry, DiffResponse, VersionDetail } from '../lib/api'
    import { store } from '../lib/store.svelte'
    import DiffFileView from './DiffFileView.svelte'

    interface Props {
        stack: string
        v1: number // older version (compare-from)
        v2: number // newer version (compare-to)
    }
    let { stack, v1, v2 }: Props = $props()

    let diff = $state<DiffResponse | null>(null)
    let diffLoading = $state(true)
    let diffError = $state('')

    // Per-file expansion state — keyed by path, scoped to this panel
    // instance so collapsing the row resets it.
    let expandedFile = $state<Record<string, boolean>>({})
    let fileLoading = $state<Record<string, boolean>>({})
    let fileError = $state<Record<string, string>>({})

    // Both versions' full content, lazily hydrated when the user
    // expands a file row.
    let fromDetail = $state<VersionDetail | null>(null)
    let toDetail = $state<VersionDetail | null>(null)

    $effect(() => {
        let cancelled = false
        ;(async () => {
            diffLoading = true
            diffError = ''
            try {
                const d = await store.ensureDiff(stack, v1, v2)
                if (!cancelled) diff = d
            } catch (e) {
                if (!cancelled) diffError = e instanceof Error ? e.message : String(e)
            } finally {
                if (!cancelled) diffLoading = false
            }
        })()
        return () => {
            cancelled = true
        }
    })

    function badgeClass(c: DiffEntry['change']): string {
        if (c === 'added') return 'bg-emerald-100 text-emerald-800'
        if (c === 'removed') return 'bg-red-100 text-red-800'
        return 'bg-amber-100 text-amber-900' // changed
    }

    function fileContent(detail: VersionDetail | null, path: string): string {
        if (!detail?.files) return ''
        const f = detail.files.find((x) => x.path === path)
        return f?.content ?? ''
    }

    async function toggleFile(path: string) {
        const open = !expandedFile[path]
        expandedFile[path] = open
        if (!open) return
        // Hydrate both sides on first open. Cached after that.
        if (!fromDetail || !toDetail) {
            fileLoading[path] = true
            fileError[path] = ''
            try {
                const [a, b] = await Promise.all([
                    store.ensureVersionLoaded(stack, v1),
                    store.ensureVersionLoaded(stack, v2),
                ])
                fromDetail = a
                toDetail = b
            } catch (e) {
                fileError[path] = e instanceof Error ? e.message : String(e)
            } finally {
                fileLoading[path] = false
            }
        }
    }
</script>

<div class="text-xs text-neutral-700">
    {#if diffLoading}
        <div class="italic text-neutral-400">loading diff…</div>
    {:else if diffError}
        <div class="rounded border border-red-300 bg-red-50 p-2 text-red-800">{diffError}</div>
    {:else if !diff || diff.equal || !diff.entries || diff.entries.length === 0}
        <div class="italic text-neutral-400">no changes</div>
    {:else}
        <div class="text-[11px] text-neutral-500">
            v{v1} <span class="text-neutral-400">→</span> v{v2}
        </div>
        <ul class="mt-1 space-y-1">
            {#each diff.entries as e (e.path)}
                {@const canExpand = true}
                <li>
                    <button
                        type="button"
                        class="flex w-full items-center gap-2 rounded px-1 py-0.5 text-left hover:bg-white"
                        onclick={() => canExpand && toggleFile(e.path)}
                        title={e.change === 'changed' ? 'click to show line-level diff' : e.change === 'added' ? 'click to show added content' : 'click to show removed content'}
                    >
                        <span class="inline-block w-3 text-neutral-400 transition-transform {expandedFile[e.path] ? 'rotate-90' : ''}">▶</span>
                        <span class="rounded px-1.5 py-0.5 text-[10px] font-medium {badgeClass(e.change)}">{e.change}</span>
                        <span class="font-mono text-[11px] text-neutral-800">{e.path}</span>
                    </button>
                    {#if expandedFile[e.path]}
                        <div class="mt-1 ml-5">
                            {#if fileLoading[e.path]}
                                <div class="italic text-neutral-400">loading file…</div>
                            {:else if fileError[e.path]}
                                <div class="rounded border border-red-300 bg-red-50 p-2 text-red-800">{fileError[e.path]}</div>
                            {:else if e.change === 'added'}
                                <DiffFileView from="" to={fileContent(toDetail, e.path)} />
                            {:else if e.change === 'removed'}
                                <DiffFileView from={fileContent(fromDetail, e.path)} to="" />
                            {:else}
                                <DiffFileView
                                    from={fileContent(fromDetail, e.path)}
                                    to={fileContent(toDetail, e.path)}
                                />
                            {/if}
                        </div>
                    {/if}
                </li>
            {/each}
        </ul>
    {/if}
</div>
