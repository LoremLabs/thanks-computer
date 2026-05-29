<script lang="ts">
    import { onMount } from 'svelte'
    import CodeEditor from './CodeEditor.svelte'
    import JsonPre from './JsonPre.svelte'
    import StackView from './StackView.svelte'
    import TxclHelp from './TxclHelp.svelte'
    import { opId, type Op } from '../lib/types'
    import {
        tracks,
        allSteps,
        DEMO_HOST_SUFFIX,
        type OpFile,
        type Method,
        type Step,
    } from '../lib/tutorial'
    import {
        createDraft,
        putDraftFiles,
        validateVersion,
        activate,
        bindHostname,
        fireRequest,
        getDemoInfo,
        getTrace,
        buildDemoOp,
        type FireResult,
        type TraceResponse,
        type StackFile,
    } from '../lib/api'

    // `running` is owned by the parent (App) so the Run CTA can live in
    // the header; run() sets it. run() is exported so the header button
    // can trigger it via bind:this.
    let {
        running = $bindable(false),
    }: { running?: boolean } = $props()

    // Optional scope to break at when "Open" is clicked. null = no break;
    // a number = `?_txc.break=<currentStack>/<scope>` appended to the URL.
    // The chassis only honors this if started with --debug-breakpoints
    // (which `txco demo` enables); otherwise it's a benign no-op.
    let breakScope = $state<number | null>(null)

    const TENANT = 'default'

    // hostnameFor maps a stack name to the hostname bound to it. The
    // wildcard *.local.thanks.computer resolves to 127.0.0.1 publicly, so
    // these "look like real subdomains" but stay loopback.
    function hostnameFor(stack: string): string {
        return `${stack}.${DEMO_HOST_SUFFIX}`
    }

    // Ops + request + current stack are seeded from the walkthrough's
    // first step. Each step has its OWN stack name (set by tutorial.ts
    // withStacks); `currentStack` tracks which one is "live" — its
    // hostname is what fired requests target.
    const seed = tracks[0].steps[0]
    let currentStack = $state(seed.stack!)
    let ops = $state<OpFile[]>(seed.ops.map((o) => ({ ...o })))
    let method = $state<Method>(seed.method)
    let path = $state(seed.path)
    let body = $state(seed.body ?? '')

    let error = $state('')
    // Web-inlet port (data plane), fetched from the chassis — the curl
    // targets it, not the admin port this UI is served from.
    let webPort = $state('')
    // True once all N step-stacks have been seeded (createDraft + put +
    // activate + bindHostname). Tab switches that happen before this
    // finishes auto-wait via `run({apply: false})` — but if a user clicks
    // Run before seed completes, the explicit apply path covers it.
    let seedReady = $state(false)
    onMount(async () => {
        // Best-effort: web-inlet port for copy-as-curl.
        try {
            const info = await getDemoInfo()
            if (info) webPort = info.web_port
        } catch {
            // leave empty — curl falls back to the page origin
        }

        // Seed every step's stack in parallel. Each step gets its own
        // stack with its own hostname binding so tab switches are
        // pure-navigation (no apply). ~15 stacks × 4 admin-API calls each,
        // all parallel against localhost — usually <1s wall-clock.
        try {
            await Promise.all(
                allSteps().map(({ stack, step }) =>
                    ensureStack(stack, step.ops)
                )
            )
            seedReady = true
        } catch (e) {
            error = e instanceof Error ? e.message : String(e)
        }

        // Auto-run the default first step (build-1) so the user sees
        // output immediately. The stack was just seeded above, so this is
        // a pure fire (no second apply).
        void run({ apply: false })
    })

    let result = $state<FireResult | null>(null)
    let trace = $state<TraceResponse | null>(null)
    // Trace default hides the boot routing steps so you see just your
    // op; flip to include them.
    let showBoot = $state(false)

    const showBody = $derived(method === 'POST' || method === 'PUT')

    // A step belongs to the system boot pipeline (tenant detection +
    // routing, etc.) rather than the user's op. With per-step stacks,
    // "the user's op" means the currently-loaded step's stack — anything
    // else is system plumbing we hide by default.
    function isBootStep(stack: string): boolean {
        return stack !== currentStack
    }

    // Trace steps to show: the user's op by default; boot steps too
    // when the toggle is on. A step's own `out` is the op's output
    // INCLUDING its EMIT overlay (the chassis records it post-emit), so
    // an EMIT-only op shows the fields it contributed.
    //
    // Ordered by start time, not recording order — the recorded order
    // doesn't match execution time (boot detect runs, routes INTO play,
    // so play actually starts last even though it's recorded early).
    //
    // Date.parse only resolves to MILLISECONDS, so steps in the same ms
    // tie and fall back to (unreliable) recording order. The chassis
    // timestamps carry microseconds (RFC3339Nano), so compare the
    // sub-millisecond fractional digits too, then finished_at, to break
    // those ties.
    const subMicros = (iso?: string): number => {
        // Fractional-second digits beyond the millisecond (i.e. the
        // micro/nano part), as a comparable integer. "…709371Z" → 371.
        const m = (iso ?? '').match(/\.(\d+)/)
        if (!m) return 0
        return Number((m[1].slice(3) + '000000').slice(0, 6))
    }
    const at = (iso?: string): number => {
        const ms = Date.parse(iso ?? '')
        return Number.isNaN(ms) ? 0 : ms
    }
    const cmpTime = (a?: string, b?: string): number =>
        at(a) - at(b) || subMicros(a) - subMicros(b)
    const visibleSteps = $derived.by(() => {
        const all = trace?.steps ?? []
        const shown = showBoot ? all : all.filter((s) => !isBootStep(s.stack))
        return [...shown].sort(
            (a, b) =>
                cmpTime(a.started_at, b.started_at) ||
                cmpTime(a.finished_at, b.finished_at)
        )
    })

    // Output is shown PROD-shaped by default: private (underscore-
    // prefixed) keys stripped, like the web inlet's production projection.
    // The "show debug" toggle in the merged-result row reveals the full
    // envelope incl `_txc` (where EMITs land). The per-op trace in/out
    // below is always full — that's the inspection view.
    let showDebug = $state(false)

    const envelope = $derived<unknown>(trace?.out ?? null)

    // Remove top-level private (`_`-prefixed) keys. null/arrays/scalars
    // pass through unchanged.
    function stripPrivate(v: unknown): unknown {
        if (v && typeof v === 'object' && !Array.isArray(v)) {
            const out: Record<string, unknown> = {}
            for (const [k, val] of Object.entries(v)) {
                if (!k.startsWith('_')) out[k] = val
            }
            return out
        }
        return v
    }

    // Merged result, prod-shaped unless debug.
    const displayEnvelope = $derived(showDebug ? envelope : stripPrivate(envelope))

    // Response body: JSON pretty-printed (prod-shaped unless debug);
    // non-JSON bodies (e.g. an EMITted text/HTML body) pass through.
    const prettyBody = $derived.by<string>(() => {
        const raw = result?.body ?? ''
        if (!raw) return ''
        try {
            const parsed = JSON.parse(raw)
            return JSON.stringify(showDebug ? parsed : stripPrivate(parsed), null, 2)
        } catch {
            return raw
        }
    })

    // Live preview iframe URL — points at the web inlet directly (not at the
    // /v1/demo/fire JSON proxy), so the iframe loads with BROWSER-default
    // headers (Accept: text/html, …). Ops that branch on the Accept header
    // (e.g. "serve JSON to API clients, HTML to browsers") will return their
    // user-facing shape here, while the "raw" pane below still shows what the
    // demo's JSON fire saw. The same browser will also honor 3xx
    // redirects and meta-refresh natively — useful for the continuations
    // pattern. Latched ON RUN (not on every keystroke) so the iframe doesn't
    // re-fire as the user types in path/body; a cache-bust query forces a
    // fresh fetch each Run even when the URL is unchanged. Only set for GET
    // requests — iframes can't issue arbitrary methods via `src`.
    let liveUrl = $state('')

    // --- op-stack diagram inputs (reuses admin-ui's StackView) ----------
    // The user's authored stack: one op at scope 0 of `play`. Shown
    // before a run (structure) and enriched with durations after.
    const playOps = $derived<Op[]>(
        ops.map((o) => ({ stack: currentStack, scope: o.scope, name: o.name, txcl: o.txcl }))
    )

    // Which op's editor is shown: selected by clicking a box in the
    // op-stack diagram. Defaults to the first op.
    let selectedName = $state(seed.ops[0].name)
    const selectedOp = $derived(ops.find((o) => o.name === selectedName) ?? ops[0])
    const selectedId = $derived(
        selectedOp
            ? opId({ stack: currentStack, scope: selectedOp.scope, name: selectedOp.name, txcl: '' })
            : ''
    )
    function updateSelected(v: string) {
        const i = ops.findIndex((o) => o.name === selectedName)
        if (i >= 0) ops[i].txcl = v
    }
    // Mirror of updateSelected for the JS textarea — only writes when
    // the selected op already carries a `js` field (so editing in the
    // Build/API tracks, which don't have it, is a no-op).
    function updateSelectedJs(v: string) {
        const i = ops.findIndex((o) => o.name === selectedName)
        if (i >= 0 && ops[i].js !== undefined) ops[i].js = v
    }

    // Per-op durations keyed by opId, from ALL of this run's steps.
    // StackView only looks up the ops it renders, so covering boot too
    // is harmless and lets the boot diagram show timings when shown.
    const durations = $derived.by<Record<string, number>>(() => {
        const out: Record<string, number> = {}
        for (const s of trace?.steps ?? []) {
            if (typeof s.duration_ms === 'number') {
                out[opId({ stack: s.stack, scope: s.scope, name: s.name, txcl: '' })] =
                    s.duration_ms
            }
        }
        return out
    })

    // System (boot) ops reconstructed from the trace — we have only the
    // executed (stack, scope, name), not their source txcl.
    const bootOps = $derived.by<Op[]>(() => {
        const seen = new Set<string>()
        const out: Op[] = []
        for (const s of trace?.steps ?? []) {
            if (!isBootStep(s.stack)) continue
            const id = `${s.stack}/${s.scope}/${s.name}`
            if (seen.has(id)) continue
            seen.add(id)
            out.push({ stack: s.stack, scope: s.scope, name: s.name, txcl: '' })
        }
        return out
    })

    // One diagram per stack: the `play` stack always; the system
    // stack(s) too when "show boot steps" is on (boot routes INTO play,
    // so it's drawn first).
    const stackDiagrams = $derived.by<
        { stack: string; ops: Op[]; total: number | undefined }[]
    >(() => {
        // Stack total = WALL-CLOCK, not the sum of per-op durations.
        // Ops at the same scope run in parallel, so summing double-counts
        // them (two 168ms parallel calls are ~168ms of wall time, not
        // 336ms). Mirrors admin-ui's stackWallclockMs: span from the
        // earliest start to the latest finish; if timestamps are missing,
        // fall back to per-scope max summed across scopes (parallel within
        // a scope, sequential across), then a flat sum as a last resort.
        const total = (stack: string): number | undefined => {
            const steps = (trace?.steps ?? []).filter((s) => s.stack === stack)
            if (steps.length === 0) return undefined

            let minStart = Infinity
            let maxEnd = -Infinity
            let timestampsComplete = true
            for (const s of steps) {
                const start = Date.parse(s.started_at ?? '')
                const end = Date.parse(s.finished_at ?? '')
                if (!Number.isFinite(start) || !Number.isFinite(end)) {
                    timestampsComplete = false
                    break
                }
                if (start < minStart) minStart = start
                if (end > maxEnd) maxEnd = end
            }
            if (timestampsComplete && maxEnd >= minStart) {
                return Math.max(0, maxEnd - minStart)
            }

            const perScope = new Map<number, number>()
            let sawScope = false
            let flatSum = 0
            for (const s of steps) {
                if (typeof s.duration_ms !== 'number') continue
                flatSum += s.duration_ms
                if (typeof s.scope === 'number') {
                    sawScope = true
                    const cur = perScope.get(s.scope) ?? 0
                    if (s.duration_ms > cur) perScope.set(s.scope, s.duration_ms)
                }
            }
            if (sawScope) {
                let summed = 0
                for (const v of perScope.values()) summed += v
                return summed
            }
            return flatSum
        }
        const out: { stack: string; ops: Op[]; total: number | undefined }[] = []
        if (showBoot) {
            const byStack = new Map<string, Op[]>()
            for (const o of bootOps) {
                const list = byStack.get(o.stack) ?? []
                list.push(o)
                byStack.set(o.stack, list)
            }
            for (const [stack, ops] of byStack) {
                out.push({ stack, ops, total: total(stack) })
            }
        }
        out.push({ stack: currentStack, ops: playOps, total: total(currentStack) })
        return out
    })

    // The URL the demo is firing — same shape as the iframe's `liveUrl`
    // but WITHOUT the cache-bust query, so a user pasting it into a browser
    // or `curl` gets the exact request shape (and can keep refreshing to
    // re-hit it). Hostname is the currentStack's bound subdomain
    // (`<stack>.local.thanks.computer`) which routes to the right stack
    // via the wildcard DNS + chassis ingress. Falls back to the page's
    // own origin only if the web port hasn't loaded yet.
    const testUrl = $derived.by<string>(() => {
        if (!webPort) return `${location.origin}${path}`
        return `${location.protocol}//${hostnameFor(currentStack)}:${webPort}${path}`
    })
    // openInWindow builds the test URL (testUrl + an optional
    // ?_txc.break=<stack>/<scope> when breakScope is set) and opens it in
    // a new tab. The break form is `<currentStack>/<scope>` so it only
    // triggers inside the user's authored stack, not in boot routing.
    function openInWindow(): void {
        let url = testUrl
        if (breakScope !== null) {
            const sep = url.includes('?') ? '&' : '?'
            url = `${url}${sep}_txc.break=${encodeURIComponent(currentStack)}/${breakScope}`
        }
        window.open(url, '_blank', 'noopener,noreferrer')
    }

    // ensureStack: createDraft + put + validate + activate + bindHostname
    // for one stack. Called once per step at mount-time (parallel seed),
    // and again for the currentStack inside run({apply: true}) when the
    // user re-runs after editing. Idempotent at the chassis level —
    // bindHostname returns 409 if already bound, which the api.ts wrapper
    // treats as success.
    async function ensureStack(stack: string, opList: OpFile[]): Promise<void> {
        // For each op that carries a JS compute, build it server-side
        // first and splice the resulting `compute://sha256/…` ref into
        // that op's txcl in place of `op://<name>`. Same regex the
        // server-side resolver uses (`chassis/cli/oprefs/resolve.go`).
        // One JS per op = one EXEC, so the substitution is unambiguous.
        // The wasm cache on the server keys by bundled source, so
        // repeated Runs of unchanged JS skip javy.
        const opRefRe = /"op:\/\/[A-Za-z0-9_-]+"/g
        const resolvedOps = await Promise.all(
            opList.map(async (o) => {
                if (!o.js || !o.js.trim()) return o
                const built = await buildDemoOp(o.js, 'js')
                return { ...o, txcl: o.txcl.replace(opRefRe, `"${built.ref}"`) }
            })
        )
        const files: StackFile[] = resolvedOps.map((o) => ({
            path: `${o.scope}/${o.name}.txcl`,
            content: o.txcl,
        }))
        const draft = await createDraft(TENANT, stack, 'empty')
        await putDraftFiles(TENANT, stack, draft.version_number, files)
        // Validate before activate — the chassis activates invalid txcl
        // without complaint and then hangs the request, so catch parse /
        // ref errors here and surface them.
        const valid = await validateVersion(TENANT, stack, draft.version_number)
        if (!valid.ok) {
            const errs = valid.errors ?? []
            throw new Error(
                `txcl validation failed for ${stack}:\n` +
                    (errs.length
                        ? errs.map((e) => `  ${e.path}: ${e.err}`).join('\n')
                        : '  (no detail returned)')
            )
        }
        await activate(TENANT, stack, draft.version_number)
        // Bind <stack>.local.thanks.computer → stack. Idempotent: 409 on
        // an already-bound hostname is treated as success in api.ts.
        await bindHostname(TENANT, hostnameFor(stack), stack)
    }

    // run() does two things, in two flavors:
    //   apply: true  — re-apply CURRENT step's ops to its stack, then fire.
    //                  Used by the explicit Run button (after edits).
    //   apply: false — just fire against the current step's hostname.
    //                  Used by load() on tab switches — the step's stack
    //                  was already seeded at mount, so re-applying is
    //                  wasted work.
    export async function run({ apply = true }: { apply?: boolean } = {}) {
        running = true
        error = ''
        result = null
        trace = null
        try {
            if (apply) {
                await ensureStack(currentStack, ops)
            }
            const headers: Record<string, string> = {
                Host: hostnameFor(currentStack),
            }
            if (showBody) headers['Content-Type'] = 'application/json'
            const fired = await fireRequest({
                method,
                path,
                headers,
                body: showBody ? body : undefined,
            })
            result = fired
            // Latch the live-preview iframe URL on RUN (not on every
            // keystroke). Cache-bust query forces a fresh fetch each Run.
            if (method === 'GET' && webPort) {
                const sep = path.includes('?') ? '&' : '?'
                liveUrl = `${location.protocol}//${hostnameFor(currentStack)}:${webPort}${path}${sep}_t=${Date.now()}`
            } else {
                liveUrl = ''
            }
            // Trace writes can lag the response (async trace sink), so a
            // fetch right after the response may see a PARTIAL trace (boot
            // steps flushed, the op step + final `out` not yet) — which
            // would wrongly read as "only boot steps ran". Poll until the
            // trace is fully written (request-level finished_at set), then
            // render. Best-effort: render whatever we have if it never
            // settles. A missing trace doesn't blank the response.
            trace = null
            for (let i = 0; i < 12; i++) {
                const t = await getTrace(TENANT, fired.rid).catch(() => null)
                if (t) trace = t
                if (t?.finished_at) break
                await new Promise((r) => setTimeout(r, 200))
            }
        } catch (e) {
            error = e instanceof Error ? e.message : String(e)
        } finally {
            running = false
        }
    }

    // Load a walkthrough step into the demo and fire it. Called by the
    // parent (App) on Prev/Next. Tab switches are pure navigation — the
    // step's stack was seeded at mount, so we change `currentStack`,
    // refresh the editor state from the step's authored ops, and just
    // fire (no re-apply). To pick up user edits, click Run.
    export function load(step: Step) {
        currentStack = step.stack!
        ops = step.ops.map((o) => ({ ...o }))
        method = step.method
        path = step.path
        body = step.body ?? ''
        selectedName = ops[0]?.name ?? ''
        // If the mount-time seed hasn't finished yet (race on a very fast
        // Prev/Next click), apply on the way in so the stack exists before
        // we fire. Otherwise tab switches stay apply-free.
        void run({ apply: !seedReady })
    }
