<script lang="ts">
    import { getTrace, type TraceResponse, type TraceStep } from '../lib/api'
    import { store } from '../lib/store.svelte'
    import Ago from './Ago.svelte'
    import CopyButton from './CopyButton.svelte'
    import JsonPre from './JsonPre.svelte'

    interface Props {
        rid: string
        onBack: () => void
    }
    let { rid, onBack }: Props = $props()

    // Strip the trailing /<scope> from a stack identifier so the
    // selectStack target matches what the UI uses elsewhere. Traces
    // record values like "boot/web/0" or "hello-world/0"; the hash
    // form is "#stack/<name>" without the scope.
    function stripScope(s: string): string {
        return s.replace(/\/\d+$/, '')
    }
    function gotoStack(s: string) {
        const name = stripScope(s)
        if (name) store.selectStack(name)
    }

    let trace = $state<TraceResponse | null>(null)
    let loading = $state(true)
    let error = $state('')
    let expandedStep = $state<Record<string, boolean>>({})

    // Fetch when rid changes. Prefer the live-trace cache (populated
    // by TracesList as events arrive over /traces/stream) — this
    // short-circuits the archive lookup, which returns 404 for healthy
    // traces under the default on-error archive policy. Only when the
    // cache misses do we hit the server. The cached event carries the
    // full step list + in/out payloads, same shape as ?include=full.
    $effect(() => {
        const target = rid
        loading = true
        error = ''
        const cached = store.getCachedTrace(target)
        if (cached) {
            trace = cached as unknown as TraceResponse
            loading = false
            return
        }
        ;(async () => {
            try {
                const t = await getTrace(target)
                if (target !== rid) return
                if (!t) {
                    error = 'trace not found'
                    trace = null
                } else {
                    trace = t
                    error = ''
                }
            } catch (e) {
                error = e instanceof Error ? e.message : String(e)
            } finally {
                loading = false
            }
        })()
    })

    function statusBadgeClass(s?: string): string {
        switch (s) {
            case 'ok':
                return 'bg-emerald-100 text-emerald-800'
            case 'error':
                return 'bg-red-100 text-red-800'
            case 'in-flight':
                return 'bg-amber-100 text-amber-900'
            default:
                return 'bg-neutral-100 text-neutral-700'
        }
    }

    function transportBadgeClass(t?: string): string {
        switch (t) {
            case 'mock':
                return 'bg-brand-cyan/10 text-neutral-900 border border-brand-cyan/40'
            case 'http':
            case 'https':
                return 'bg-sky-100 text-sky-800'
            case 'txco':
                return 'bg-purple-100 text-purple-800'
            case 'goto':
                return 'bg-amber-100 text-amber-900'
            case 'noop':
                return 'bg-neutral-100 text-neutral-600'
            default:
                return 'bg-neutral-100 text-neutral-600'
        }
    }

    function formatDuration(ms?: number): string {
        if (typeof ms !== 'number' || !isFinite(ms) || ms < 0) return '—'
        if (ms < 1) return '<1 ms'
        if (ms < 10) return ms.toFixed(1) + ' ms'
        return Math.round(ms) + ' ms'
    }

    function formatBytes(n?: number): string {
        if (typeof n !== 'number' || !isFinite(n) || n < 0) return '—'
        if (n < 1024) return n + ' B'
        if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB'
        return (n / (1024 * 1024)).toFixed(1) + ' MB'
    }

    function stepLabel(s: TraceStep): string {
        // Matches the on-disk steps/<scope>-<name>/ folder naming —
        // 4-digit scope so the column lines up across rows.
        const scope = String(s.scope ?? 0).padStart(4, '0')
        return `${scope}-${s.name}`
    }

    // Per-stack tone: stable color picked by hashing the stack name so
    // the same stack lands on the same color across runs. _sys/* always
    // renders neutral — system steps shouldn't compete visually with
    // app steps the user is actually trying to debug. Full Tailwind
    // class strings (not concatenated) so the JIT scanner picks them up.
    const stackPalette = [
        { border: 'border-l-purple-500', bg: 'bg-purple-100', text: 'text-purple-800' },
        { border: 'border-l-sky-500',    bg: 'bg-sky-100',    text: 'text-sky-800' },
        { border: 'border-l-amber-500',  bg: 'bg-amber-100',  text: 'text-amber-900' },
        { border: 'border-l-emerald-500',bg: 'bg-emerald-100',text: 'text-emerald-800' },
        { border: 'border-l-rose-500',   bg: 'bg-rose-100',   text: 'text-rose-800' },
    ]
    const sysTone = { border: 'border-l-neutral-300', bg: 'bg-neutral-100', text: 'text-neutral-600' }
    function stackTone(stack?: string) {
        if (!stack) return sysTone
        const name = stripScope(stack)
        if (name.startsWith('_sys/') || name === '_sys') return sysTone
        let h = 0
        for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) | 0
        return stackPalette[Math.abs(h) % stackPalette.length]
    }

    function stepKey(s: TraceStep, i: number): string {
        // Two steps in the same scope with the same name shouldn't
        // collide; fold in the index as a tiebreaker.
        return `${i}:${stepLabel(s)}`
    }

    function toggleStep(key: string) {
        if (expandedStep[key]) {
            delete expandedStep[key]
        } else {
            expandedStep[key] = true
        }
    }

    // Display steps in true execution order (started_at) rather than
    // lexicographic step-label order — otherwise scope-50 rows from
    // multiple stacks shuffle together by name and the boot→app→boot
    // flow becomes invisible. JS sort is stable, so steps without a
    // timestamp (or with identical timestamps) preserve array order.
    function stepStart(s: TraceStep): number {
        return s.started_at ? Date.parse(s.started_at) : 0
    }
    const orderedSteps = $derived((trace?.steps ?? []).slice().sort((a, b) => stepStart(a) - stepStart(b)))

    const rawUrl = $derived(`/traces/requests/${encodeURIComponent(rid)}/`)
    // Capture into locally-named refs so the template doesn't have to
    // wrestle the narrowing back from `trace.stack` (which the type
    // checker sees as possibly undefined inside async closures).
    const stackName = $derived(trace?.stack ?? '')
    const routeName = $derived(trace?.route ?? '')
