import type { ListOpsResponse, Op } from './types'

// Same-origin fetch: when the SPA is served at /admin/ from the chassis
// itself, /v1/* is a sibling path. In dev (vite serve) the proxy in
// vite.config.ts forwards /v1/* to localhost:8081.
async function getJSON<T>(path: string): Promise<T | null> {
    const resp = await fetch(path, { credentials: 'same-origin' })
    if (resp.status === 401) {
        dispatchSessionLost()
        throw new Error(`${path}: 401 unauthorized`)
    }
    if (!resp.ok) {
        if (resp.status === 404) return null
        throw new Error(`${path}: ${resp.status} ${resp.statusText}`)
    }
    return resp.json() as Promise<T>
}

// dispatchSessionLost notifies the store that an authenticated fetch
// returned 401. The store catches the event in App.svelte and routes
// to the login view without losing the current hash. Defined once so
// every mutating call site can call it the same way.
function dispatchSessionLost() {
    if (typeof window !== 'undefined') {
        window.dispatchEvent(new Event('txco:session-lost'))
    }
}

// check401 is a tiny helper for the mutation endpoints: dispatches
// the session-lost event when the response is 401 so callers don't
// have to repeat the boilerplate.
function check401(resp: Response): boolean {
    if (resp.status === 401) {
        dispatchSessionLost()
        return true
    }
    return false
}

// Tenant directory shape returned by GET /v1/tenants. Slug is the
// stable URL handle (also what the user types into a picker);
// tenant_id is the internal opaque identifier and isn't needed for
// most UI surfaces, but we carry it for future server calls.
export interface Tenant {
    tenant_id: string
    slug: string
    name?: string
    created_at?: string
}

export interface ListTenantsResponse {
    tenants?: Tenant[]
}

export async function listTenants(): Promise<Tenant[]> {
    const body = await getJSON<ListTenantsResponse>('/v1/tenants')
    return body?.tenants ?? []
}

// Ops are now tenant-scoped at the URL level. Flat /v1/ops was retired
// in the phase-3 migration (the server returns 410 with a hint).
//
// Retained for backward-compatibility while we migrate the UI to the
// versioned-opstack endpoints; once the migration is complete this
// can be deleted along with its single caller.
export async function listOps(tenant: string, stack?: string): Promise<Op[]> {
    if (!tenant) return []
    const qs = stack ? `?stack=${encodeURIComponent(stack)}` : ''
    const body = await getJSON<ListOpsResponse>(
        `/v1/tenants/${encodeURIComponent(tenant)}/ops${qs}`
    )
    return body?.ops ?? []
}

// Versioned opstack control plane. Server-side reference:
// chassis/server/admin/stacks.go (stackRecord / versionRecord /
// versionDetail / stackFile) and route table in server.go:169-179.
// Field names match the server's JSON tags 1:1.

export interface Stack {
    name: string
    // Active version number. Absent when no version has ever been
    // activated (a brand-new stack whose first draft is unactivated).
    active_version?: number
    created_at: string
}

export interface Version {
    version_number: number
    status: 'draft' | 'superseded' | 'revoked'
    parent_version_number?: number
    created_by: string
    created_at: string
    // Present when the version has been (or is) activated. The
    // active pointer lives on the parent stack — this field is just
    // historical metadata for the row.
    activated_at?: string
    manifest_hash: string
    // True iff this version is the stack's current active_version.
    // Recompute carefully on cache invalidation after an activation.
    is_active: boolean
}

export interface StackFile {
    path: string
    // Omitted on GET /versions list calls (server only emits content
    // on the full version-detail fetch). Always present on
    // GET /versions/{n}.
    content?: string
    content_hash: string
}

export interface VersionDetail extends Version {
    files: StackFile[]
}

export interface ListStacksResponse {
    stacks?: Stack[]
}

export interface ListVersionsResponse {
    versions?: Version[]
}