</script>

<div class="flex flex-col gap-5">
    <!-- request bar: method + path, Run flush right -->
    <div class="flex flex-col gap-2">
        <div class="flex items-center gap-2">
            <select
                bind:value={method}
                class="rounded border border-neutral-300 bg-white px-2 py-1.5 font-mono text-sm text-neutral-700"
            >
                <option>GET</option>
                <option>POST</option>
                <option>PUT</option>
                <option>DELETE</option>
            </select>
            <input
                bind:value={path}
                spellcheck="false"
                class="flex-1 rounded border border-neutral-300 bg-white px-2 py-1.5 font-mono text-sm text-neutral-700"
                placeholder="/"
            />
            <select
                bind:value={breakScope}
                title="Optionally pause execution at a given scope (requires --debug-breakpoints on the chassis, which `txco demo` enables)."
                class="rounded border border-neutral-300 bg-neutral-100 px-2 py-1.5 font-mono text-xs text-neutral-700"
            >
                <option value={null}>break at: none</option>
                {#each ops as op}
                    <option value={op.scope}>break at: {op.scope} · {op.name}</option>
                {/each}
            </select>
            <button
                type="button"
                onclick={openInWindow}
                title="open the test URL in a new tab"
                class="rounded border border-neutral-300 bg-neutral-100 px-3 py-1.5 text-sm text-neutral-700 hover:bg-neutral-50"
            >
                Open ↗
            </button>
        </div>
        {#if showBody}
            <textarea
                bind:value={body}
                spellcheck="false"
                rows="4"
                class="rounded border border-neutral-300 bg-white px-2 py-1.5 font-mono text-xs text-neutral-700"
                placeholder={'{ "hello": "world" }'}
            ></textarea>
        {/if}
        {#if error}
            <div class="overflow-auto rounded border border-red-400 bg-red-50 p-2 font-mono text-xs whitespace-pre-wrap text-red-800 max-w-sm">{error}</div>
        {/if}
    </div>

    <!-- main: op-stack selector + selected op editor | outputs -->
    <div class="grid gap-6 lg:grid-cols-2">
        <!-- LEFT: op-stack selector + the selected op's editor -->
        <div class="flex flex-col gap-4">
            <div>
                <h2 class="mb-1 text-sm font-semibold text-neutral-700">op stack</h2>
                <div class="flex flex-col gap-2">
                    {#each stackDiagrams as d (d.stack)}
                        <div class="rounded border border-neutral-200 bg-white">
                            <StackView
                                stack={d.stack}
                                ops={d.ops}
                                lastDurations={durations}
                                stackTotalMs={d.total}
                                onSelectOp={d.stack === currentStack
                                    ? (op) => (selectedName = op.name)
                                    : undefined}
                                selected={d.stack === currentStack ? selectedId : undefined}
                            />
                        </div>
                    {/each}
                </div>
                <p class="mt-1 text-[11px] text-neutral-400">
                    Click an op to edit it. Two parallel ops at scope 0 each EMIT a
                    field; the results merge.
                </p>
            </div>

            {#if selectedOp}
                <div>
                    <div class="mb-1 flex items-baseline gap-2">
                        <span class="font-mono text-sm font-semibold text-neutral-900">{selectedOp.name}</span>
                        <span class="text-[11px] text-neutral-400">scope {selectedOp.scope}</span>
                    </div>
                    {#key selectedName}
                        <CodeEditor value={selectedOp.txcl} onChange={updateSelected} />
                    {/key}
                    {#if selectedOp.js !== undefined}
                        <div class="mt-3">
                            <div class="mb-1 flex items-baseline gap-2">
                                <span class="font-mono text-xs font-semibold text-neutral-700">compute</span>
                                <span class="text-[11px] text-neutral-400">JavaScript — runs sandboxed via @txco/op</span>
                            </div>
                            {#key selectedName}
                                <textarea
                                    value={selectedOp.js}
                                    oninput={(e) => updateSelectedJs(e.currentTarget.value)}
                                    spellcheck="false"
                                    rows="10"
                                    class="w-full rounded border border-neutral-300 bg-white px-2 py-1.5 font-mono text-xs leading-snug text-neutral-700"
                                ></textarea>
                            {/key}
                        </div>
                    {/if}
                    <details class="mt-2 rounded border border-neutral-200 bg-white">
                        <summary class="cursor-pointer select-none px-3 py-1.5 text-xs text-neutral-600 hover:bg-neutral-50">
                            txcl reference
                        </summary>
                        <div class="max-h-[40vh] overflow-auto border-t border-neutral-200 px-3 py-2">
                            <TxclHelp />
                        </div>
                    </details>
                </div>
            {/if}
        </div>

        <!-- RIGHT: response + envelope + trace -->
    <div class="flex flex-col gap-4">
        <div>
            <h2 class="mb-1 text-sm font-semibold text-neutral-700">response</h2>
            {#if result}
                <div class="mb-2 flex items-center gap-2">
                    <span
                        class="rounded px-2 py-0.5 font-mono text-xs font-semibold {result.status >= 200 && result.status < 300
                            ? 'bg-emerald-100 text-emerald-800'
                            : result.status >= 400
                              ? 'bg-red-100 text-red-800'
                              : 'bg-neutral-100 text-neutral-700'}"
                    >
                        {result.status}
                    </span>
                </div>
                {#if liveUrl}
                    <div class="mb-0.5 text-[10px] uppercase tracking-wide text-neutral-400">
                        rendered <span class="normal-case text-neutral-300">— what a browser sees (Accept: text/html, …)</span>
                    </div>
                    <!--
                        sandbox="allow-scripts allow-same-origin": needed so the
                        chassis-served wait page (continuation-ui SPA) can fetch
                        its poll URL without hitting null-origin CORS failures —
                        without allow-same-origin the iframe's origin is opaque
                        and fetch() to a relative URL is treated as cross-origin
                        with no CORS headers → the page jumps to "reconnecting…".
                        Combining the two flags is only dangerous when the iframe
                        is same-origin with the embedder (it could remove the
                        sandbox); here the iframe lives on the web-inlet port and
                        the demo on the admin port, so origins differ.
                    -->
                    <iframe
                        src={liveUrl}
                        sandbox="allow-scripts allow-same-origin"
                        title="response preview"
                        class="mb-2 h-72 w-full rounded border border-neutral-200 bg-white"
                    ></iframe>
                {/if}
                <div class="mb-0.5 text-[10px] uppercase tracking-wide text-neutral-400">
                    raw <span class="normal-case text-neutral-300">— what a JSON client sees</span>
                </div>
                <pre class="max-h-48 overflow-auto rounded border border-neutral-200 bg-neutral-50 p-2 font-mono text-[11px] leading-snug whitespace-pre-wrap break-all text-neutral-700">{prettyBody || '(empty body)'}</pre>
            {:else}
                <p class="text-sm italic text-neutral-400">run to see a response</p>
            {/if}
        </div>

        <div class="min-h-0">
            <div class="mb-1 flex items-center justify-between gap-2">
                <h2 class="text-sm font-semibold text-neutral-700">
                    merged result
                    <span class="font-normal text-neutral-400">— final envelope after all ops merge</span>
                </h2>
                <label class="flex items-center gap-1.5 text-[11px] text-neutral-500">
                    show debug
                    <input type="checkbox" bind:checked={showDebug} />
                </label>
            </div>
            <div class="max-h-72 overflow-auto">
                <JsonPre value={displayEnvelope} emptyLabel="run to see the merged envelope" />
            </div>
        </div>

        <div>
            <div class="mb-1 flex items-center justify-between gap-2">
                <h2 class="text-sm font-semibold text-neutral-700">trace</h2>
                {#if trace?.steps && trace.steps.length > 0}
                    <label class="flex items-center gap-1.5 text-[11px] text-neutral-500">
                        show boot steps
                        <input type="checkbox" bind:checked={showBoot} />
                    </label>
                {/if}
            </div>
            {#if visibleSteps.length > 0}
                <ul class="flex flex-col gap-3">
                    {#each visibleSteps as step, i (i)}
                        <li class="rounded border border-neutral-200 p-2">
                            <div class="mb-1 flex items-center justify-between gap-2 font-mono text-xs">
                                <span class="text-neutral-700">
                                    {step.stack}/{step.scope}/{step.name || '(op)'}
                                </span>
                                {#if step.status}
                                    <span
                                        class="rounded px-1.5 py-0.5 text-[10px] {step.status === 'ok' || step.status === 'success'
                                            ? 'bg-emerald-100 text-emerald-800'
                                            : step.error
                                              ? 'bg-red-100 text-red-800'
                                              : 'bg-neutral-100 text-neutral-600'}"
                                    >
                                        {step.status}
                                    </span>
                                {/if}
                            </div>
                            {#if step.error}
                                <p class="mb-1 font-mono text-[11px] text-red-700">{step.error}</p>
                            {/if}
                            <div class="grid gap-2 md:grid-cols-2">
                                <div>
                                    <div class="mb-0.5 text-[10px] uppercase tracking-wide text-neutral-400">in</div>
                                    <div class="max-h-40 overflow-auto">
                                        <JsonPre value={step.in} emptyLabel="—" />
                                    </div>
                                </div>
                                <div>
                                    <div class="mb-0.5 text-[10px] uppercase tracking-wide text-neutral-400">out</div>
                                    <div class="max-h-40 overflow-auto">
                                        <JsonPre value={step.out} emptyLabel="—" />
                                    </div>
                                </div>
                            </div>
                        </li>
                    {/each}
                </ul>
            {:else if result && (trace?.steps?.length ?? 0) > 0}
                <p class="text-sm italic text-neutral-400">
                    only boot steps ran — your op didn't fire. Enable “show boot
                    steps” to see routing.
                </p>
            {:else if result}
                <p class="text-sm italic text-neutral-400">no trace steps recorded</p>
            {:else}
                <p class="text-sm italic text-neutral-400">run to see the trace</p>
            {/if}
        </div>
    </div>
    </div>
</div>
