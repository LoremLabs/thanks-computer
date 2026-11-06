<script lang="ts">
    import { COMPUTE_REF, getTrace, getTraceBody, traceRawUrl, type TraceResponse, type TraceStep } from '../lib/api'
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
    // A step's bodies when they are not in the document, by step key.
    let fetched = $state<Record<string, { in?: unknown; out?: unknown }>>({})
    let fetching = $state<Record<string, 'loading' | 'unavailable'>>({})
    let modelCallsOnly = $state(false)

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
        fetched = {}
        fetching = {}
        const cached = store.getCachedTrace(target)
        if (cached) {
            trace = cached as unknown as TraceResponse
            loading = false
            return
        }
        ;(async () => {
            try {
                const t = await getTrace(store.state.currentTenant, target)
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

    // The op a step ran, as an #ops deep link to its definition (the
    // stack's active version). The step's name and its operation both link
    // there, except a compute's operation, which opens its source. Only the
    // tenant's own stacks link — boot and other system steps have no op to
    // open here.
    const tenantStacks = $derived(new Set(store.state.stacks.map((st) => st.name)))
    function stepOpHref(s: TraceStep): string {
        const stack = stripScope(s.stack ?? '')
        if (!stack || !s.name || !tenantStacks.has(stack)) return ''
        return `#ops/${stack}/${s.scope ?? 0}/${s.name}`
    }

    // A compute step's operation (`compute://sha256/<digest>`) links to its
    // source, read through the step's own stack — tenant stacks only.
    function stepComputeHref(s: TraceStep): string {
        const m = COMPUTE_REF.exec(s.operation ?? '')
        const stack = stripScope(s.stack ?? '')
        if (!m || !stack || !tenantStacks.has(stack)) return ''
        return `#compute/${stack}/${m[1]}`
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

    function toggleStep(key: string, s: TraceStep) {
        if (expandedStep[key]) {
            delete expandedStep[key]
        } else {
            expandedStep[key] = true
            loadBody(key, s)
        }
    }

    // A long trace keeps its bodies out of its document: fetch a step's when
    // it is opened, once.
    async function loadBody(key: string, s: TraceStep) {
        if (!s.body_ref || s.in !== undefined || s.out !== undefined || fetched[key] || fetching[key]) return
        const target = rid
        fetching[key] = 'loading'
        const body = await getTraceBody(store.state.currentTenant, target, s.body_ref).catch(() => null)
        if (target !== rid) return
        if (body) {
            fetched[key] = { in: body.in ?? undefined, out: body.out ?? undefined }
            delete fetching[key]
        } else {
            fetching[key] = 'unavailable'
        }
    }

    function bodyLabel(key: string, s: TraceStep, side: 'in' | 'out'): string {
        const state = fetching[key]
        if (state === 'loading') return 'loading…'
        if (state === 'unavailable') return 'not available: this body was not stored'
        const [cut, n] = side === 'in' ? [s.input_truncated, s.input_bytes] : [s.output_truncated, s.output_bytes]
        if (cut) return `not kept, ${formatBytes(n)}`
        return side === 'in' ? 'no input recorded' : 'no output recorded'
    }

    function modelLine(s: TraceStep): string {
        const m = s.model
        if (!m) return ''
        const name = m.model || m.kind
        if (m.error_code) return `${name} ${m.error_code}`
        const n = (v?: number) => (v ?? 0).toLocaleString()
        return m.kind === 'embed' ? `${name} ${n(m.tokens_in)}` : `${name} ${n(m.tokens_in)} → ${n(m.tokens_out)}`
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
    const modelCalls = $derived(orderedSteps.filter((s) => s.model))
    const modelTotals = $derived(
        modelCalls.reduce(
            (sum, s) => ({ in: sum.in + (s.model?.tokens_in ?? 0), out: sum.out + (s.model?.tokens_out ?? 0) }),
            { in: 0, out: 0 },
        ),
    )
    // A step keeps its key, and so its open row and fetched bodies, whichever
    // way the table is filtered.
    const rows = $derived(
        orderedSteps.map((s, i) => ({ s, i })).filter(({ s }) => !modelCallsOnly || s.model),
    )

    const rawUrl = $derived(traceRawUrl(store.state.currentTenant, rid))
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
                {#if trace.tags && Object.keys(trace.tags).length}
                    <span class="text-neutral-500">tags</span>
                    <span class="flex flex-wrap gap-1.5 font-mono text-neutral-800">
                        {#each Object.entries(trace.tags).sort() as [k, v]}
                            <span><span class="text-neutral-400">{k}=</span>{v}</span>
                        {/each}
                    </span>
                {/if}
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

        {#if modelCalls.length}
            <div class="mb-2 flex items-center gap-3 text-xs text-neutral-600">
                <span>
                    {modelCalls.length} model {modelCalls.length === 1 ? 'call' : 'calls'},
                    <span class="font-mono">{modelTotals.in.toLocaleString()} → {modelTotals.out.toLocaleString()}</span> tokens
                </span>
                <button
                    type="button"
                    class="rounded border border-neutral-300 bg-white px-2 py-0.5 text-neutral-700 hover:bg-neutral-50"
                    aria-pressed={modelCallsOnly}
                    onclick={() => (modelCallsOnly = !modelCallsOnly)}
                >{modelCallsOnly ? 'show all steps' : 'show model calls'}</button>
            </div>
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
                        {#each rows as { s, i }, n (stepKey(s, i))}
                            {@const key = stepKey(s, i)}
                            {@const expanded = !!expandedStep[key]}
                            {@const tone = stackTone(s.stack)}
                            {@const prevStack = n > 0 ? stripScope(rows[n - 1].s.stack ?? '') : ''}
                            {@const curStack = stripScope(s.stack ?? '')}
                            {@const stackChanged = n > 0 && prevStack !== curStack}
                            {@const opHref = stepOpHref(s)}
                            {@const computeHref = stepComputeHref(s)}
                            {@const body = fetched[key]}
                            <tr class="cursor-pointer border-l-4 hover:bg-neutral-50 {tone.border} {stackChanged ? 'border-t-2 border-t-neutral-300' : ''}"
                                onclick={() => toggleStep(key, s)}>
                                <td class="px-3 py-2 align-top font-mono text-xs text-neutral-700">
                                    {#if modelCallsOnly}<span class="mr-1 text-neutral-400">{n + 1}.</span>{/if}<span class="mr-1 inline-block w-3 text-neutral-400 transition-transform {expanded ? 'rotate-90' : ''}">▶</span>{#if opHref}<a
                                        href={opHref}
                                        class="underline-offset-2 hover:text-neutral-900 hover:underline"
                                        title="open {opHref.slice('#ops/'.length)}"
                                        onclick={(e) => e.stopPropagation()}
                                    >{stepLabel(s)}</a>{:else}{stepLabel(s)}{/if}
                                    {#if curStack}
                                        <span class="ml-2 rounded px-1.5 py-0.5 text-[10px] font-medium {tone.bg} {tone.text}" title="stack {curStack}">
                                            {curStack}
                                        </span>
                                    {/if}
                                </td>
                                <td class="px-3 py-2 align-top font-mono text-xs text-neutral-800" title={s.operation ?? ''}>
                                    {#if computeHref}<a
                                        href={computeHref}
                                        class="underline-offset-2 hover:text-neutral-900 hover:underline"
                                        title="open this compute's source"
                                        onclick={(e) => e.stopPropagation()}
                                    >{s.operation}</a>{:else if opHref && s.operation}<a
                                        href={opHref}
                                        class="underline-offset-2 hover:text-neutral-900 hover:underline"
                                        title="open {opHref.slice('#ops/'.length)}"
                                        onclick={(e) => e.stopPropagation()}
                                    >{s.operation}</a>{:else}{s.operation ?? '—'}{/if}
                                    {#if s.model}
                                        <span class="ml-2 rounded px-1.5 py-0.5 text-[10px] font-medium {s.model.error_code ? 'bg-red-100 text-red-800' : 'bg-brand-cyan/15 text-neutral-700'}"
                                              title="{s.model.provider ?? ''} {s.model.latency_ms ?? 0} ms{s.model.retries ? `, ${s.model.retries} retries` : ''}">
                                            {modelLine(s)}{#if s.model.retries} ×{s.model.retries + 1}{/if}
                                        </span>
                                    {/if}
                                </td>
                                <td class="px-3 py-2 align-top">
                                    <span class="rounded px-1.5 py-0.5 text-[10px] font-medium {transportBadgeClass(s.transport)}">
                                        {s.transport ?? '—'}
                                    </span>
                                </td>
                                <td class="px-3 py-2 align-top font-mono text-xs text-neutral-600">
                                    {formatDuration(s.duration_ms)}
                                    {#if s.passes}
                                        <span class="ml-1 rounded bg-neutral-100 px-1.5 py-0.5 text-[10px] font-medium text-neutral-700"
                                              title="repeated {s.passes} pass(es); stopped: {s.stop_reason ?? '?'}">
                                            ×{s.passes} {s.stop_reason ?? ''}
                                        </span>
                                    {/if}
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
                                                    <JsonPre value={s.in ?? body?.in} emptyLabel={bodyLabel(key, s, 'in')} />
                                                </div>
                                            </section>
                                            <section class="flex min-w-0 flex-col">
                                                <div class="mb-1 px-1 text-[11px] font-semibold uppercase tracking-wide text-neutral-500">out</div>
                                                <div class="min-w-0">
                                                    <JsonPre value={s.out ?? body?.out} emptyLabel={bodyLabel(key, s, 'out')} />
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