export async function listStacks(tenant: string): Promise<Stack[]> {
    if (!tenant) return []
    const body = await getJSON<ListStacksResponse>(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks`
    )
    return body?.stacks ?? []
}

export async function getStack(
    tenant: string,
    name: string
): Promise<Stack | null> {
    if (!tenant || !name) return null
    return getJSON<Stack>(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(name)}`
    )
}

export async function listVersions(
    tenant: string,
    name: string
): Promise<Version[]> {
    if (!tenant || !name) return []
    const body = await getJSON<ListVersionsResponse>(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(name)}/versions`
    )
    return body?.versions ?? []
}

export async function getVersion(
    tenant: string,
    name: string,
    versionNumber: number
): Promise<VersionDetail | null> {
    if (!tenant || !name || !versionNumber) return null
    // include=content opt-in: the server omits file bodies otherwise.
    // The version_adapter needs the txcl + mock JSON to render ops.
    return getJSON<VersionDetail>(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(name)}/versions/${versionNumber}?include=content`
    )
}

// GET /stacks/{name}/diff?v1=N&v2=M — per-file change list between two
// versions (added / changed / removed). Returns only hashes; file
// content is fetched separately via getVersion when the UI wants to
// render a line-level diff.
export interface DiffEntry {
    path: string
    change: 'added' | 'changed' | 'removed'
    from_hash?: string
    to_hash?: string
}
export interface DiffResponse {
    v1: number
    v2: number
    equal: boolean
    entries?: DiffEntry[]
}
export async function diffVersions(
    tenant: string,
    name: string,
    v1: number,
    v2: number
): Promise<DiffResponse | null> {
    if (!tenant || !name || !v1 || !v2) return null
    return getJSON<DiffResponse>(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(name)}/diff?v1=${v1}&v2=${v2}`
    )
}

// Mutating endpoints. Authentication is handled at the cookie layer:
// in signed-mode chassis the user logs in via /auth/browser/login,
// which sets a session cookie; in open-dev mode requests pass
// unsigned. Either way a 401 from these endpoints triggers a
// `txco:session-lost` event so the store can route to the login
// view without bouncing the user from an editor mid-keystroke.

export interface ValidateError {
    path: string
    err: string
}

export interface ValidateResponse {
    ok: boolean
    errors?: ValidateError[]
    checked: number
}

// POST /stacks/{name}/versions/{n}/validate — runs txcl parse +
// ref/graph checks server-side. Returns ok:true when the draft is
// safe to activate; ok:false with per-file errors otherwise.
export async function validateVersion(
    tenant: string,
    name: string,
    versionNumber: number
): Promise<ValidateResponse> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(name)}/versions/${versionNumber}/validate`,
        {
            method: 'POST',
            credentials: 'same-origin',
        }
    )
    if (check401(resp)) {
        throw new Error(`validate ${name} v${versionNumber}: 401 unauthorized`)
    }
    if (!resp.ok) {
        throw new Error(
            `validate ${name} v${versionNumber}: ${resp.status} ${resp.statusText}`
        )
    }
    return (await resp.json()) as ValidateResponse
}

export class ValidationFailedError extends Error {
    errors: ValidateError[]
    constructor(stack: string, versionNumber: number, errors: ValidateError[]) {
        const summary = errors
            .map((e) => `${e.path}: ${e.err}`)
            .slice(0, 3)
            .join('; ')
        super(
            `validation failed for ${stack} v${versionNumber} (${errors.length} error${errors.length === 1 ? '' : 's'}): ${summary}`
        )
        this.name = 'ValidationFailedError'
        this.errors = errors
    }
}

export interface ActivateResponse {
    version_number: number
    prior_version_number?: number
}

export async function activateStack(
    tenant: string,
    name: string,
    versionNumber: number
): Promise<ActivateResponse> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(name)}/activate`,
        {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ version_number: versionNumber }),
        }
    )
    if (check401(resp)) {
        throw new Error(`activate ${name} v${versionNumber}: 401 unauthorized`)
    }
    if (!resp.ok) {
        throw new Error(`activate ${name} v${versionNumber}: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as ActivateResponse
}

