// Same-origin fetch wrappers for the demo. When the SPA is
// served at /demo/ from the chassis itself, /v1/* and /traces/* are
// sibling paths. In dev (vite serve) the proxy in vite.config.ts
// forwards them to localhost:8081.
//
// Endpoint paths/shapes mirror admin-ui/src/lib/api.ts 1:1 (the
// versioned-opstack control plane in chassis/server/admin/stacks.go
// and the trace endpoints in chassis/server/admin/trace_request.go),
// trimmed to just the calls a Run needs: create draft → put files →
// activate → fire → fetch trace.
//
// credentials: 'omit' — the demo talks to the admin API
// UNAUTHENTICATED (the local `txco demo` chassis runs open). We must NOT
// send cookies: a stale `txco_session` cookie left on localhost by
// admin-ui or a prior session (cookies ignore port) would hit the auth
// middleware's cookie branch and 401 against this fresh chassis's DB,
// before it can fall through to open-dev. Omitting credentials skips
// that branch entirely.

// --- shared types (subset copied from admin-ui) -------------------------

// StackFile mirrors the server's stackFile JSON. For PUT we only ever
// send {path, content}; content_hash is server-assigned and read back
// on GET.
export interface StackFile {
    path: string
    content?: string
    content_hash?: string
}

export interface CreateDraftResponse {
    version_number: number
}

export interface ActivateResponse {
    version_number: number
    prior_version_number?: number
}

// Trace shapes — mirror chassis/server/admin/trace_request.go. Only
// the fields the demo reads are typed; the rest pass through.
export interface TraceStep {
    stack: string
    scope: number
    name: string
    operation?: string
    transport?: string
    status?: string
    duration_ms?: number
    started_at?: string
    finished_at?: string
    error?: string
    // Only present when the trace is fetched with ?include=full.
    // Payloads are arbitrary JSON; UI treats them as opaque.
    in?: unknown
    out?: unknown
}

export interface TraceResponse {
    rid: string
    src?: string
    stack?: string
    route?: string
    started_at?: string
    finished_at?: string
    duration_ms?: number
    status?: string
    steps?: TraceStep[]
    in?: unknown
    out?: unknown
}

// FireResult is the contract for the play fire-proxy (built in
// parallel server-side). The proxy fires `req` at the chassis web
// inlet and returns the response plus the request id so we can fetch
// the trace.
export interface FireResult {
    status: number
    headers: Record<string, string>
    body: string
    rid: string
}

export interface FireRequest {
    method: string
    path: string
    headers?: Record<string, string>
    body?: string
}

// --- low-level helpers --------------------------------------------------

async function getJSON<T>(path: string): Promise<T | null> {
    const resp = await fetch(path, { credentials: 'omit' })
    if (!resp.ok) {
        if (resp.status === 404) return null
        throw new Error(`${path}: ${resp.status} ${resp.statusText}`)
    }
    return resp.json() as Promise<T>
}

// --- control-plane calls (tenant "default", scratch stack "play") -------

// createDraft POSTs to /stacks/{stack}/draft. The endpoint
// auto-vivifies the stack if it doesn't exist yet. `from='empty'`
// yields a fresh empty draft (mapped to the server's empty-source
// form on the wire); `from='active'` clones the active version.
export async function createDraft(
    tenant: string,
    stack: string,
    from: 'empty' | 'active' = 'empty'
): Promise<CreateDraftResponse> {
    // Server treats "" / "active" as "clone active"; for a brand-new
    // scratch stack with no active version, "" produces an empty
    // draft, which is exactly what 'empty' means here.
    const wireFrom = from === 'empty' ? '' : 'active'
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(stack)}/draft`,
        {
            method: 'POST',
            credentials: 'omit',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ from: wireFrom }),
        }
    )
    if (!resp.ok) {
        throw new Error(`create draft ${stack}: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as CreateDraftResponse
}

// putDraftFiles replaces the draft's ENTIRE file set in one atomic PUT
// (the server clears stack_files for the version and re-inserts — see
// handlePutDraftFiles in chassis/server/admin/stacks.go). This is the
// right primitive for the demo: each Run sets the draft to
// exactly the editor's current files, with no per-file base_hash
// bookkeeping. (The single-file PATCH path is create/update with
// optimistic concurrency — using it here collided with
// `file_already_exists` once the cloned draft already held the file.)
export async function putDraftFiles(
    tenant: string,
    stack: string,
    versionNumber: number,
    files: StackFile[]
): Promise<void> {
    const url =
        `/v1/tenants/${encodeURIComponent(tenant)}` +
        `/stacks/${encodeURIComponent(stack)}` +
        `/versions/${versionNumber}/files`
    const resp = await fetch(url, {
        method: 'PUT',
        credentials: 'omit',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            files: files.map((f) => ({ path: f.path, content: f.content ?? '' })),
        }),
    })
    if (!resp.ok) {
        throw new Error(`put files: ${resp.status} ${resp.statusText}`)
    }
}