</script>

<div class="flex h-full flex-col p-4">
    <header class="mb-3 flex items-start gap-3">
        <button
            type="button"
            class="rounded border border-neutral-300 bg-white px-2 py-0.5 text-xs text-neutral-700 hover:bg-neutral-50"
            onclick={onBack}
        >
            ← back
        </button>
        <div class="flex-1">
            <h2 class="flex items-center gap-2 font-mono text-base font-semibold text-neutral-900">
                <span title={rid}>{rid}</span>
                <CopyButton text={rid} title="copy rid" />
            </h2>
            <p class="text-xs text-neutral-500">
                <a class="underline decoration-neutral-300 hover:decoration-neutral-700"
                   href={rawUrl} target="_blank" rel="noopener">
                    open raw
                </a>
            </p>
        </div>
    </header>

    {#if loading && !trace}
        <p class="text-sm italic text-neutral-400">loading…</p>
    {:else if error}
        <p class="rounded border border-red-300 bg-red-50 p-2 text-xs text-red-800">{error}</p>
    {:else if trace}
        <section class="mb-4 rounded border border-neutral-200 bg-white p-3 text-sm">
            <div class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
                <span class="text-neutral-500">source</span>
                <span class="font-mono text-neutral-800">{trace.src ?? '—'}</span>
                <span class="text-neutral-500">stack</span>
                <span class="font-mono text-neutral-800">
                    {#if stackName}
                        <button
                            type="button"
                            class="underline decoration-neutral-400 underline-offset-2 hover:text-neutral-900 hover:decoration-neutral-700"
                            title="open {stripScope(stackName)}"
                            onclick={() => gotoStack(stackName)}
                        >{stackName}</button>
                    {:else}
                        —
                    {/if}{#if routeName}
                        <span class="text-neutral-400">→</span>
                        <button
                            type="button"
                            class="underline decoration-neutral-400 underline-offset-2 hover:text-neutral-900 hover:decoration-neutral-700"
                            title="open {stripScope(routeName)}"
                            onclick={() => gotoStack(routeName)}
                        >{routeName}</button>
                    {/if}
                </span>
                <span class="text-neutral-500">started</span>
                <span class="font-mono text-neutral-400"><Ago at={trace.started_at} /></span>
                <span class="text-neutral-500">duration</span>
                <span class="font-mono text-neutral-800">{formatDuration(trace.duration_ms)}</span>
                {#if typeof trace.bytes_in === 'number' || typeof trace.bytes_out === 'number'}
                    <span class="text-neutral-500">bytes in / out</span>
                    <span class="font-mono text-neutral-800">{formatBytes(trace.bytes_in)} <span class="text-neutral-400">/</span> {formatBytes(trace.bytes_out)}</span>
                {/if}
                {#if typeof trace.fuel === 'number'}
                    <span class="text-neutral-500">fuel</span>
                    <span class="font-mono text-neutral-800">{trace.fuel.toLocaleString()}</span>
                {/if}
                <span class="text-neutral-500">status</span>
                <span>
                    <span class="rounded px-1.5 py-0.5 text-[11px] font-medium {statusBadgeClass(trace.status)}">
                        {trace.status ?? '—'}
                    </span>
                </span>
            </div>
        </section>

        {#if trace.continuation}
            {@const c = trace.continuation}
            <section class="mb-4 rounded border border-brand-cyan/50 bg-white p-3 text-xs">
                <div class="mb-2 font-medium text-neutral-700">
                    continuation
                    {#if c.run_continuation_id}
                        <span class="ml-2 font-mono text-neutral-400">{c.run_continuation_id}</span>
                    {/if}
                </div>
                <div class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
                    <span class="text-neutral-500">originating request</span>
                    <span class="font-mono">
                        {#if c.origin_rid && c.origin_rid !== trace.rid}
                            {@const orid = c.origin_rid}
                            <button
                                class="underline decoration-neutral-400 underline-offset-2 hover:text-neutral-900 hover:decoration-neutral-700"
                                onclick={() => store.showTraces(orid)}>{orid}</button>
                        {:else if c.origin_rid}
                            {c.origin_rid} <span class="text-neutral-400">(this)</span>
                        {:else}
                            <span class="text-neutral-400">—</span>
                        {/if}
                    </span>
                    {#if c.resumes && c.resumes.length}
                        <span class="text-neutral-500">resumes</span>
                        <span class="flex flex-col gap-0.5 font-mono">
                            {#each c.resumes as rr}
                                {#if rr.rid === trace.rid}
                                    <span>{rr.stage} <span class="text-neutral-400">(this)</span></span>
                                {:else}
                                    {@const rrid = rr.rid}
                                    <button
                                        class="text-left underline decoration-neutral-400 underline-offset-2 hover:text-neutral-900 hover:decoration-neutral-700"
                                        onclick={() => store.showTraces(rrid)}>{rr.stage}</button>
                                {/if}
                            {/each}
                        </span>
                    {/if}
                </div>
            </section>
        {/if}

        <section class="overflow-auto rounded border border-neutral-200 bg-white">
            <table class="w-full text-sm">
                <thead class="border-b border-neutral-200 bg-neutral-50 text-left text-xs uppercase tracking-wide text-neutral-500">
                    <tr>
                        <th class="px-3 py-2 font-medium">step</th>
                        <th class="px-3 py-2 font-medium">operation</th>
                        <th class="px-3 py-2 font-medium">transport</th>
                        <th class="px-3 py-2 font-medium">dur</th>
                        <th class="px-3 py-2 font-medium">status</th>
                    </tr>
                </thead>
                <tbody class="divide-y divide-neutral-100">
                    {#if (orderedSteps).length === 0}
                        <tr>
                            <td class="px-3 py-2 text-sm italic text-neutral-400" colspan="5">
                                no steps recorded
                            </td>
                        </tr>
                    {:else}
                        {#each orderedSteps as s, i (stepKey(s, i))}
                            {@const key = stepKey(s, i)}
                            {@const expanded = !!expandedStep[key]}
                            {@const tone = stackTone(s.stack)}
                            {@const prevStack = i > 0 ? stripScope((orderedSteps)[i - 1].stack ?? '') : ''}
                            {@const curStack = stripScope(s.stack ?? '')}
                            {@const stackChanged = i > 0 && prevStack !== curStack}
                            <tr class="cursor-pointer border-l-4 hover:bg-neutral-50 {tone.border} {stackChanged ? 'border-t-2 border-t-neutral-300' : ''}"
                                onclick={() => toggleStep(key)}>
                                <td class="px-3 py-2 align-top font-mono text-xs text-neutral-700">
                                    <span class="mr-1 inline-block w-3 text-neutral-400 transition-transform {expanded ? 'rotate-90' : ''}">▶</span>{stepLabel(s)}
                                    {#if curStack}
                                        <span class="ml-2 rounded px-1.5 py-0.5 text-[10px] font-medium {tone.bg} {tone.text}" title="stack {curStack}">
                                            {curStack}
                                        </span>
                                    {/if}
                                </td>
                                <td class="px-3 py-2 align-top font-mono text-xs text-neutral-800" title={s.operation ?? ''}>
                                    {s.operation ?? '—'}
                                </td>
                                <td class="px-3 py-2 align-top">
                                    <span class="rounded px-1.5 py-0.5 text-[10px] font-medium {transportBadgeClass(s.transport)}">
                                        {s.transport ?? '—'}
                                    </span>
                                </td>
                                <td class="px-3 py-2 align-top font-mono text-xs text-neutral-600">
                                    {formatDuration(s.duration_ms)}
                                </td>
                                <td class="px-3 py-2 align-top">
                                    <span class="rounded px-1.5 py-0.5 text-[11px] font-medium {statusBadgeClass(s.status)}">
                                        {s.status ?? '—'}
                                    </span>
                                </td>
                            </tr>
                            {#if expanded}
                                <tr class="border-l-4 bg-neutral-50 {tone.border}">
                                    <td colspan="5" class="px-3 py-2">
                                        {#if s.error}
                                            <pre class="mb-2 whitespace-pre-wrap rounded bg-red-50 p-1.5 text-left text-[11px] text-red-800">{s.error}</pre>
                                        {/if}
                                        <div class="grid grid-cols-1 gap-3 lg:grid-cols-2">
                                            <section class="flex min-w-0 flex-col">
                                                <div class="mb-1 px-1 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">in</div>
                                                <div class="min-w-0">
                                                    <JsonPre value={s.in} emptyLabel="no input recorded" />
                                                </div>
                                            </section>
                                            <section class="flex min-w-0 flex-col">
                                                <div class="mb-1 px-1 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">out</div>
                                                <div class="min-w-0">
                                                    <JsonPre value={s.out} emptyLabel="no output recorded" />
                                                </div>
                                            </section>
                                        </div>
                                    </td>
                                </tr>
                            {/if}
                        {/each}
                    {/if}
                </tbody>
            </table>
        </section>
    {/if}
</div>