export interface CreateDraftResponse {
    version_number: number
}

// PATCH / DELETE on a draft's individual files. Both require a
// base_hash for optimistic concurrency — server side at
// chassis/server/admin/stacks.go:121-142 (patchFileRequest,
// deleteFileRequest). 409 base_hash_mismatch and 409
// version_not_draft are typed below so the store / UI can branch.

export interface PatchFileResponse {
    path: string
    content_hash: string
    manifest_hash: string
}

export interface DeleteFileResponse {
    path: string
    deleted: boolean
    manifest_hash: string
}

export class BaseHashMismatchError extends Error {
    code = 'base_hash_mismatch'
    detail?: Record<string, unknown>
    constructor(message: string, detail?: Record<string, unknown>) {
        super(message)
        this.name = 'BaseHashMismatchError'
        this.detail = detail
    }
}

export class VersionNotDraftError extends Error {
    code = 'version_not_draft'
    detail?: Record<string, unknown>
    constructor(message: string, detail?: Record<string, unknown>) {
        super(message)
        this.name = 'VersionNotDraftError'
        this.detail = detail
    }
}

// 409 dispatcher: read the server's error code and throw the typed
// shape so call sites don't have to inspect string codes.
async function throw409(label: string, resp: Response): Promise<never> {
    let body: { error?: string; detail?: Record<string, unknown> } = {}
    try {
        body = (await resp.json()) as typeof body
    } catch {
        // ignore — server returned non-JSON for some reason
    }
    if (body.error === 'base_hash_mismatch') {
        throw new BaseHashMismatchError(`${label}: ${body.error}`, body.detail)
    }
    if (body.error === 'version_not_draft') {
        throw new VersionNotDraftError(`${label}: ${body.error}`, body.detail)
    }
    throw new Error(`${label}: ${resp.status} ${body.error ?? resp.statusText}`)
}

export async function patchFile(
    tenant: string,
    stack: string,
    versionNumber: number,
    path: string,
    content: string,
    baseHash: string
): Promise<PatchFileResponse> {
    const url =
        `/v1/tenants/${encodeURIComponent(tenant)}` +
        `/stacks/${encodeURIComponent(stack)}` +
        `/versions/${versionNumber}/files`
    const resp = await fetch(url, {
        method: 'PATCH',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path, content, base_hash: baseHash }),
    })
    if (check401(resp)) {
        throw new Error(`patch ${stack} ${path}: 401 unauthorized`)
    }
    if (resp.status === 409) await throw409(`patch ${stack} ${path}`, resp)
    if (!resp.ok) {
        throw new Error(`patch ${stack} ${path}: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as PatchFileResponse
}

export async function deleteFile(
    tenant: string,
    stack: string,
    versionNumber: number,
    path: string,
    baseHash: string
): Promise<DeleteFileResponse> {
    const url =
        `/v1/tenants/${encodeURIComponent(tenant)}` +
        `/stacks/${encodeURIComponent(stack)}` +
        `/versions/${versionNumber}/files`
    const resp = await fetch(url, {
        method: 'DELETE',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ path, base_hash: baseHash }),
    })
    if (check401(resp)) {
        throw new Error(`delete ${stack} ${path}: 401 unauthorized`)
    }
    if (resp.status === 409) await throw409(`delete ${stack} ${path}`, resp)
    if (!resp.ok) {
        throw new Error(`delete ${stack} ${path}: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as DeleteFileResponse
}

// `from`: "active" clones the currently-active version's files into
// the new draft. Empty string / omitted creates an empty draft.
export async function createDraft(
    tenant: string,
    name: string,
    from: 'active' | '' = 'active'
): Promise<CreateDraftResponse> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/stacks/${encodeURIComponent(name)}/draft`,
        {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ from }),
        }
    )
    if (check401(resp)) {
        throw new Error(`create draft ${name}: 401 unauthorized`)
    }
    if (!resp.ok) {
        throw new Error(`create draft ${name}: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as CreateDraftResponse
}

