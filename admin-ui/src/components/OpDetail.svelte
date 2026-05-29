<script lang="ts">
    import { store } from '../lib/store.svelte'
    import { mockReqPath, mockResPath, fileForOp } from '../lib/version_adapter'
    import type { Op } from '../lib/types'
    import type { ValidateError } from '../lib/api'
    import JsonEditor from './JsonEditor.svelte'
    import JsonPre from './JsonPre.svelte'
    import Tabs from './Tabs.svelte'
    import TxclEditor from './TxclEditor.svelte'
    import TxclHelp from './TxclHelp.svelte'

    interface Props {
        op: Op | null
        // Most-recent observed step.in / step.out for this op,
        // populated by store.refreshLastDurations(). `undefined` =
        // never observed (or the chassis redacted it); the tab shows
        // an empty-state line.
        lastInput?: unknown
        lastOutput?: unknown
        // Which version of `op.stack` is currently focused. Drives
        // the editor's readonly gate (only drafts are editable) and
        // the PATCH path's `versionNumber`.
        version?: number
        // Whether the focused version is a draft. Editing is gated
        // here so active/superseded versions stay read-only.
        isDraft?: boolean
    }

    let { op, lastInput, lastOutput, version, isDraft = false }: Props = $props()

    const TABS = ['Resonator', 'Sample', 'Mock']
    // Active tab persists across op changes — switching from `hello`
    // to `world` while on the Sample tab keeps you on Sample. The
    // component instance survives prop changes, so plain $state is
    // enough; no need to lift this to the store.
    let active = $state(TABS[0])

    // Last validation result for the focused version. Drives the
    // per-file inline error panels under each editor.
    const validation = $derived(
        op && typeof version === 'number'
            ? store.state.lastValidation[`${op.stack}:${version}`]
            : undefined
    )
    function errorsFor(path: string): ValidateError[] {
        return validation?.errors?.filter((e) => e.path === path) ?? []
    }

    // Derived paths + hashes for the three editable files. Kept in
    // $derived so they recompute when the op (or its version slice)
    // changes.
    const txclPath = $derived(op ? fileForOp(op.scope, op.name) : '')
    const mockReqFile = $derived(op ? mockReqPath(op.scope) : '')
    const mockResFile = $derived(op ? mockResPath(op.scope) : '')

    async function saveTxcl(content: string) {
        if (!op || typeof version !== 'number') return
        await store.patchDraftFile(op.stack, version, txclPath, content, op.etag ?? '')
    }

    // Delete this op from the editable draft. Auto-draft is handled in
    // store.deleteOp (mirrors createOp); when viewing a non-draft the
    // confirm calls that out. Errors surface via the global error banner.
    let deleting = $state(false)
    async function onDelete() {
        if (!op || typeof version !== 'number') return
        const note = isDraft
            ? ''
            : '\n\nThis will create a new draft from the active version.'
        if (!confirm(`Delete operation ${op.scope}/${op.name} from "${op.stack}"?${note}`)) {
            return
        }
        deleting = true
        try {
            await store.deleteOp(op.stack, version, op.scope, op.name)
        } finally {
            deleting = false
        }
    }

    // Mock saves go through patchMockFile, which auto-clones a draft
    // from active when the focused version isn't already a draft. The
    // store re-derives the right base_hash from the target draft's
    // files, so the editor doesn't need to know which version it's
    // writing to.
    async function saveMockReq(_parsed: unknown, raw: string) {
        if (!op || typeof version !== 'number') return
        await store.patchMockFile(op.stack, version, mockReqFile, raw)
    }

    async function saveMockRes(_parsed: unknown, raw: string) {
        if (!op || typeof version !== 'number') return
        await store.patchMockFile(op.stack, version, mockResFile, raw)
    }

    async function reload() {
        if (!op) return
        await store.refreshVersions(op.stack)
        if (typeof version === 'number') {
            // Drop the cached slice so the next rebuild refetches.
            delete store.state.versionCache[`${op.stack}:${version}`]
            await store.setStackVersion(op.stack, version)
        }
    }
</script>

