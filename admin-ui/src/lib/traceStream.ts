// Client for GET /traces/stream — the admin's body-on-bus live tail.
//
// The server holds the request open up to ~30s and returns either a
// 200 with a batch of new closed-trace events + a next_cursor, or a
// 202 (Retry-After) when the budget expires with no traffic. We just
// reconnect in a tight loop on either outcome — cursors stay stable
// so reconnects don't miss events that landed mid-poll (within the
// server-side ring buffer, which is best-effort live tail, not
// durable replay).
//
// 404 means the chassis is running a trace backend with no Armable
// (file/noop) — the caller should fall back to the archive list.
//
// See internal docs/todo-trace-live-stream.md for the design rationale.

// dispatchSessionLost notifies the store that an authenticated fetch
// returned 401 (App.svelte listens for this event and routes to the
// login view). The function is module-private in api.ts; we inline the
// same one-liner here rather than widening that module's surface.
function dispatchSessionLost(): void {
    if (typeof window !== 'undefined') {
        window.dispatchEvent(new Event('txco:session-lost'))
    }
}

// Wire shape mirrors traceStreamEvent in
// chassis/server/admin/trace_stream.go. Only the fields the UI reads
// are typed; the rest pass through as `unknown` (the JS grep walks
// the full JSON, so we don't lose anything from the wire).
export interface TraceStreamEvent {
    rid: string
    src?: string
    tenant?: string
    stack?: string
    route?: string
    started_at?: string
    finished_at?: string
    duration_ms?: number
    status?: string
    payload_bytes?: number
    payload_truncated?: boolean
    steps?: unknown[]
    in?: unknown
    out?: unknown
    cursor: string
}

export interface TraceStreamResponse {
    events?: TraceStreamEvent[]
    next_cursor?: string
}

// Thrown when /traces/stream returns 404 — the chassis backend has no
// live-stream Armable registered (file/noop sink). UI falls back to
// archive polling on this.
export class TraceStreamUnavailableError extends Error {
    code = 'unavailable' as const
    constructor() {
        super('live trace stream not available on this backend')
    }
}

// fetchTraceStream issues one long-poll. Returns null on 202 (timeout
// with no events) so the caller can distinguish "loop again" from
// "received a batch."
export async function fetchTraceStream(
    tenant: string,
    cursor: string,
    waitMs: number,
    signal?: AbortSignal,
): Promise<TraceStreamResponse | null> {
    const qp = new URLSearchParams()
    if (cursor) qp.set('cursor', cursor)
    qp.set('wait', String(waitMs))
    // Tenant-scoped stream when a tenant is selected (server confines it to
    // that tenant's traces); flat /traces/stream (super-admin) otherwise.
    const base = tenant
        ? `/v1/tenants/${encodeURIComponent(tenant)}/traces/stream`
        : '/traces/stream'
    const resp = await fetch(`${base}?${qp.toString()}`, {
        credentials: 'same-origin',
        signal,
    })
    if (resp.status === 401) {
        dispatchSessionLost()
        throw new Error('trace stream: 401 unauthorized')
    }
    if (resp.status === 202) return null
    if (resp.status === 404) throw new TraceStreamUnavailableError()
    if (!resp.ok) {
        throw new Error(`trace stream: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as TraceStreamResponse
}

export interface TraceStreamOpts {
    // Tenant slug to scope the stream to (empty = flat super-admin stream).
    tenant?: string
    // Per-request long-poll budget. Server clamps to its own
    // TraceStreamLongPollMS, so values above ~30000 just hit the cap.
    waitMs?: number
    onEvent: (ev: TraceStreamEvent) => void
    onError?: (err: Error) => void
    onUnavailable?: () => void
}

// startTraceStream runs the long-poll loop until the returned stop
// function is called. Errors trigger exponential backoff (capped at
// 15s); the 404 unavailable case calls onUnavailable once and stops
// the loop so the UI can switch to archive mode.
export function startTraceStream(opts: TraceStreamOpts): () => void {
    const tenant = opts.tenant ?? ''
    const waitMs = opts.waitMs ?? 25000
    const ac = new AbortController()
    let cursor = ''
    let stopped = false
    let backoff = 500
    const backoffMax = 15000

    void (async () => {
        while (!stopped) {
            try {
                const r = await fetchTraceStream(tenant, cursor, waitMs, ac.signal)
                backoff = 500
                if (r) {
                    for (const ev of r.events ?? []) {
                        opts.onEvent(ev)
                    }
                    if (r.next_cursor) cursor = r.next_cursor
                }
                // 202 just means "no events in this window" — loop
                // immediately to reconnect (cursor unchanged).
            } catch (err) {
                if (ac.signal.aborted || stopped) return
                if (err instanceof TraceStreamUnavailableError) {
                    opts.onUnavailable?.()
                    return
                }
                opts.onError?.(err as Error)
                await sleep(backoff, ac.signal)
                backoff = Math.min(backoff * 2, backoffMax)
            }
        }
    })()

    return () => {
        stopped = true
        ac.abort()
    }
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
    return new Promise((resolve) => {
        const t = setTimeout(resolve, ms)
        signal.addEventListener('abort', () => {
            clearTimeout(t)
            resolve()
        }, { once: true })
    })
}

// matchesGrep is the in-memory filter for live mode. The server-side
// grep walks step names + operations + errors; here we have the full
// event body, so we just substring-search the JSON. Case-insensitive
// to mirror the server.
export function matchesGrep(ev: TraceStreamEvent, needle: string): boolean {
    if (!needle) return true
    const n = needle.toLowerCase()
    try {
        return JSON.stringify(ev).toLowerCase().includes(n)
    } catch {
        return false
    }
}