// Trace shapes — mirror chassis/server/admin/trace_request.go. Only the
// fields the UI actually reads are typed; the rest pass through.
export interface TraceSummary {
    rid: string
    src?: string
    stack?: string
    route?: string
    status?: string
    started_at?: string
    duration_ms?: number
}

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
    input_bytes?: number
    output_bytes?: number
    input_truncated?: boolean
    output_truncated?: boolean
    error?: string
    // Only present when the trace is fetched with ?include=full.
    // Payloads are arbitrary JSON; UI treats them as opaque.
    in?: unknown
    out?: unknown
}

export interface TraceListResponse {
    traces?: TraceSummary[]
    total?: number
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
    // Present only when this trace is part of a suspended/resumed
    // continuation run — cross-links to the originating request trace
    // and each per-stage resume trace.
    continuation?: TraceContinuation
}

export interface TraceContinuation {
    self: 'origin' | 'resume'
    run_continuation_id?: string
    origin_rid?: string
    resumes?: { rid: string; stage: string }[]
}

export async function listTraces(limit = 20): Promise<TraceSummary[]> {
    const body = await getJSON<TraceListResponse>(
        `/traces/requests.json?limit=${limit}`
    )
    return body?.traces ?? []
}

// ETag-aware variant used by the Traces stream page. The chassis hashes
// stat data for the trace directory and returns 304 when nothing's
// changed, so polling stays cheap even at 2s intervals. Callers cache
// the returned etag and pass it back as ifNoneMatch on the next tick.
export interface ListTracesResult {
    response: TraceListResponse | null // null on 304 Not Modified
    etag: string
    notModified: boolean
}

export async function listTracesETag(
    limit = 50,
    grep = '',
    ifNoneMatch = ''
): Promise<ListTracesResult> {
    const qp = new URLSearchParams()
    qp.set('limit', String(limit))
    if (grep) qp.set('grep', grep)
    const headers: Record<string, string> = {}
    if (ifNoneMatch) headers['If-None-Match'] = ifNoneMatch
    const resp = await fetch(`/traces/requests.json?${qp.toString()}`, {
        credentials: 'same-origin',
        headers,
    })
    if (resp.status === 401) {
        dispatchSessionLost()
        throw new Error('listTracesETag: 401 unauthorized')
    }
    if (resp.status === 304) {
        return { response: null, etag: ifNoneMatch, notModified: true }
    }
    if (resp.status === 404) {
        // No trace dir yet (fresh chassis, no requests served). This is
        // the empty state, not an error — return an empty list so the
        // caller renders "no traces yet" without a red error box.
        return {
            response: { traces: [], total: 0 },
            etag: '',
            notModified: false,
        }
    }
    if (!resp.ok) {
        throw new Error(`listTracesETag: ${resp.status} ${resp.statusText}`)
    }
    const body = (await resp.json()) as TraceListResponse
    return {
        response: body,
        etag: resp.headers.get('ETag') ?? '',
        notModified: false,
    }
}

// `include=full` so per-step `in` / `out` payloads come along — we
// surface them in the "Last req" / "Last res" tabs and need them
// cached client-side after one fetch.
export async function getTrace(rid: string): Promise<TraceResponse | null> {
    return getJSON<TraceResponse>(
        `/traces/requests/${encodeURIComponent(rid)}.json?include=full`
    )
}

// --- browser-auth session endpoints (Phase 2b consumer) -----------------

// The chassis-side counterparts live in
// chassis/server/admin/browserauth.go. See internal docs/todo-admin-ui-browser-auth.md
// for the full design rationale.

// SessionInfo mirrors the chassis's sessionInfoResponse. `open_dev` is
// the carve-out for chassis in `--auth-mode=both` with no admin user
// configured: the UI treats that as authed and skips the login flow.
export interface SessionInfo {
    source: 'browser' | 'open' | 'signed' | 'basic'
    open_dev?: boolean
    session_id?: string
    actor_id?: string
    tenant_id?: string
    capabilities?: string[]
    expires_at?: string
}