// bindHostname maps `hostname` → (tenant, stack) so a fired request
// routes to the scratch stack (detect-tenant resolves the Host header).
// Must run after the stack exists (post-activate). A 409 means the host
// is already bound — fine for our idempotent localhost→play mapping.
export async function bindHostname(
    tenant: string,
    hostname: string,
    stack: string
): Promise<void> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/hostnames`,
        {
            method: 'POST',
            credentials: 'omit',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ hostname, stack }),
        }
    )
    if (!resp.ok && resp.status !== 409) {
        throw new Error(
            `bind ${hostname}→${stack}: ${resp.status} ${resp.statusText}`
        )
    }
}

// activate flips the draft to the stack's active version.
export async function activate(
    tenant: string,
    stack: string,
    versionNumber: number
): Promise<ActivateResponse> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(stack)}/activate`,
        {
            method: 'POST',
            credentials: 'omit',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ version_number: versionNumber }),
        }
    )
    if (!resp.ok) {
        throw new Error(
            `activate ${stack} v${versionNumber}: ${resp.status} ${resp.statusText}`
        )
    }
    return (await resp.json()) as ActivateResponse
}

export interface ValidateError {
    path: string
    err: string
}
export interface ValidateResponse {
    ok: boolean
    errors?: ValidateError[]
    checked?: number
}

// validateVersion runs the same server-side txcl parse + ref/graph
// checks the CLI runs before activate. Catching invalid txcl here lets
// the demo surface parse errors immediately instead of activating
// a broken op and then hanging on a request the chassis can't process.
export async function validateVersion(
    tenant: string,
    stack: string,
    versionNumber: number
): Promise<ValidateResponse> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(stack)}/versions/${versionNumber}/validate`,
        { method: 'POST', credentials: 'omit' }
    )
    if (!resp.ok) {
        throw new Error(
            `validate ${stack} v${versionNumber}: ${resp.status} ${resp.statusText}`
        )
    }
    return (await resp.json()) as ValidateResponse
}

// fireRequest POSTs the sample request to the server-side play
// fire-proxy, which fires it at the chassis web inlet and returns the
// response plus the request id (rid) for trace lookup. This endpoint
// is being built in parallel; the client codes to its contract.
export async function fireRequest(req: FireRequest): Promise<FireResult> {
    // Safety net: bound the request so a runtime-stuck op surfaces an
    // error instead of leaving the UI on "running…" forever. (Validation
    // before activate catches the common parse-error case earlier.)
    const ctrl = new AbortController()
    const timer = setTimeout(() => ctrl.abort(), 20000)
    try {
        const resp = await fetch('/v1/demo/fire', {
            method: 'POST',
            credentials: 'omit',
            headers: { 'Content-Type': 'application/json' },
            signal: ctrl.signal,
            body: JSON.stringify({
                method: req.method,
                path: req.path,
                headers: req.headers ?? {},
                body: req.body ?? '',
            }),
        })
        if (!resp.ok) {
            throw new Error(`fire: ${resp.status} ${resp.statusText}`)
        }
        return (await resp.json()) as FireResult
    } catch (e) {
        if (e instanceof DOMException && e.name === 'AbortError') {
            throw new Error('fire: timed out — the op may be stuck or erroring at runtime')
        }
        throw e
    } finally {
        clearTimeout(timer)
    }
}

export interface DemoInfo {
    web_addr: string
    web_port: string
}

export interface BuildOpResult {
    ref: string // "compute://sha256/<digest>"
    digest: string
    engine: string
    bytes: number
}

// buildDemoOp ships a single compute-op source (JS/TS) to the demo
// build endpoint, which bundles + compiles + uploads it as a
// content-addressed wasm artifact and returns the `compute://sha256/…`
// ref to splice into the op's txcl in place of `op://<name>`. Same
// toolchain `txco apply` uses (esbuild + javy); identical source skips
// javy on repeat thanks to the server-side wasm cache. javy must be on
// PATH — surfaces as a structured "compile_unavailable" error.
export async function buildDemoOp(
    source: string,
    lang: 'js' | 'ts' | 'mjs' = 'js'
): Promise<BuildOpResult> {
    const resp = await fetch('/v1/demo/op/build', {
        method: 'POST',
        credentials: 'omit',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ source, lang }),
    })
    if (!resp.ok) {
        // The server returns `{error: "<code>", detail: {...map...}}` (see
        // chassis/server/admin/ops.go:98 writeJSONError → errorResponse).
        // Surface the code plus a human-readable message extracted from
        // the detail map — common keys are `detail` (compile errors from
        // CleanJSError / javy install hint) and `err` (Go error strings).
        // Anything else is JSON-stringified so we never fall back to
        // "[object Object]".
        let bodyText = ''
        try {
            const j = (await resp.json()) as {
                error?: string
                detail?: Record<string, unknown>
            }
            const code = j.error ?? ''
            let msg = ''
            const d = j.detail
            if (d && typeof d === 'object') {
                if (typeof d.detail === 'string') msg = d.detail
                else if (typeof d.err === 'string') msg = d.err
                else msg = JSON.stringify(d)
            }
            bodyText = [code, msg].filter(Boolean).join(': ')
        } catch {
            bodyText = `${resp.status} ${resp.statusText}`
        }
        throw new Error(`build compute: ${bodyText}`)
    }
    return (await resp.json()) as BuildOpResult
}

// getDemoInfo reports the chassis web-inlet port (the data plane), which
// differs from the admin port that serves this UI. The "copy as curl"
// command targets it so the copied request actually reaches the stack.
export async function getDemoInfo(): Promise<DemoInfo | null> {
    return getJSON<DemoInfo>('/v1/demo/info')
}

// getTrace fetches the per-request trace. `include=full` so per-step
// in/out envelope payloads come along — the demo renders them.
export async function getTrace(
    _tenant: string,
    rid: string
): Promise<TraceResponse | null> {
    return getJSON<TraceResponse>(
        `/traces/requests/${encodeURIComponent(rid)}.json?include=full`
    )
}
