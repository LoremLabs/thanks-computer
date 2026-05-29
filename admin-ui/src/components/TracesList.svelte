<script lang="ts">
    import { listTracesETag, type TraceSummary } from '../lib/api'
    import { store } from '../lib/store.svelte'
    import {
        startTraceStream,
        matchesGrep,
        type TraceStreamEvent,
    } from '../lib/traceStream'
    import Ago from './Ago.svelte'

    // Strip the trailing /<scope> from a stack identifier. Trace events
    // carry values like "boot/0" or "hello-world/0"; the selectedStack
    // in the global store is the bare name (e.g. "boot", "hello-world").
    function stripScope(s: string): string {
        return s ? s.replace(/\/\d+$/, '') : ''
    }

    interface Props {
        onSelectTrace: (rid: string) => void
    }
    let { onSelectTrace }: Props = $props()

    // Mode is internal — the UI does not expose a toggle. Default is
    // `live`; if the chassis has no Armable registered (e.g. file
    // backend), the live-stream effect catches a 404 and silently
    // falls back to `archive` so the user still sees recent history.
    // Archive mode polls /traces/requests.json on a fixed 5s cadence.
    type Mode = 'archive' | 'live'
    let mode = $state<Mode>('live')
    // Ring buffer cap for live mode. 5000 events at ~2 KB summary
    // each is ~10 MB worst case — fine for a tab. Older events drop
    // off the bottom.
    const LIVE_RING_MAX = 5000
    // Fixed archive poll cadence. 5s is the sweet spot — feels live
    // without burning CPU; the ETag 304 path keeps the cost low.
    const ARCHIVE_POLL_MS = 5000

    let traces = $state<TraceSummary[]>([])
    let liveEvents = $state<TraceStreamEvent[]>([])
    // Plain variable, NOT $state — the template never renders it, and
    // making it reactive caused a feedback loop: the polling effect
    // reads etag (sync, inside tick) and resets it sync, while tick's
    // async write fires the effect again, flooding the browser with
    // requests. Keep etag out of the reactive graph.
    let etag = ''
    let loading = $state(true)
    let error = $state('')

    // Refresh button bumps this counter; the live and archive effects
    // read it as a dep so a click forces a clean re-subscribe (live)
    // or a fresh fetch (archive). Cheaper than rebuilding state by hand.
    let refreshNonce = $state(0)

    // Free-text filter. Typed value debounces into `search`. In archive
    // mode this is sent to the server's ?grep= param (matched against
    // step names, operation strings, error text). In live mode it
    // filters the ring buffer client-side over the FULL event body —
    // payloads, headers, anything in the JSON.
    let query = $state('')
    let search = $state('')
    let debounceId: ReturnType<typeof setTimeout> | null = null
    $effect(() => {
        const q = query
        if (debounceId) clearTimeout(debounceId)
        debounceId = setTimeout(() => {
            search = q.trim()
        }, 250)
        return () => {
            if (debounceId) clearTimeout(debounceId)
        }
    })

    async function tick() {
        if (typeof document !== 'undefined' && document.hidden) return
        try {
            const r = await listTracesETag(50, search, etag)
            if (r.notModified) {
                error = ''
                return
            }
            if (r.response) {
                traces = r.response.traces ?? []
                etag = r.etag
                error = ''
            }
        } catch (e) {
            error = e instanceof Error ? e.message : String(e)
        } finally {
            loading = false
        }
    }

    $effect(() => {
        // Archive polling — only runs when we've fallen back to archive
        // mode. Re-fetch + restart the timer whenever mode/search/
        // refresh fire; the etag reset means switching filters won't
        // 304 against the previous query's fingerprint.
        const _m = mode
        if (_m !== 'archive') return
        const _s = search
        const _r = refreshNonce
        void _s
        void _r
        etag = ''
        tick()
        const id = setInterval(tick, ARCHIVE_POLL_MS)
        return () => clearInterval(id)
    })

    $effect(() => {
        // Live streaming — long-poll loop owned by startTraceStream.
        // We push events into the bounded ring and let the template
        // re-render. A 404 from the backend (no Armable) auto-switches
        // to archive so the user gets something useful instead of a
        // blank screen. The refresh nonce is a dep so a button click
        // re-subscribes from a clean ring.
        const _m = mode
        if (_m !== 'live') return
        const _r = refreshNonce
        void _r
        loading = true
        liveEvents = []
        const stop = startTraceStream({
            onEvent: (ev) => {
                // Prepend to keep newest first; bound at LIVE_RING_MAX.
                // Splice on the Svelte $state array re-triggers reactivity.
                liveEvents = [ev, ...liveEvents].slice(0, LIVE_RING_MAX)
                // Mirror into the global store keyed by rid so
                // TraceDetail can short-circuit the archive lookup
                // (which 404s for healthy traces under the default
                // on-error archive policy).
                store.cacheLiveTrace(ev)
                loading = false
                error = ''
            },
            onError: (e) => {
                error = e.message
            },
            onUnavailable: () => {
                error = 'live stream unavailable on this backend — falling back to archive'
                mode = 'archive'
            },
        })
        return () => stop()
    })

    function refresh() {
        refreshNonce += 1
    }

    // Project a live TraceStreamEvent into the same TraceSummary shape
    // the archive table renders — keeps one row template for both modes.
    function liveToSummary(ev: TraceStreamEvent): TraceSummary {
        return {
            rid: ev.rid,
            src: ev.src,
            stack: ev.stack,
            route: ev.route,
            status: ev.status,
            started_at: ev.started_at,
            duration_ms: ev.duration_ms,
        }
    }

    // The list the template actually renders. Live mode applies the
    // JS grep here; archive mode already filtered server-side.
    // Stack filter: when a stack is selected in the sidebar (which the
    // dropdown drives), narrow the trace list to events whose stack OR
    // post-goto route matches. Empty selectedStack = show everything.
    // We compare against the bare stack name (no scope) since trace
    // events carry values like "boot/0" or "hello-world/0".
    function matchesStack(ev: TraceStreamEvent, selected: string): boolean {
        if (!selected) return true
        if (stripScope(ev.stack || '') === selected) return true
        if (stripScope(ev.route || '') === selected) return true
        return false
    }
    function summaryMatchesStack(t: TraceSummary, selected: string): boolean {
        if (!selected) return true
        if (stripScope(t.stack || '') === selected) return true
        if (stripScope(t.route || '') === selected) return true
        return false
    }

    let visibleTraces = $derived.by<TraceSummary[]>(() => {
        const sel = store.state.selectedStack
        if (mode !== 'live') {
            if (!sel) return traces
            return traces.filter((t) => summaryMatchesStack(t, sel))
        }
        const needle = search
        const out: TraceSummary[] = []
        for (const ev of liveEvents) {
            if (!matchesStack(ev, sel)) continue
            if (matchesGrep(ev, needle)) out.push(liveToSummary(ev))
        }
        return out
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

    function formatDuration(ms?: number): string {
        if (typeof ms !== 'number' || !isFinite(ms) || ms < 0) return '—'
        if (ms < 1) return '<1 ms'
        if (ms < 10) return ms.toFixed(1) + ' ms'
        return Math.round(ms) + ' ms'
    }

    // Log-scale fill fraction so 1ms is still visible while 1s fills
    // the bar. Mirrors StackView.svelte's drawDurationBar exactly so
    // the two views read as the same chart.
    const BAR_FULL_SCALE_MS = 1000
    function barFrac(ms?: number): number {
        if (typeof ms !== 'number' || !isFinite(ms) || ms <= 0) return 0
        const raw = Math.log(ms + 1) / Math.log(BAR_FULL_SCALE_MS + 1)
        return Math.min(1, Math.max(0, raw))
    }

    function routeOf(t: TraceSummary): string {
        // Server emits `route` for traces that jumped to a downstream
        // stack; otherwise show the entry stack. The CLI TUI does the
        // same fallback (chassis/cli/trace_tui.go:359 area).
        return t.route || t.stack || ''
    }

    function shortRid(rid: string): string {
        return rid.length > 10 ? rid.slice(0, 4) + '…' + rid.slice(-6) : rid
    }
</script>

<div class="flex h-full flex-col p-4">
    <header class="mb-3 flex items-center justify-between gap-3">
        <div>
            <h2 class="font-mono text-base font-semibold text-neutral-900">traces</h2>
            <p class="text-xs text-neutral-500">
                {#if mode === 'live'}
                    live stream from the body-on-bus tail (no history).
                {:else}
                    archive of recent requests; click a row to inspect steps.
                {/if}
            </p>
        </div>
        <div class="flex items-center gap-3">
            <label class="flex items-center gap-1.5 text-xs text-neutral-500">
                search
                <input
                    type="search"
                    class="w-48 rounded border border-neutral-300 bg-white px-2 py-0.5 font-mono text-xs text-neutral-800 placeholder:text-neutral-400 focus:border-brand-cyan focus:outline-none"
                    bind:value={query}
                />
            </label>
            <button
                type="button"
                onclick={refresh}
                class="rounded border border-neutral-300 bg-white px-2 py-0.5 font-mono text-xs text-neutral-700 hover:bg-neutral-50 focus:border-brand-cyan focus:outline-none"
                title="reset and refetch"
            >
                refresh
            </button>
        </div>
    </header>

    {#if error}
        <p class="mb-2 rounded border border-red-300 bg-red-50 p-2 text-xs text-red-800">
            {error}
        </p>
    {/if}

    {#if loading && visibleTraces.length === 0}
        <p class="text-sm italic text-neutral-400">
            {#if mode === 'live'}waiting for the next request…{:else}loading…{/if}
        </p>
    {:else if visibleTraces.length === 0}
        <p class="rounded border border-neutral-200 bg-white p-4 text-sm italic text-neutral-400">
            {#if search && mode === 'live'}
                no live events match <span class="font-mono">{search}</span> in the
                last {liveEvents.length} (live ring, no history).
            {:else if search}
                no matches for <span class="font-mono">{search}</span> in the last 5000 traces.
            {:else if mode === 'live'}
                live tail attached — make a request to see one here.
            {:else}
                no traces yet — make a request to see one here.
            {/if}
        </p>
    {:else}
        <div class="overflow-auto rounded border border-neutral-200 bg-white">
            <table class="w-full text-sm">
                <thead class="border-b border-neutral-200 bg-neutral-50 text-left text-xs uppercase tracking-wide text-neutral-500">
                    <tr>
                        <th class="px-3 py-2 font-medium">time</th>
                        <th class="px-3 py-2 font-medium">route</th>
                        <th class="px-3 py-2 font-medium w-40">dur</th>
                        <th class="px-3 py-2 font-medium">src</th>
                        <th class="px-3 py-2 font-medium">rid</th>
                    </tr>
                </thead>
                <tbody class="divide-y divide-neutral-100">
                    {#each visibleTraces as t (t.rid)}
                        <tr
                            class="cursor-pointer hover:bg-neutral-50"
                            onclick={() => onSelectTrace(t.rid)}
                        >
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-400">
                                <Ago at={t.started_at} />
                            </td>
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-800">
                                {routeOf(t)}
                            </td>
                            <td class="px-3 py-2 align-top">
                                <div class="relative h-3.5 w-32 rounded bg-neutral-200">
                                    {#if barFrac(t.duration_ms) > 0}
                                        <div
                                            class="absolute inset-y-0 left-0 rounded bg-brand-cyan"
                                            style="width: {(barFrac(t.duration_ms) * 100).toFixed(2)}%"
                                        ></div>
                                    {/if}
                                    <span class="absolute inset-y-0 right-1.5 flex items-center font-mono text-[10px] text-neutral-700">
                                        {formatDuration(t.duration_ms)}
                                    </span>
                                </div>
                            </td>
                            <td class="px-3 py-2 align-top">
                                <span class="font-mono text-xs text-neutral-700">{t.src || '—'}</span>
                                {#if t.status && t.status !== 'ok'}
                                    <span class="ml-1.5 rounded px-1.5 py-0.5 text-[11px] font-medium {statusBadgeClass(t.status)}">
                                        {t.status}
                                    </span>
                                {/if}
                            </td>
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-500" title={t.rid}>
                                {#if t.rid.startsWith('resume-')}
                                    <span class="mr-1 rounded bg-brand-cyan/15 px-1 py-0.5 text-[10px] text-neutral-600" title="continuation resume">⮐ resume</span>
                                {/if}{shortRid(t.rid)}
                            </td>
                        </tr>
                    {/each}
                </tbody>
            </table>
        </div>
    {/if}
</div>