// BootstrapInvalidError signals that the token the browser just
// posted to /auth/browser/exchange is no longer redeemable — either
// it was already consumed, has expired, or was never minted. Caller
// surfaces a friendlier "run `txco auth login` again" message.
export class BootstrapInvalidError extends Error {
    code = 'bootstrap_invalid'
    constructor(message: string) {
        super(message)
        this.name = 'BootstrapInvalidError'
    }
}

// getSession is the very first call the UI makes. Returns null when
// the chassis says we're not authed (401); the store uses that to
// render the login view.
export async function getSession(): Promise<SessionInfo | null> {
    const resp = await fetch('/auth/browser/session', {
        credentials: 'same-origin',
    })
    if (resp.status === 401) return null
    if (!resp.ok) {
        throw new Error(`getSession: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as SessionInfo
}

// exchangeToken trades a single-use bootstrap token (minted by
// `txco auth login`) for a session cookie. The cookie is HttpOnly so
// JS can't read it directly — but it's now on every subsequent
// same-origin fetch. The response body is informational; the cookie
// is the actual auth credential.
export async function exchangeToken(token: string): Promise<SessionInfo> {
    const resp = await fetch('/auth/browser/exchange', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token }),
    })
    if (resp.status === 404) {
        throw new BootstrapInvalidError(
            'this login link has already been used or has expired — run `txco auth login` for a fresh one'
        )
    }
    if (resp.status === 400) {
        throw new BootstrapInvalidError(
            'this login link is malformed — run `txco auth login` for a fresh one'
        )
    }
    if (!resp.ok) {
        throw new Error(`exchange: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as SessionInfo
}

// deleteSession revokes the current session ("sign out"). Idempotent
// at the server.
export async function deleteSession(): Promise<void> {
    const resp = await fetch('/auth/browser/session', {
        method: 'DELETE',
        credentials: 'same-origin',
    })
    if (!resp.ok && resp.status !== 401) {
        throw new Error(`signOut: ${resp.status} ${resp.statusText}`)
    }
}

// --- secrets ------------------------------------------------------------
//
// Server-side reference: chassis/server/admin/secret_endpoints.go
// (secretRecord / listSecretsResponse) and the route table in
// server.go:266-273. The read path lands here; write functions
// (create / rotate / revoke / patch-description) arrive in Sprint 2.

// SecretMetadata mirrors the chassis's secretRecord. Metadata only —
// there is NO value field on this shape by design. Cleartext is only
// ever returned by the generate / rotate-generated endpoints (v1.5),
// in a separate value-bearing shape.
export interface SecretMetadata {
    secret_id: string
    tenant_id: string
    // "" / absent = tenant-wide; otherwise the stack this secret is
    // scoped to. v1 only ever creates tenant-wide; stack scoping is a
    // v2 surface.
    stack?: string
    name: string
    description?: string
    created_at: string
    created_by?: string
    last_rotated_at?: string
    key_version: number
    version_no: number
}

interface ListSecretsResponse {
    secrets?: SecretMetadata[]
}

// SecretStoreUnavailableError is thrown when the chassis returns 503
// secret_store_unavailable — the operator opted out of the feature by
// leaving SecretMasterKeyPath empty. The UI renders a "not configured"
// state instead of a generic error.
export class SecretStoreUnavailableError extends Error {
    code = 'secret_store_unavailable'
    constructor(message: string) {
        super(message)
        this.name = 'SecretStoreUnavailableError'
    }
}

// The next three mirror the server's translateStoreErr sentinels. They
// are thrown by the write endpoints wired in Sprint 2; defined here so
// the typed error surface lands alongside the read path.
export class SecretNotFoundError extends Error {
    code = 'secret_not_found'
    constructor(message: string) {
        super(message)
        this.name = 'SecretNotFoundError'
    }
}

export class SecretExistsError extends Error {
    code = 'secret_exists'
    constructor(message: string) {
        super(message)
        this.name = 'SecretExistsError'
    }
}

export class SecretNameImmutableError extends Error {
    code = 'name_immutable'
    constructor(message: string) {
        super(message)
        this.name = 'SecretNameImmutableError'
    }
}

// readErrorCode pulls the server's `error` code out of a JSON error
// body, returning '' when the body isn't JSON or carries no code.
// Used to branch typed errors off the wire codes.
async function readErrorCode(resp: Response): Promise<string> {
    try {
        const body = (await resp.json()) as { error?: string }
        return body.error ?? ''
    } catch {
        return ''
    }
}

// listSecrets returns the active secrets (tenant-wide + stack-scoped)
// for the tenant. Metadata only. Throws SecretStoreUnavailableError
// when the feature is opted out so the UI can render a distinct state.
export async function listSecrets(tenant: string): Promise<SecretMetadata[]> {
    if (!tenant) return []
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/secrets`,
        { credentials: 'same-origin' }
    )
    if (resp.status === 401) {
        dispatchSessionLost()
        throw new Error('listSecrets: 401 unauthorized')
    }
    if (resp.status === 503) {
        const code = await readErrorCode(resp)
        if (code === 'secret_store_unavailable') {
            throw new SecretStoreUnavailableError(
                'the secret store is not configured on this chassis'
            )
        }
        throw new Error(`listSecrets: 503 ${code || resp.statusText}`)
    }
    if (!resp.ok) {
        throw new Error(`listSecrets: ${resp.status} ${resp.statusText}`)
    }
    const body = (await resp.json()) as ListSecretsResponse
    return body.secrets ?? []
}

interface SecretResponse {
    secret: SecretMetadata
}

// throwSecretError maps the server's translateStoreErr wire codes to
// typed errors so call sites can branch (the modal surfaces
// secret_exists distinctly; rotate/revoke surface not-found). Consumes
// the response body — only call on a non-ok response.
async function throwSecretError(label: string, resp: Response): Promise<never> {
    const code = await readErrorCode(resp)
    switch (code) {
        case 'secret_not_found':
            throw new SecretNotFoundError(`${label}: secret not found`)
        case 'secret_exists':
            throw new SecretExistsError(`${label}: a secret with that name already exists`)
        case 'name_immutable':
            throw new SecretNameImmutableError(
                `${label}: secret names are immutable (rename = create-new + revoke-old)`
            )
        case 'invalid_name':
            throw new Error(`${label}: invalid name — must match [A-Z][A-Z0-9_]*`)
        case 'empty_value':
            throw new Error(`${label}: value cannot be empty`)
        case 'missing_name':
            throw new Error(`${label}: name is required`)
        default:
            throw new Error(`${label}: ${resp.status} ${code || resp.statusText}`)
    }
}

// createSecret stores an operator-supplied value (tenant-wide). The
// value is sent once in the request body; the response is metadata
// only — the server never echoes it back. v2 adds the stack-scope
// parameter.
export async function createSecret(
    tenant: string,
    name: string,
    value: string,
    description: string
): Promise<SecretMetadata> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/secrets`,
        {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ name, value, description }),
        }
    )
    if (check401(resp)) {
        throw new Error(`create secret ${name}: 401 unauthorized`)
    }
    if (!resp.ok) await throwSecretError(`create secret ${name}`, resp)
    const body = (await resp.json()) as SecretResponse
    return body.secret
}

