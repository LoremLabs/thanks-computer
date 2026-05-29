<script lang="ts">
    import { ValidationFailedError, type Version } from '../lib/api'
    import Ago from './Ago.svelte'
    import DiffPanel from './DiffPanel.svelte'

    interface Props {
        stack: string
        versions: Version[]
        activeVersion?: number
        onSelectVersion: (n: number) => void
        onActivate: (n: number) => Promise<void>
        onCreateDraft: () => Promise<void>
    }

    let { stack, versions, activeVersion, onSelectVersion, onActivate, onCreateDraft }: Props = $props()

    let creatingDraft = $state(false)
    let createDraftError = $state('')
    async function doCreateDraft() {
        if (creatingDraft) return
        creatingDraft = true
        createDraftError = ''
        try {
            await onCreateDraft()
        } catch (e) {
            createDraftError = e instanceof Error ? e.message : String(e)
        } finally {
            creatingDraft = false
        }
    }

    const ordered = $derived(
        [...versions].sort((a, b) => b.version_number - a.version_number)
    )

    // Per-row UI state: which row is in-flight, the last error to
    // display inline (sticky until user dismisses). Keyed by version
    // number so concurrent clicks don't cross-contaminate.
    let busy = $state<Record<number, boolean>>({})
    let errors = $state<Record<number, string>>({})

    // Diff expansion: which rows have their diff panel open. The
    // earliest version has no prior, so its row never gets a chevron.
    let expandedRow = $state<Record<number, boolean>>({})

    function statusLabel(v: Version): string {
        if (v.is_active || (activeVersion && v.version_number === activeVersion)) {
            return 'active'
        }
        return v.status
    }

    function canActivate(v: Version): boolean {
        if (statusLabel(v) === 'active') return false
        if (v.status === 'revoked') return false
        return true
    }

    // Tooltip explaining why activate is disabled, when it is.
    function activateDisabledReason(v: Version): string {
        if (statusLabel(v) === 'active') return 'already the active version'
        if (v.status === 'revoked') return 'revoked versions cannot be activated'
        return ''
    }

    async function doActivate(n: number) {
        if (!confirm(`Activate ${stack} v${n}?`)) return
        busy[n] = true
        delete errors[n]
        try {
            await onActivate(n)
        } catch (e) {
            if (e instanceof ValidationFailedError) {
                const lines = e.errors.map((er) => `  • ${er.path}: ${er.err}`).join('\n')
                errors[n] = `validation failed (${e.errors.length} error${e.errors.length === 1 ? '' : 's'}):\n${lines}`
            } else {
                errors[n] = e instanceof Error ? e.message : String(e)
            }
        } finally {
            busy[n] = false
        }
    }
</script>

<div class="flex h-full flex-col p-4">
    <header class="mb-3 flex items-start justify-between gap-3">
        <div>
            <h2 class="font-mono text-base font-semibold text-neutral-900">{stack} <span class="text-neutral-400">·</span> history</h2>
            <p class="text-xs text-neutral-500">activate any prior version, or activate a draft (which also clones it into a fresh draft so editing can continue).</p>
            {#if createDraftError}
                <p class="mt-1 text-xs text-red-600">draft failed: {createDraftError}</p>
            {/if}
        </div>
        <button
            type="button"
            class="shrink-0 rounded border border-brand-cyan/40 bg-brand-cyan/10 px-2 py-0.5 text-xs text-neutral-900 hover:bg-brand-cyan/20 disabled:opacity-50"
            title="clone the active version into a new draft you can edit"
            disabled={creatingDraft}
            onclick={doCreateDraft}
        >
            {creatingDraft ? 'cloning…' : 'Clone'}
        </button>
    </header>

    {#if ordered.length === 0}
        <p class="rounded border border-neutral-200 bg-white p-4 text-sm italic text-neutral-400">
            no versions
        </p>
    {:else}
        <div class="overflow-auto rounded border border-neutral-200 bg-white">
            <table class="w-full text-sm">
                <thead class="border-b border-neutral-200 bg-neutral-50 text-left text-xs uppercase tracking-wide text-neutral-500">
                    <tr>
                        <th class="px-3 py-2 font-medium">version</th>
                        <th class="px-3 py-2 font-medium">status</th>
                        <th class="px-3 py-2 font-medium">activated</th>
                        <th class="px-3 py-2"></th>
                        <th class="px-3 py-2"></th>
                    </tr>
                </thead>
                <tbody class="divide-y divide-neutral-100">
                    {#each ordered as v, i (v.version_number)}
                        {@const label = statusLabel(v)}
                        {@const disabledActivate = !canActivate(v) || !!busy[v.version_number]}
                        {@const prior = ordered[i + 1]}
                        {@const expanded = !!expandedRow[v.version_number]}
                        <tr class="hover:bg-neutral-50">
                            <td class="px-3 py-2 align-top font-mono">
                                {#if prior}
                                    <button
                                        type="button"
                                        class="mr-1 inline-block w-3 cursor-pointer text-neutral-400 transition-transform hover:text-neutral-700 {expanded ? 'rotate-90' : ''}"
                                        title="show diff vs v{prior.version_number}"
                                        onclick={() => (expandedRow[v.version_number] = !expanded)}
                                    >▶</button>
                                {:else}
                                    <span class="mr-1 inline-block w-3"></span>
                                {/if}v{v.version_number}
                            </td>
                            <td class="px-3 py-2 align-top">
                                {#if label !== 'superseded'}
                                    <span
                                        class="rounded px-1.5 py-0.5 text-[11px] font-medium
                                            {label === 'active' ? 'bg-brand-cyan/10 text-neutral-900' : ''}
                                            {label === 'draft' ? 'bg-amber-100 text-amber-900' : ''}
                                            {label === 'revoked' ? 'bg-red-100 text-red-800' : ''}"
                                    >
                                        {label}
                                    </span>
                                {/if}
                            </td>
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-400">
                                <Ago at={v.activated_at} />
                            </td>
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-600">{v.created_by}</td>
                            <td class="px-3 py-2 align-top text-right">
                                <div class="inline-flex gap-1">
                                    <button
                                        type="button"
                                        class="rounded border border-neutral-300 bg-white px-2 py-0.5 text-xs text-neutral-700 hover:bg-neutral-50"
                                        onclick={() => onSelectVersion(v.version_number)}
                                    >
                                        view
                                    </button>
                                    <button
                                        type="button"
                                        disabled={disabledActivate}
                                        title={activateDisabledReason(v) || `activate ${stack} v${v.version_number}`}
                                        class="rounded border border-brand-red/50 bg-brand-red/10 px-2 py-0.5 text-xs text-neutral-900 hover:bg-brand-red/20 disabled:cursor-not-allowed disabled:opacity-40 disabled:hover:bg-brand-red/10"
                                        onclick={() => doActivate(v.version_number)}
                                    >
                                        {busy[v.version_number] ? 'activating…' : 'activate'}
                                    </button>
                                </div>
                                {#if errors[v.version_number]}
                                    <pre class="mt-1 whitespace-pre-wrap rounded bg-red-50 p-1.5 text-left text-[11px] text-red-800">{errors[v.version_number]}</pre>
                                {/if}
                            </td>
                        </tr>
                        {#if expanded && prior}
                            <tr class="bg-neutral-50">
                                <td colspan="5" class="px-3 py-2">
                                    <DiffPanel
                                        {stack}
                                        v1={prior.version_number}
                                        v2={v.version_number}
                                    />
                                </td>
                            </tr>
                        {/if}
                    {/each}
                </tbody>
            </table>
        </div>
    {/if}
</div>
