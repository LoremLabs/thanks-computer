<script lang="ts">
    import { onMount } from 'svelte'

    type State = 'working' | 'reconnecting' | 'failed'

    let phase: State = $state('working')
    let failMsg: string = $state('')

    const params = new URLSearchParams(location.search)
    const rcid = params.get('_txc.continuation') ?? ''
    const pollURL =
        location.pathname + '?_txc.continuation=' + encodeURIComponent(rcid) + '&format=json'
    const resultURL = location.pathname + '?_txc.continuation=' + encodeURIComponent(rcid)

    const BASE_MS = 3000
    let backoff = 0
    let stopped = false
    let paused = false

    // Spread retries so many tabs waiting on the same run don't hit the
    // server in lockstep.
    const jitter = (ms: number) => ms + Math.random() * ms * 0.3

    async function tick() {
        if (stopped) return
        // The server now long-polls (holds the request open until the run
        // changes). A backgrounded tab shouldn't keep a connection open
        // for nothing — pause and pick back up on focus.
        if (document.hidden) {
            paused = true
            return
        }
        const started = Date.now()
        try {
            const res = await fetch(pollURL, {
                cache: 'no-store',
                headers: { Accept: 'application/json' },
            })
            const data = await res.json().catch(() => ({}) as Record<string, unknown>)
            const status = String((data as Record<string, unknown>).status ?? '')

            if (status === 'completed') {
                stopped = true
                // Navigate to the same handle WITHOUT format=json: the
                // server returns the rendered result page (a real page
                // load, with its own headers/content-type).
                location.assign(resultURL)
                return
            }
            if (status === 'failed') {
                stopped = true
                phase = 'failed'
                return
            }
            // waiting / resumable (or an unexpected transient body).
            // Self-tuning cadence: if the server held the request open
            // (adaptive long-poll), `elapsed` already covered the wait —
            // re-poll right away. If it answered instantly (long-poll
            // disabled server-side), this preserves the ~BASE_MS grid.
            backoff = 0
            phase = 'working'
            schedule(jitter(Math.max(0, BASE_MS - (Date.now() - started))))
        } catch {
            // network blip / chassis restart: surface, back off, retry.
            phase = 'reconnecting'
            backoff = Math.min(backoff + 1, 4)
            schedule(jitter(BASE_MS * (1 + backoff)))
        }
    }

    function schedule(ms: number) {
        if (stopped) return
        if (document.hidden) {
            paused = true
            return
        }
        setTimeout(tick, ms)
    }

    function onVisible() {
        if (stopped || !paused || document.hidden) return
        paused = false
        schedule(0)
    }

    onMount(() => {
        if (!rcid) {
            phase = 'failed'
            failMsg = 'missing continuation id'
            return
        }
        document.addEventListener('visibilitychange', onVisible)
        schedule(0)
        return () => {
            stopped = true
            document.removeEventListener('visibilitychange', onVisible)
        }
    })
</script>

<main
    class="flex min-h-full items-center justify-center bg-neutral-50 px-4 text-neutral-900"
>
    <div
        class="w-full max-w-md rounded-lg border border-neutral-200 bg-white p-10 text-center shadow-sm"
    >
        <div class="text-2xl font-semibold tracking-tight">
            thanks, c<span class="o o1">o</span><span class="o o2">o</span><span
                class="o o3">o</span
            >mputer.
        </div>

        {#if phase === 'failed'}
            <p class="mt-6 text-sm text-brand-red">
                {failMsg || 'This run failed.'}
            </p>
        {:else}
            <p class="mt-6 text-sm text-neutral-600">
                {phase === 'reconnecting' ? 'reconnecting…' : 'working…'}
            </p>
            <p class="mt-2 text-xs text-neutral-400">
                This page updates automatically.
            </p>
        {/if}
    </div>
</main>