// rotateSecret writes a new version with an operator-supplied value.
// Returns metadata only.
export async function rotateSecret(
    tenant: string,
    name: string,
    value: string
): Promise<SecretMetadata> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/secrets/${encodeURIComponent(name)}/rotate`,
        {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ value }),
        }
    )
    if (check401(resp)) {
        throw new Error(`rotate secret ${name}: 401 unauthorized`)
    }
    if (!resp.ok) await throwSecretError(`rotate secret ${name}`, resp)
    const body = (await resp.json()) as SecretResponse
    return body.secret
}

// updateSecretDescription patches the description. The body carries
// ONLY description — sending `name` is rejected server-side
// (name_immutable), so we never include it.
export async function updateSecretDescription(
    tenant: string,
    name: string,
    description: string
): Promise<SecretMetadata> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/secrets/${encodeURIComponent(name)}`,
        {
            method: 'PATCH',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ description }),
        }
    )
    if (check401(resp)) {
        throw new Error(`update secret ${name}: 401 unauthorized`)
    }
    if (!resp.ok) await throwSecretError(`update secret ${name}`, resp)
    const body = (await resp.json()) as SecretResponse
    return body.secret
}

// revokeSecret soft-deletes the active row. Server returns 204.
export async function revokeSecret(tenant: string, name: string): Promise<void> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/secrets/${encodeURIComponent(name)}`,
        {
            method: 'DELETE',
            credentials: 'same-origin',
        }
    )
    if (check401(resp)) {
        throw new Error(`revoke secret ${name}: 401 unauthorized`)
    }
    if (resp.status === 204) return
    if (!resp.ok) await throwSecretError(`revoke secret ${name}`, resp)
}

// ===========================================================================
// Demo-route endpoints (#demo). Migrated from demo-ui/src/lib/api.ts —
// preserved verbatim shape so the Runner ports without code changes.
// All five only have any effect when the chassis runs under `txco demo`
// (which exposes /v1/demo/* and sets up open-dev auth + hostname binds).
// ===========================================================================

// bindHostname maps `hostname` → (tenant, stack) so a fired request routes
// to the stack via the chassis's ingress Host-header lookup. Idempotent:
// 409 (hostname already bound) is treated as success — the demo seeds
// every step's hostname at mount and re-runs the same bind on every
// Run({apply:true}).
export async function bindHostname(
    tenant: string,
    hostname: string,
    stack: string
): Promise<void> {
    const resp = await fetch(
        `/v1/tenants/${encodeURIComponent(tenant)}/hostnames`,
        {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ hostname, stack }),
        }
    )
    if (check401(resp)) {
        throw new Error(`bind ${hostname}→${stack}: 401 unauthorized`)
    }
    if (!resp.ok && resp.status !== 409) {
        throw new Error(
            `bind ${hostname}→${stack}: ${resp.status} ${resp.statusText}`
        )
    }
}

// putDraftFiles replaces the draft's ENTIRE file set in one atomic PUT
// (the server clears stack_files for the version and re-inserts — see
// handlePutDraftFiles in chassis/server/admin/stacks.go). Right primitive
// for the demo: each Run sets the draft to exactly the editor's current
// files, with no per-file base_hash bookkeeping. (Admin's regular ops
// view uses patchFile per file, which collides with file_already_exists
// once a cloned draft already holds the file.)
// Accepts a permissive `{path, content}` shape rather than the full
// StackFile (which requires `content_hash` for GET responses). The wire
// PUT only sends path + content; the server assigns the hash.
export async function putDraftFiles(
    tenant: string,
    stack: string,
    versionNumber: number,
    files: { path: string; content?: string }[]
): Promise<void> {
    const url =
        `/v1/tenants/${encodeURIComponent(tenant)}` +
        `/stacks/${encodeURIComponent(stack)}` +
        `/versions/${versionNumber}/files`
    const resp = await fetch(url, {
        method: 'PUT',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
            files: files.map((f) => ({ path: f.path, content: f.content ?? '' })),
        }),
    })
    if (check401(resp)) {
        throw new Error(`put files: 401 unauthorized`)
    }
    if (!resp.ok) {
        throw new Error(`put files: ${resp.status} ${resp.statusText}`)
    }
}

// FireResult is the contract for the /v1/demo/fire proxy. The proxy fires
// `req` at the chassis web inlet and returns the response plus the
// request id so the UI can fetch the trace.
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

// fireRequest POSTs the sample request to /v1/demo/fire, which proxies
// it to the chassis web inlet and returns the response + request id (rid)
// for trace lookup. Demo-only endpoint; harmless 404 in non-demo
// chassis (the demo route also won't be reachable from the nav).
export async function fireRequest(req: FireRequest): Promise<FireResult> {
    // Safety net: bound the request so a runtime-stuck op surfaces an
    // error instead of leaving the UI on "running…" forever. (Validation
    // before activate catches the common parse-error case earlier.)
    const ctrl = new AbortController()
    const timer = setTimeout(() => ctrl.abort(), 20000)
    try {
        const resp = await fetch('/v1/demo/fire', {
            method: 'POST',
            credentials: 'same-origin',
            headers: { 'Content-Type': 'application/json' },
            signal: ctrl.signal,
            body: JSON.stringify({
                method: req.method,
                path: req.path,
                headers: req.headers ?? {},
                body: req.body ?? '',
            }),
        })
        if (check401(resp)) {
            throw new Error(`fire: 401 unauthorized`)
        }
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

// getDemoInfo reports the chassis web-inlet port (the data plane), which
// differs from the admin port that serves this UI. Returns null when the
// endpoint 404s — used by the store as the "is the chassis in demo mode?"
// probe (presence ⇒ yes), and by the Runner to build copy-as-curl URLs.
export async function getDemoInfo(): Promise<DemoInfo | null> {
    const resp = await fetch('/v1/demo/info', { credentials: 'same-origin' })
    if (!resp.ok) {
        if (resp.status === 404) return null
        throw new Error(`/v1/demo/info: ${resp.status} ${resp.statusText}`)
    }
    return (await resp.json()) as DemoInfo
}

// Curriculum shape — must match chassis/demo.Curriculum (single source
// of truth lives in Go; this is the SPA's TypeScript view of the same
// JSON). Any new field added server-side: declare it here too (or use
// `unknown`) so the SPA can still render.
export interface DemoOpFile {
    name: string
    scope: number
    txcl: string
    js?: string
}
export interface DemoStep {
    title: string
    prose: string
    ops: DemoOpFile[]
    method: 'GET' | 'POST' | 'PUT' | 'DELETE'
    path: string
    body?: string
    stack: string
}
export interface DemoTrack {
    id: string
    title: string
    steps: DemoStep[]
}
export interface Curriculum {
    host_suffix: string
    tracks: DemoTrack[]
}

// getCurriculum fetches the demo walkthrough curriculum from
// /v1/demo/curriculum. The chassis owns the curriculum data
// (chassis/demo/curriculum.go); this SPA fetches it on mount rather
// than carrying its own copy so the two can't drift. The endpoint is
// only mounted when the chassis is in demo mode, so a non-demo
// chassis returns 404 here — same probe shape as getDemoInfo.
export async function getCurriculum(): Promise<Curriculum> {
    const resp = await fetch('/v1/demo/curriculum', {
        credentials: 'same-origin',
    })
    if (!resp.ok) {
        throw new Error(
            `/v1/demo/curriculum: ${resp.status} ${resp.statusText}`
        )
    }
    return (await resp.json()) as Curriculum
}

export interface BuildOpResult {
    ref: string // "compute://sha256/<digest>"
    digest: string
    engine: string
    bytes: number
}

// buildDemoOp ships a single compute-op source (JS/TS) to the demo build
// endpoint, which bundles + compiles + uploads it as a content-addressed
// wasm artifact and returns the `compute://sha256/…` ref to splice into
// the op's txcl in place of `op://<name>`. Same toolchain `txco apply`
// uses (esbuild + javy); identical source skips javy on repeat thanks to
// the server-side wasm cache. javy must be on PATH — surfaces as a
// structured "compile_unavailable" error.
export async function buildDemoOp(
    source: string,
    lang: 'js' | 'ts' | 'mjs' = 'js'
): Promise<BuildOpResult> {
    const resp = await fetch('/v1/demo/op/build', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ source, lang }),
    })
    if (!resp.ok) {
        // The server returns `{error: "<code>", detail: {...map...}}` (see
        // chassis/server/admin/ops.go writeJSONError → errorResponse).
        // Surface the code plus a human-readable message extracted from
        // the detail map — common keys are `detail` (compile errors from
        // CleanJSError / javy install hint) and `err` (Go error strings).
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