{#if !op}
    <div class="flex h-full items-center justify-center text-sm text-neutral-400">
        &nbsp;
    </div>
{:else}
    <div class="flex h-full flex-col gap-3">
        <header class="flex items-baseline justify-between gap-2">
            <h2 class="font-mono text-base font-semibold text-neutral-900">
                {op.stack}<span class="text-neutral-400">/</span>{op.scope}<span class="text-neutral-400">/</span>{op.name}
            </h2>
            <button
                type="button"
                class="shrink-0 rounded border border-red-300 bg-red-50 px-2 py-0.5 text-xs text-red-700 hover:bg-red-100 disabled:opacity-40"
                disabled={deleting}
                onclick={onDelete}
            >
                {deleting ? 'Deleting…' : 'Delete op'}
            </button>
        </header>
        <Tabs tabs={TABS} {active} onSelect={(t) => (active = t)} />
        <div class="flex-1 overflow-auto">
            {#if active === 'Resonator'}
                <div class="flex h-full flex-col">
                    <table class="w-auto text-sm">
                        <tbody class="divide-y divide-neutral-200">
                            <tr><td class="py-1 pr-4 text-neutral-500">stack</td><td class="font-mono">{op.stack}</td></tr>
                            <tr><td class="py-1 pr-4 text-neutral-500">scope</td><td class="font-mono">{op.scope}</td></tr>
                            <tr><td class="py-1 pr-4 text-neutral-500">name</td><td class="font-mono">{op.name}</td></tr>
                            {#if op.revision != null}
                                <tr><td class="py-1 pr-4 text-neutral-500">revision</td><td class="font-mono">{op.revision}</td></tr>
                            {/if}
                            {#if op.etag}
                                <tr><td class="py-1 pr-4 text-neutral-500">etag</td><td class="font-mono">{op.etag}</td></tr>
                            {/if}
                        </tbody>
                    </table>
                    <div class="mt-6">
                        {#if op.txcl && op.txcl.length > 0}
                            <TxclEditor
                                value={op.txcl}
                                readonly={!isDraft}
                                errors={errorsFor(txclPath)}
                                onSave={saveTxcl}
                                onReload={reload}
                            />
                        {:else if isDraft}
                            <TxclEditor
                                value=""
                                readonly={false}
                                errors={errorsFor(txclPath)}
                                onSave={saveTxcl}
                                onReload={reload}
                            />
                        {:else}
                            <p class="text-sm italic text-neutral-400">no resonator recorded</p>
                        {/if}
                    </div>
                    <div class="mt-auto pt-4">
                        <TxclHelp />
                    </div>
                </div>
            {:else if active === 'Sample'}
                <div class="grid h-full grid-cols-1 gap-3 lg:grid-cols-2">
                    <section class="flex min-w-0 flex-col">
                        <div class="mb-1 px-1 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">request</div>
                        <div class="min-h-0 min-w-0 flex-1">
                            <JsonPre value={lastInput} emptyLabel="no recent request" />
                        </div>
                    </section>
                    <section class="flex min-w-0 flex-col">
                        <div class="mb-1 px-1 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">response</div>
                        <div class="min-h-0 min-w-0 flex-1">
                            <JsonPre value={lastOutput} emptyLabel="no recent response" />
                        </div>
                    </section>
                </div>
            {:else if active === 'Mock'}
                <div class="grid h-full grid-cols-1 gap-3 lg:grid-cols-2">
                    <section class="flex min-w-0 flex-col">
                        <div class="mb-1 px-1 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">request</div>
                        <div class="min-h-0 min-w-0 flex-1">
                            <JsonEditor
                                value={op.mock_req}
                                readonly={false}
                                autoDraftHint={isDraft ? '' : 'creates a new draft'}
                                emptyLabel="no mock request"
                                onSave={saveMockReq}
                                onReload={reload}
                            />
                            {#if errorsFor(mockReqFile).length > 0}
                                <div class="mt-2 rounded border border-red-300 bg-red-50 p-2 text-xs text-red-800">
                                    <div class="mb-1 font-semibold">
                                        validation: {errorsFor(mockReqFile).length} error{errorsFor(mockReqFile).length === 1 ? '' : 's'}
                                    </div>
                                    <ul class="space-y-0.5">
                                        {#each errorsFor(mockReqFile) as e}
                                            <li><span class="font-mono">{e.path}</span>: {e.err}</li>
                                        {/each}
                                    </ul>
                                </div>
                            {/if}
                        </div>
                    </section>
                    <section class="flex min-w-0 flex-col">
                        <div class="mb-1 px-1 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">response</div>
                        <div class="min-h-0 min-w-0 flex-1">
                            <JsonEditor
                                value={op.mock_res}
                                readonly={false}
                                autoDraftHint={isDraft ? '' : 'creates a new draft'}
                                emptyLabel="no mock response"
                                onSave={saveMockRes}
                                onReload={reload}
                            />
                            {#if errorsFor(mockResFile).length > 0}
                                <div class="mt-2 rounded border border-red-300 bg-red-50 p-2 text-xs text-red-800">
                                    <div class="mb-1 font-semibold">
                                        validation: {errorsFor(mockResFile).length} error{errorsFor(mockResFile).length === 1 ? '' : 's'}
                                    </div>
                                    <ul class="space-y-0.5">
                                        {#each errorsFor(mockResFile) as e}
                                            <li><span class="font-mono">{e.path}</span>: {e.err}</li>
                                        {/each}
                                    </ul>
                                </div>
                            {/if}
                        </div>
                    </section>
                </div>
            {/if}
        </div>
    </div>
{/if}
