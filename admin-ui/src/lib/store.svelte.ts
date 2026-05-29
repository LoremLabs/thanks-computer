import {
    activateStack,
    BootstrapInvalidError,
    createDraft,
    createSecret as apiCreateSecret,
    deleteFile,
    deleteSession,
    diffVersions,
    exchangeToken,
    getSession,
    getTrace,
    getVersion,
    listSecrets,
    listStacks,
    listTenants,
    listTraces,
    listVersions,
    patchFile,
    revokeSecret as apiRevokeSecret,
    rotateSecret as apiRotateSecret,
    SecretStoreUnavailableError,
    updateSecretDescription as apiUpdateSecretDescription,
    validateVersion,
    ValidationFailedError,
    type DiffResponse,
    type SecretMetadata,
    type SessionInfo,
    type Stack,
    type Tenant,
    type TraceStep,
    type ValidateResponse,
    type Version,
    type VersionDetail,
} from './api'
import type { TraceStreamEvent } from './traceStream'
import { opId, type Op } from './types'
import { fileForOp, versionToOps } from './version_adapter'

// TraceCachedEvent is the cache entry shape — exactly the wire shape
// the live-stream emits. We re-export the alias so consumers don't
// reach across modules for the type.
export type TraceCachedEvent = TraceStreamEvent

// Svelte 5 runes store: a single $state object the rest of the app
// reads/mutates. Module-level so every component sees the same
// instance.
interface Selection {
    op: string // opId or ''
    stack: string // stack name or ''
    // Optional version override for the selection's stack. When
    // empty, the stack's active version is shown. Passed through to
    // currentVersionByStack on hash sync.
    version: number | null
    // 'versions' = render the versions-list page for `stack` instead
    // of the box view. 'login' = render the browser-auth login view
    // (with an optional `loginToken` to auto-exchange on landing).
    // 'traces' = render the trace stream (list or per-rid detail).
    // 'secrets' = render the secrets list or per-secret detail.
    // Otherwise '' (op-or-stack view).
    page: 'versions' | 'login' | 'traces' | 'secrets' | ''
    // Captured from `#login?t=<token>` and consumed once by
    // syncFromHash → tryExchange. Not part of the persistent URL.
    loginToken?: string
    // Captured from `#traces/<rid>` — empty when on the list view.
    traceRid?: string
    // Captured from `#secrets/<name>` — empty when on the list view.
    secretName?: string
}

function cacheKey(stack: string, n: number): string {
    return `${stack}:${n}`
}

// Wall-clock for a stack's slice of a trace. Prefers timestamps so
// overlapping siblings (same-scope parallel steps) don't get double-
// counted — a naive sum would hide whether parallel execution worked.
// Falls back to per-scope max-then-sum when timestamps are missing,
// and finally to a flat sum if scope is also missing.
function stackWallclockMs(steps: TraceStep[]): number {
    let minStart = Infinity
    let maxEnd = -Infinity
    let timestampsComplete = true
    for (const s of steps) {
        if (!s.started_at || !s.finished_at) {
            timestampsComplete = false
            break
        }
        const start = Date.parse(s.started_at)
        const end = Date.parse(s.finished_at)
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
        let total = 0
        for (const v of perScope.values()) total += v
        return total
    }
    return flatSum
}

function createStore() {
    const initial = readHash()
    const state = $state({
        // Flat ops list — concatenation of all stacks' currently-
        // selected versions (active by default). Rebuilt whenever a
        // version selection or fetch resolves.
        ops: [] as Op[],
        loading: false as boolean,
        error: null as string | null,
        selectedId: initial.op,
        selectedStack: initial.stack,
        // If set, the App renders a versions-list page for this stack
        // name instead of the box view / op detail. Driven by the
        // `#stack/<name>/versions` hash.
        showVersionsList: initial.page === 'versions' ? initial.stack : '',
        // Traces stream routing. Empty when not on the traces view;
        // '__list__' for the list page; '<rid>' for a per-trace detail.
        // Hash forms: #traces and #traces/<rid>.
        showTraces:
            initial.page === 'traces'
                ? initial.traceRid && initial.traceRid !== ''
                    ? initial.traceRid
                    : '__list__'
                : '',
        // Secrets routing, mirroring showTraces. '' when not on the
        // secrets view; '__list__' for the list page; '<name>' for a
        // per-secret detail. Hash forms: #secrets and #secrets/<name>.
        showSecrets:
            initial.page === 'secrets'
                ? initial.secretName && initial.secretName !== ''
                    ? initial.secretName
                    : '__list__'
                : '',
        // Secret metadata for the current tenant (read path). No values
        // ever land here. secretsUnavailable is set when the chassis
        // returns 503 secret_store_unavailable (operator opted out via
        // empty SecretMasterKeyPath) so the list renders a distinct
        // "not configured" state rather than an error.
        secrets: [] as SecretMetadata[],
        secretsLoaded: false,
        secretsUnavailable: false,
        loadedAt: null as Date | null,
        // Tenant directory (unchanged).
        tenants: [] as Tenant[],
        currentTenant: '' as string,
        // Versioned-opstack state.
        //
        // `stacks` is the tenant's stack directory. Each row carries
        // the active_version pointer. Refreshed by refresh().
        stacks: [] as Stack[],
        // Per-stack full version history (drafts + superseded). Fetched
        // lazily — on the first stack-select that needs it (currently:
        // when the user opens the versions-list page or the version
        // dropdown).
        versionsByStack: {} as Record<string, Version[]>,
        // Per-stack current selection. Defaults to that stack's
        // active_version on first sight. Updated by setStackVersion;
        // overridden by hash routing (#stack/<name>/v<N>).
        currentVersionByStack: {} as Record<string, number>,
        // Cache of full VersionDetail (incl. files) keyed by
        // "<stack>:<version_number>". Populated on demand by
        // ensureVersionLoaded; never evicted in this phase (workspaces
        // we care about have a handful of stacks × handful of versions).
        versionCache: {} as Record<string, VersionDetail>,
        // Cache of /diff responses keyed by "<stack>:<v1>:<v2>". The
        // server-side response is just per-file hashes + change tags
        // so this is tiny per entry; never evicted in this phase.
        // VersionsList's per-row expansion is the only writer.
        diffCache: {} as Record<string, DiffResponse>,
        // Most-recent validation response per "<stack>:<version>".
        // Populated by patchDraftFile (and by future explicit Validate
        // actions). Read back by editor components to render per-file
        // errors inline.
        lastValidation: {} as Record<string, ValidateResponse>,
        // Most-recent observed duration_ms per op, keyed by opId().
        lastDurations: {} as Record<string, number>,
        lastInputs: {} as Record<string, unknown>,
        lastOutputs: {} as Record<string, unknown>,
        stackLastDurations: {} as Record<string, number>,
        jsonCollapsed: readJsonCollapsed(),
        // Per-stack sidebar collapse overrides, persisted to
        // localStorage. A present key is an explicit user choice
        // (true = collapsed, false = expanded); absent means "follow
        // the default" (OpTree opens only the first stack when there's
        // more than one). Mirrors jsonCollapsed.
        stacksCollapsed: readStacksCollapsed(),
        // Browser-auth session state. `session === null` means either
        // "haven't checked yet" (sessionLoaded=false) or "checked and
        // not authed" (sessionLoaded=true). open_dev passes through as
        // session.open_dev=true so the UI renders the dashboard without
        // routing to /login. loginPending is true while an exchange is
        // in flight (used by the Login view to render a spinner).
        // intendedHash captures the URL the user was trying to reach
        // when a 401 happened, so we can restore it after re-login.
        session: null as SessionInfo | null,
        sessionLoaded: false,
        loginPending: false,
        loginError: null as string | null,
        intendedHash: '' as string,
        // Live-trace cache: keyed by rid, holding the full ClosedTrace
        // shape received over /traces/stream. Populated by TracesList
        // as events arrive; consumed by TraceDetail to avoid an
        // unnecessary archive-lookup roundtrip (which often 404s for
        // successful traces under the default `on-error` policy).
        // Bounded by LIVE_TRACE_CACHE_MAX with FIFO eviction; the
        // eviction order is maintained separately so we don't have to
        // walk the map on each insert.
        liveTraceCache: {} as Record<string, TraceCachedEvent>,
        liveTraceOrder: [] as string[],
    })

    async function refreshTenants() {
        try {
            const tenants = await listTenants()
            state.tenants = tenants
            if (tenants.length === 0) {
                state.currentTenant = ''
                state.error = 'no tenants visible to this caller'
                return
            }
            const saved = readSavedTenant()
            const found = saved && tenants.find((t) => t.slug === saved)
            state.currentTenant = found ? saved : tenants[0].slug
            persistTenant(state.currentTenant)
        } catch (e) {
            state.error = e instanceof Error ? e.message : String(e)
            state.tenants = []
        }
    }

    // refresh re-fetches the full ops picture for the current tenant:
    // list stacks → fetch each stack's currently-selected version
    // (default = active_version) → rebuild state.ops as the union of
    // all stacks' op rows.
    //
    // The UI's sidebar groups by stack, so showing every stack's ops
    // at once preserves the current navigation experience even
    // though the storage model is now per-stack-per-version.
    async function refresh() {
        state.loading = true
        state.error = null
        try {
            if (!state.currentTenant) await refreshTenants()
            if (!state.currentTenant) {
                state.ops = []
                return
            }
            const stacks = await listStacks(state.currentTenant)
            state.stacks = stacks
            // Seed currentVersionByStack: keep an existing selection
            // if the user has one and it's still valid; otherwise
            // default to active_version. Stacks without an active
            // version yet (drafts only) fall back to their newest
            // version_number so the sidebar still has something to
            // show — costs one extra fetch per unactivated stack on
            // first load.
            const needNewest: string[] = []
            for (const s of stacks) {
                const existing = state.currentVersionByStack[s.name]
                if (existing && existing > 0) continue
                if (typeof s.active_version === 'number') {
                    state.currentVersionByStack[s.name] = s.active_version
                } else {
                    needNewest.push(s.name)
                }
            }
            if (needNewest.length > 0) {
                await Promise.all(
                    needNewest.map(async (name) => {
                        const vs = await listVersions(state.currentTenant, name)
                        state.versionsByStack[name] = vs
                        if (vs.length > 0) {
                            // listVersions returns newest first (server orders DESC).
                            state.currentVersionByStack[name] = vs[0].version_number
                        }
                    })
                )
            }
            await rebuildOps()
            state.loadedAt = new Date()
        } catch (e) {
            state.error = e instanceof Error ? e.message : String(e)
            state.ops = []
        } finally {
            state.loading = false
        }
    }

    // ensureVersionLoaded fetches (stack, n) into versionCache if not
    // already present. Idempotent.
    async function ensureVersionLoaded(stack: string, n: number): Promise<VersionDetail | null> {
        if (!stack || !n) return null
        const key = cacheKey(stack, n)
        const hit = state.versionCache[key]
        if (hit) return hit
        const detail = await getVersion(state.currentTenant, stack, n)
        if (detail) state.versionCache[key] = detail
        return detail
    }

    // ensureDiff fetches /diff?v1=..&v2=.. once per (stack, v1, v2) and
    // caches the response. The server response is hashes-only so this
    // is tiny; only the line-level diff renderer needs file content,
    // which it pulls via ensureVersionLoaded separately.
    async function ensureDiff(stack: string, v1: number, v2: number): Promise<DiffResponse | null> {
        if (!stack || !v1 || !v2) return null
        const key = `${stack}:${v1}:${v2}`
        const hit = state.diffCache[key]
        if (hit) return hit
        const d = await diffVersions(state.currentTenant, stack, v1, v2)
        if (d) state.diffCache[key] = d
        return d
    }

    // rebuildOps: for every stack with a selected version, fetch (if
    // needed) its detail and concat the resulting Op[]. Sets
    // state.ops once at the end so the UI only re-renders once.
    async function rebuildOps() {
        const pending: Array<Promise<Op[]>> = []
        for (const s of state.stacks) {
            const n = state.currentVersionByStack[s.name]
            if (!n) continue
            pending.push(
                ensureVersionLoaded(s.name, n).then((v) =>
                    v ? versionToOps(v, s.name) : []
                )
            )
        }
        const chunks = await Promise.all(pending)
        state.ops = chunks.flat()
    }

    // setStackVersion: switch which version of `stack` the UI is
    // viewing. Loads the version detail (cache or fetch), rebuilds
    // ops, and re-syncs the hash so deep-linking works.
    async function setStackVersion(stack: string, n: number) {
        if (!stack || !n) return
        if (state.currentVersionByStack[stack] === n && state.versionCache[cacheKey(stack, n)]) {
            return // already current and cached
        }
        state.currentVersionByStack[stack] = n
        await ensureVersionLoaded(stack, n)
        await rebuildOps()
        // Keep the URL in sync only when this stack is the selection.
        // Otherwise we're pre-loading silently (e.g. on initial refresh).
        if (state.selectedStack === stack && state.showVersionsList !== stack) {
            writeHash(currentSelection())
        }
    }

    // refreshVersions fetches the full history for one stack. Called
    // lazily when the user opens the versions-list page or the
    // version dropdown.
    async function refreshVersions(stack: string) {
        if (!stack || !state.currentTenant) return
        try {
            const versions = await listVersions(state.currentTenant, stack)
            state.versionsByStack[stack] = versions
        } catch {
            // ignore — keep whatever's already in the map
        }
    }

    async function setTenant(slug: string) {
        if (!slug || slug === state.currentTenant) return
        state.currentTenant = slug
        persistTenant(slug)
        // Invalidate per-tenant caches. Drafts and version numbers
        // are tenant-scoped, so nothing carries across.
        state.selectedId = ''
        state.selectedStack = ''
        state.showVersionsList = ''
        state.stacks = []
        state.versionsByStack = {}
        state.currentVersionByStack = {}
        state.versionCache = {}
        // Secrets are tenant-scoped too — drop the cache and reload.
        state.secrets = []
        state.secretsLoaded = false
        state.secretsUnavailable = false
        writeHash({ op: '', stack: '', version: null, page: '' })
        await refresh()
        refreshLastDurations()
        refreshSecrets()
    }

    async function refreshLastDurations(limit = 20) {
        try {
            const summaries = await listTraces(limit)
            if (summaries.length === 0) return
            const traces = await Promise.all(
                summaries.map((s) => getTrace(s.rid).catch(() => null))
            )
            const nextDur: Record<string, number> = {}
            const nextIn: Record<string, unknown> = {}
            const nextOut: Record<string, unknown> = {}
            const nextStack: Record<string, number> = {}
            for (const t of traces) {
                if (!t?.steps) continue
                const byStack = new Map<string, TraceStep[]>()
                for (const step of t.steps) {
                    const id = `${step.stack}/${step.scope}/${step.name}`
                    if (typeof step.duration_ms === 'number' && !(id in nextDur)) {
                        nextDur[id] = step.duration_ms
                    }
                    if (!(id in nextIn) && step.in !== undefined) {
                        nextIn[id] = step.in
                    }
                    if (!(id in nextOut) && step.out !== undefined) {
                        nextOut[id] = step.out
                    }
                    if (!byStack.has(step.stack)) byStack.set(step.stack, [])
                    byStack.get(step.stack)!.push(step)
                }
                for (const [s, steps] of byStack) {
                    if (!(s in nextStack)) nextStack[s] = stackWallclockMs(steps)
                }
            }
            state.lastDurations = nextDur
            state.lastInputs = nextIn
            state.lastOutputs = nextOut
            state.stackLastDurations = nextStack
        } catch {
            // best-effort
        }
    }

    function currentSelection(): Selection {
        // Traces view takes precedence over stack/op selection — when
        // we're on the traces page, the URL is purely #traces[/<rid>]
        // and we don't carry a stack pin.
        if (state.showTraces) {
            return {
                op: '',
                stack: '',
                version: null,
                page: 'traces',
                traceRid: state.showTraces === '__list__' ? '' : state.showTraces,
            }
        }
        if (state.showSecrets) {
            return {
                op: '',
                stack: '',
                version: null,
                page: 'secrets',
                secretName:
                    state.showSecrets === '__list__' ? '' : state.showSecrets,
            }
        }
        // Stack name to use for version-pinning: either the explicit
        // selectedStack (stack/versions view) or the op's parent
        // stack (op-detail view). The URL form is decided separately
        // by sel.op vs sel.stack.
        let pinStack = state.selectedStack
        if (!pinStack && state.selectedId) {
            pinStack = state.selectedId.split('/')[0] || ''
        }
        const versionForStack = pinStack ? state.currentVersionByStack[pinStack] : undefined
        const activeForStack = pinStack
            ? state.stacks.find((s) => s.name === pinStack)?.active_version
            : undefined
        // Only include `v<N>` in the URL when the user is viewing
        // something other than the active version — keeps short URLs
        // for the common case.
        const versionOverride =
            versionForStack && versionForStack !== activeForStack ? versionForStack : null
        const page: 'versions' | '' =
            state.showVersionsList === state.selectedStack && state.selectedStack ? 'versions' : ''
        return {
            op: state.selectedId,
            stack: state.selectedStack,
            version: versionOverride,
            page,
        }
    }

    function selectOp(op: Op | null) {
        state.selectedId = op ? opId(op) : ''
        // Note: selectedStack stays empty when picking an op leaf —
        // App.svelte derives the "focused stack for header context"
        // from either selectedStack or the selected op's parent
        // stack. This preserves the existing routing (stack vs op
        // detail) while still letting the header show a version
        // badge during op-detail browsing.
        state.selectedStack = ''
        state.showVersionsList = ''
        state.showTraces = ''
        state.showSecrets = ''
        writeHash(currentSelection())
    }

    function selectStack(stack: string | null) {
        state.selectedStack = stack || ''
        state.selectedId = ''
        state.showVersionsList = ''
        state.showTraces = ''
        state.showSecrets = ''
        writeHash(currentSelection())
    }

    // Open the versions-list page for `stack`. The selected stack
    // stays the same so navigating "back" lands sensibly.
    function showVersions(stack: string) {
        if (!stack) return
        state.selectedStack = stack
        state.selectedId = ''
        state.showVersionsList = stack
        state.showTraces = ''
        state.showSecrets = ''
        // Lazy-fetch history.
        if (!state.versionsByStack[stack]) refreshVersions(stack)
        writeHash(currentSelection())
    }

    // Open the traces stream. Pass an rid for the per-trace detail
    // view, or call with no argument for the list page. Clears the
    // stack/op selection so the sidebar nav reflects "we're on
    // traces, not on a stack".
    function showTraces(rid?: string) {
        state.selectedId = ''
        state.selectedStack = ''
        state.showVersionsList = ''
        state.showSecrets = ''
        state.showTraces = rid && rid !== '' ? rid : '__list__'
        writeHash(currentSelection())
    }

    // Open the secrets view. Pass a name for the per-secret detail
    // view, or call with no argument for the list page. Mirrors
    // showTraces: clears the stack/op/traces selection so the sidebar
    // nav reflects "we're on secrets".
    function showSecrets(name?: string) {
        state.selectedId = ''
        state.selectedStack = ''
        state.showVersionsList = ''
        state.showTraces = ''
        state.showSecrets = name && name !== '' ? name : '__list__'
        if (!state.secretsLoaded) refreshSecrets()
        writeHash(currentSelection())
    }

    // refreshSecrets loads secret metadata for the current tenant.
    // Best-effort: a 503 (feature opted out) sets secretsUnavailable
    // so the list renders a distinct state; a 401 has already fired
    // session-lost via the api layer; any other error leaves the list
    // empty without clobbering the global error banner (this is a
    // background load triggered on nav/boot).
    async function refreshSecrets() {
        if (!state.currentTenant) {
            state.secrets = []
            state.secretsUnavailable = false
            state.secretsLoaded = true
            return
        }
        try {
            const secrets = await listSecrets(state.currentTenant)
            state.secrets = secrets
            state.secretsUnavailable = false
        } catch (e) {
            state.secrets = []
            state.secretsUnavailable = e instanceof SecretStoreUnavailableError
        } finally {
            state.secretsLoaded = true
        }
    }

    // --- secret write actions ---------------------------------------
    // Each mutates via the api then reloads the metadata list so the
    // table + detail reflect the new state. Errors propagate to the
    // caller (the modal / detail panel renders them inline); these do
    // NOT touch the global error banner. No cleartext is ever stored
    // on the store — values pass straight through to the api call.

    async function createSecret(
        name: string,
        value: string,
        description: string
    ): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        await apiCreateSecret(state.currentTenant, name, value, description)
        await refreshSecrets()
    }

    async function rotateSecret(name: string, value: string): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        await apiRotateSecret(state.currentTenant, name, value)
        await refreshSecrets()
    }

    async function updateSecretDescription(
        name: string,
        description: string
    ): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        await apiUpdateSecretDescription(state.currentTenant, name, description)
        await refreshSecrets()
    }

    async function revokeSecret(name: string): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        await apiRevokeSecret(state.currentTenant, name)
        await refreshSecrets()
    }

    function syncFromHash() {
        const h = readHash()
        // Login deep-link: `#login?t=<token>` triggers a one-shot
        // exchange and then clears the token from the URL so a stray
        // back/forward navigation can't re-fire it (the server would
        // reject the second consume anyway, but a clean URL is nicer).
        if (h.page === 'login') {
            if (h.loginToken) {
                const token = h.loginToken
                // Replace the URL first so the token doesn't linger.
                writeHash({ op: '', stack: '', version: null, page: 'login' })
                tryExchange(token)
            }
            // Selection state stays whatever it was — App.svelte
            // overlays the Login view based on requiresLogin(state).
            return
        }
        // Traces routes override everything else — they're not
        // stack-scoped and don't carry an op/version pin.
        if (h.page === 'traces') {
            state.selectedId = ''
            state.selectedStack = ''
            state.showVersionsList = ''
            state.showSecrets = ''
            state.showTraces = h.traceRid && h.traceRid !== '' ? h.traceRid : '__list__'
            return
        }
        // Secrets routes — same shape as traces (not stack-scoped).
        if (h.page === 'secrets') {
            state.selectedId = ''
            state.selectedStack = ''
            state.showVersionsList = ''
            state.showTraces = ''
            state.showSecrets =
                h.secretName && h.secretName !== '' ? h.secretName : '__list__'
            if (!state.secretsLoaded) refreshSecrets()
            return
        }
        state.selectedId = h.op
        state.selectedStack = h.stack
        state.showVersionsList = h.page === 'versions' ? h.stack : ''
        state.showTraces = ''
        state.showSecrets = ''
        if (h.stack && typeof h.version === 'number') {
            // The hash carries an explicit version pin; honor it.
            // Fire-and-forget: setStackVersion fetches + rebuilds ops.
            setStackVersion(h.stack, h.version)
        }
        if (h.page === 'versions' && h.stack && !state.versionsByStack[h.stack]) {
            refreshVersions(h.stack)
        }
    }

    // refreshSession is called at app boot and any time a 401 fires
    // on a downstream fetch. It's the single source of truth for
    // "what does the chassis think our auth context is right now."
    async function refreshSession() {
        try {
            const s = await getSession()
            state.session = s
            state.sessionLoaded = true
            state.loginError = null
            // In open-dev mode the chassis advertises {open_dev: true}
            // and the UI skips the login flow entirely. The tenant
            // selector + everything else still works.
        } catch (e) {
            // Network or 5xx — leave session=null but mark loaded so
            // the login view renders with an error rather than a
            // forever-loading spinner.
            state.session = null
            state.sessionLoaded = true
            state.loginError = e instanceof Error ? e.message : String(e)
        }
    }

    // tryExchange POSTs the bootstrap token to /auth/browser/exchange.
    // On success the cookie is set and refreshSession populates the
    // store; on failure loginError is surfaced inline.
    async function tryExchange(token: string) {
        state.loginPending = true
        state.loginError = null
        try {
            await exchangeToken(token)
            await refreshSession()
            // Restore the URL the user was trying to reach (captured
            // when a 401 routed them here). If there was no intended
            // hash, leave them on the default landing page.
            if (state.intendedHash) {
                if (typeof window !== 'undefined') {
                    window.location.hash = state.intendedHash
                }
                state.intendedHash = ''
            } else if (typeof window !== 'undefined' && window.location.hash === '#login') {
                window.location.hash = ''
            }
        } catch (e) {
            if (e instanceof BootstrapInvalidError) {
                state.loginError = e.message
            } else {
                state.loginError = e instanceof Error ? e.message : String(e)
            }
        } finally {
            state.loginPending = false
        }
    }

    // signOut hits the chassis's DELETE endpoint then clears local
    // session state. We deliberately reset to "sessionLoaded but no
    // session" so the Login view renders without a second fetch.
    async function signOut() {
        try {
            await deleteSession()
        } catch {
            // Server-side revoke failed — fall through to local
            // cleanup. Cookie may or may not have been cleared by
            // the server response; resetting local state still
            // bounces the user to login, which is what they wanted.
        }
        state.session = null
        state.sessionLoaded = true
        if (typeof window !== 'undefined') {
            window.location.hash = 'login'
        }
    }

    // captureIntendedHash records the URL the user was on when a 401
    // happened, so we can return them there after re-login. Skips
    // capturing when they were already on #login (avoids loops).
    function captureIntendedHash() {
        if (typeof window === 'undefined') return
        const h = window.location.hash || ''
        if (h && !h.startsWith('#login')) {
            state.intendedHash = h.startsWith('#') ? h.slice(1) : h
        }
    }

    // Activate a version, then immediately clone-to-draft so the user
    // always has an editable draft sitting alongside the new active.
    //
    // Pre-flight: run server-side validate first. If parse / ref /
    // graph checks fail, surface the errors and do NOT activate —
    // matches the "you can't publish a broken version" rule.
    //
    // Post-flight refreshes: stacks (active_version pointer changed),
    // versionsByStack (statuses + new draft row), versionCache (drop
    // the per-stack slice — statuses changed), and ops (sidebar
    // reflects new active).
    //
    // Best-effort on clone-to-draft: if it fails, the activation
    // still happened — surface the error but don't roll back.
    async function activateVersion(stack: string, versionNumber: number): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        const v = await validateVersion(state.currentTenant, stack, versionNumber)
        if (!v.ok) {
            throw new ValidationFailedError(stack, versionNumber, v.errors ?? [])
        }
        await activateStack(state.currentTenant, stack, versionNumber)
        let cloneErr: unknown
        try {
            await createDraft(state.currentTenant, stack, 'active')
        } catch (e) {
            cloneErr = e
        }
        // Drop cached version details for this stack — at least two
        // rows just flipped status.
        for (const key of Object.keys(state.versionCache)) {
            if (key.startsWith(stack + ':')) delete state.versionCache[key]
        }
        await refreshVersions(stack)
        try {
            const stacks = await listStacks(state.currentTenant)
            state.stacks = stacks
        } catch {
            // ignore — UI degrades
        }
        state.currentVersionByStack[stack] = versionNumber
        await rebuildOps()
        if (cloneErr) {
            throw cloneErr instanceof Error
                ? cloneErr
                : new Error(String(cloneErr))
        }
    }

    // Edit a single file inside a draft. Optimistic-concurrency
    // honored via baseHash (see api.ts patchFile). On success:
    //   - drop the per-stack version cache slice so the next read
    //     refetches the new content + hash from the server,
    //   - re-fetch the version list (manifest_hash changed),
    //   - rebuild the flat ops list so the UI reflects the new content,
    //   - fire validateVersion and cache the result under
    //     `${stack}:${n}` so the editor can render per-file errors
    //     inline.
    //
    // Validation is best-effort: a failed validate call doesn't fail
    // the save (the file is already persisted server-side).
    async function patchDraftFile(
        stack: string,
        n: number,
        path: string,
        content: string,
        baseHash: string
    ): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        await patchFile(state.currentTenant, stack, n, path, content, baseHash)
        delete state.versionCache[cacheKey(stack, n)]
        await refreshVersions(stack)
        await rebuildOps()
        try {
            const v = await validateVersion(state.currentTenant, stack, n)
            state.lastValidation[`${stack}:${n}`] = v
        } catch {
            // ignore — validation is advisory
        }
    }

    // patchMockFile — auto-draft-on-save variant used by JsonEditor in
    // the Mock tab. If the focused version is already a draft, behaves
    // like patchDraftFile (with a re-derived baseHash). Otherwise it
    // clones a fresh draft from active (matching the Clone button's
    // policy of always creating, never reusing), applies the edit to
    // the new draft, and switches focus to it.
    //
    // baseHash is re-derived from the target draft's file list so the
    // caller doesn't need to know which version's hashes apply. For
    // the common "edit on active" case the cloned draft's content_hash
    // matches active's, so the PATCH succeeds without a round-trip
    // mismatch.
    async function patchMockFile(
        stack: string,
        focusedN: number,
        path: string,
        content: string
    ): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        const focused = state.versionsByStack[stack]?.find(
            (v) => v.version_number === focusedN
        )
        const isDraft = focused?.status === 'draft'
        let targetN = focusedN
        if (!isDraft) {
            const { version_number } = await createDraft(
                state.currentTenant,
                stack,
                'active'
            )
            targetN = version_number
            await refreshVersions(stack)
            try {
                const stacks = await listStacks(state.currentTenant)
                state.stacks = stacks
            } catch {
                // ignore — UI degrades
            }
        }
        const detail = await ensureVersionLoaded(stack, targetN)
        const baseHash =
            detail?.files?.find((f) => f.path === path)?.content_hash ?? ''
        await patchFile(state.currentTenant, stack, targetN, path, content, baseHash)
        delete state.versionCache[cacheKey(stack, targetN)]
        await refreshVersions(stack)
        await rebuildOps()
        if (targetN !== focusedN) {
            await setStackVersion(stack, targetN)
        }
        try {
            const v = await validateVersion(state.currentTenant, stack, targetN)
            state.lastValidation[`${stack}:${targetN}`] = v
        } catch {
            // ignore — validation is advisory
        }
    }

    // createOp — auto-draft + INSERT a brand-new .txcl file at
    // <scope>/<name>.txcl. Server's PATCH endpoint accepts base_hash=''
    // as the create marker (see chassis/server/admin/stacks.go), so we
    // route through patchFile with an empty hash. The duplicate-name /
    // already-exists check is best-effort client-side; the server is
    // authoritative and returns 409 file_already_exists if we miss it.
    async function createOp(
        stack: string,
        focusedN: number,
        scope: number,
        name: string,
        content: string
    ): Promise<{ versionNumber: number; opId: string }> {
        if (!state.currentTenant) throw new Error('no tenant')
        if (!stack || !name || !Number.isFinite(scope) || scope < 0) {
            throw new Error('invalid op identity')
        }
        const focused = state.versionsByStack[stack]?.find(
            (v) => v.version_number === focusedN
        )
        const isDraft = focused?.status === 'draft'
        let targetN = focusedN
        if (!isDraft) {
            const { version_number } = await createDraft(
                state.currentTenant,
                stack,
                'active'
            )
            targetN = version_number
            await refreshVersions(stack)
            try {
                const stacks = await listStacks(state.currentTenant)
                state.stacks = stacks
            } catch {
                // ignore — UI degrades
            }
        }
        const path = fileForOp(scope, name)
        await patchFile(state.currentTenant, stack, targetN, path, content, '')
        delete state.versionCache[cacheKey(stack, targetN)]
        await refreshVersions(stack)
        await rebuildOps()
        if (targetN !== focusedN) {
            await setStackVersion(stack, targetN)
        }
        // Drop the user on the new op's detail view so they can keep
        // editing the txcl in the Resonator tab.
        const newOp: Op = { stack, scope, name, txcl: content }
        selectOp(newOp)
        try {
            const v = await validateVersion(state.currentTenant, stack, targetN)
            state.lastValidation[`${stack}:${targetN}`] = v
        } catch {
            // ignore — validation is advisory
        }
        return { versionNumber: targetN, opId: opId(newOp) }
    }

    // deleteOp — remove a single op (its <scope>/<name>.txcl file) from the
    // editable draft. Mirrors createOp's auto-draft: if the focused version
    // isn't a draft, clone active into a fresh draft and delete there, so the
    // live opstack is untouched until activation. Mock files are shared
    // per-scope (version_adapter), so they are intentionally NOT removed.
    async function deleteOp(
        stack: string,
        focusedN: number,
        scope: number,
        name: string
    ): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        const focused = state.versionsByStack[stack]?.find(
            (v) => v.version_number === focusedN
        )
        let targetN = focusedN
        if (focused?.status !== 'draft') {
            const { version_number } = await createDraft(
                state.currentTenant,
                stack,
                'active'
            )
            targetN = version_number
            await refreshVersions(stack)
            try {
                state.stacks = await listStacks(state.currentTenant)
            } catch {
                // ignore — UI degrades
            }
        }
        const path = fileForOp(scope, name)
        // Re-derive base_hash from the target draft's files (the cloned file
        // keeps active's content_hash, but re-deriving is the robust pattern
        // patchMockFile already uses).
        const detail = await ensureVersionLoaded(stack, targetN)
        const baseHash =
            detail?.files?.find((f) => f.path === path)?.content_hash ?? ''
        await deleteFile(state.currentTenant, stack, targetN, path, baseHash)
        delete state.versionCache[cacheKey(stack, targetN)]
        await refreshVersions(stack)
        await rebuildOps()
        if (targetN !== focusedN) {
            await setStackVersion(stack, targetN)
        }
        // The deleted op was the one being viewed — drop the user onto the
        // parent stack's canvas (remaining ops) rather than an empty panel.
        selectStack(stack)
        try {
            const v = await validateVersion(state.currentTenant, stack, targetN)
            state.lastValidation[`${stack}:${targetN}`] = v
        } catch {
            // ignore — validation is advisory
        }
    }

    // Clone the stack's currently-active version into a fresh draft
    // and switch the UI to it. Entry point for "Edit" when the user
    // is viewing a non-draft (active or superseded) version.
    async function createDraftForStack(stack: string): Promise<void> {
        if (!state.currentTenant) throw new Error('no tenant')
        const { version_number } = await createDraft(state.currentTenant, stack, 'active')
        // Refresh the version list so the new draft row shows up.
        await refreshVersions(stack)
        // Refresh stacks too — created_at on the stack row may not
        // change, but active_version pointers don't here either, so
        // this is mostly defensive.
        try {
            const stacks = await listStacks(state.currentTenant)
            state.stacks = stacks
        } catch {
            // ignore — UI degrades
        }
        await setStackVersion(stack, version_number)
        // Make sure the focused-stack selection is set so the editor
        // is visible without a second click.
        if (state.selectedStack !== stack) state.selectedStack = stack
        state.showVersionsList = ''
        writeHash(currentSelection())
    }

    // Cap on the live-trace cache. Matches TracesList's LIVE_RING_MAX
    // so the two stay in lockstep: anything visible in the ring is
    // resolvable via the cache without a server roundtrip.
    const LIVE_TRACE_CACHE_MAX = 5000

    function cacheLiveTrace(ev: TraceCachedEvent) {
        if (!ev.rid) return
        if (!(ev.rid in state.liveTraceCache)) {
            state.liveTraceOrder.push(ev.rid)
            // FIFO evict the oldest if we're over cap. Drop both from
            // the order list and the map so the entry doesn't linger.
            while (state.liveTraceOrder.length > LIVE_TRACE_CACHE_MAX) {
                const drop = state.liveTraceOrder.shift()
                if (drop) delete state.liveTraceCache[drop]
            }
        }
        state.liveTraceCache[ev.rid] = ev
    }

    function getCachedTrace(rid: string): TraceCachedEvent | undefined {
        return state.liveTraceCache[rid]
    }

    function toggleJsonCollapse(path: string) {
        if (state.jsonCollapsed[path]) {
            delete state.jsonCollapsed[path]
        } else {
            state.jsonCollapsed[path] = true
        }
        persistJsonCollapsed(state.jsonCollapsed)
    }

    // Record an explicit collapse choice for a sidebar stack and
    // persist it. `collapsed=true` collapses; `false` expands. OpTree
    // owns the default (first-open) rule; this only stores deviations
    // the user clicked.
    function setStackCollapsed(stack: string, collapsed: boolean) {
        state.stacksCollapsed[stack] = collapsed
        persistStacksCollapsed(state.stacksCollapsed)
    }

    return {
        state,
        activateVersion,
        cacheLiveTrace,
        captureIntendedHash,
        createDraftForStack,
        createOp,
        createSecret,
        deleteOp,
        ensureDiff,
        ensureVersionLoaded,
        getCachedTrace,
        patchDraftFile,
        patchMockFile,
        refresh,
        refreshSecrets,
        refreshSession,
        refreshTenants,
        refreshLastDurations,
        refreshVersions,
        revokeSecret,
        rotateSecret,
        selectOp,
        selectStack,
        setStackVersion,
        setTenant,
        showSecrets,
        showTraces,
        showVersions,
        signOut,
        syncFromHash,
        setStackCollapsed,
        toggleJsonCollapse,
        tryExchange,
        updateSecretDescription,
    }
}

// isAuthed encapsulates the "should the UI render the dashboard?"
// rule. Open-dev mode counts as authed (the chassis is explicitly not
// enforcing). The signed/basic sources are CLI shapes — defensive,
// since the UI never produces them, but treated as authed if seen.
export function isAuthed(s: SessionInfo | null): boolean {
    if (!s) return false
    if (s.open_dev) return true
    return s.source !== 'open'
}

// requiresLogin is the "should we show the login view?" predicate.
// False until the first session probe completes so we don't flash
// the login view during boot. False forever in open-dev.
export function requiresLogin(loaded: boolean, s: SessionInfo | null): boolean {
    return loaded && !isAuthed(s)
}

// hasCapability is the "should this write control be enabled?" rule.
// It mirrors the server's chassis/auth/policy matcher so the UI never
// disagrees with what the endpoint will allow: a capability is
// `domain:instance:action`; `admin:all` and bare `*` expand to
// `*:*:*`; a granted `*` segment matches anything. This is why a plain
// `includes()` was wrong — an admin's session carries `admin:all`
// (the super-admin flag is translated to that wildcard at browser-
// session bootstrap), which a literal match would miss.
//
// Open-dev mode (the chassis explicitly not enforcing) grants
// everything.
export function hasCapability(s: SessionInfo | null, cap: string): boolean {
    if (!s) return false
    if (s.open_dev) return true
    if (!s.capabilities) return false
    const wantSegs = capSegments(cap)
    return s.capabilities.some((g) => capMatches(g, wantSegs))
}

// capSegments normalises a capability to exactly three segments,
// matching chassis/auth/policy.segments: "admin:all"/"*" → *:*:*; a
// 2-segment legacy form (`opstack:read`) gets a "*" instance; anything
// else returns empty segments that never match.
function capSegments(cap: string): [string, string, string] {
    const c = cap.trim()
    if (c === '') return ['', '', '']
    if (c === 'admin:all' || c === '*') return ['*', '*', '*']
    const p = c.split(':')
    if (p.length === 2) return [p[0], '*', p[1]]
    if (p.length === 3) return [p[0], p[1], p[2]]
    return ['', '', '']
}

// capMatches reports whether a granted capability covers `wantSegs`,
// segment-by-segment: each granted segment passes if it's "*" or
// string-equal. Mirrors chassis/auth/policy.matches.
function capMatches(granted: string, wantSegs: [string, string, string]): boolean {
    if (granted.trim() === '') return false
    const g = capSegments(granted)
    for (let i = 0; i < 3; i++) {
        if (g[i] === '*') continue
        if (g[i] !== wantSegs[i]) return false
    }
    return true
}

const TENANT_KEY = 'admin-ui:tenant'

function readSavedTenant(): string {
    if (typeof localStorage === 'undefined') return ''
    try {
        return localStorage.getItem(TENANT_KEY) || ''
    } catch {
        return ''
    }
}

function persistTenant(slug: string) {
    if (typeof localStorage === 'undefined') return
    try {
        localStorage.setItem(TENANT_KEY, slug)
    } catch {
        // ignore quota / disabled storage
    }
}

const JSON_COLLAPSED_KEY = 'admin-ui:json-collapsed'

function readJsonCollapsed(): Record<string, boolean> {
    if (typeof localStorage === 'undefined') return {}
    try {
        const raw = localStorage.getItem(JSON_COLLAPSED_KEY)
        if (!raw) return {}
        const parsed = JSON.parse(raw)
        if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
            const out: Record<string, boolean> = {}
            for (const [k, v] of Object.entries(parsed)) {
                if (v === true) out[k] = true
            }
            return out
        }
    } catch {
        // ignore malformed storage
    }
    return {}
}

function persistJsonCollapsed(map: Record<string, boolean>) {
    if (typeof localStorage === 'undefined') return
    try {
        localStorage.setItem(JSON_COLLAPSED_KEY, JSON.stringify(map))
    } catch {
        // quota / disabled storage — collapse state survives the
        // session but won't carry across reloads. Acceptable.
    }
}

const STACKS_COLLAPSED_KEY = 'admin-ui:stacks-collapsed'

// Like readJsonCollapsed, but values are explicit booleans (true =
// collapsed, false = expanded) since the stack default isn't "all
// expanded" — absence means "follow the first-open default".
function readStacksCollapsed(): Record<string, boolean> {
    if (typeof localStorage === 'undefined') return {}
    try {
        const raw = localStorage.getItem(STACKS_COLLAPSED_KEY)
        if (!raw) return {}
        const parsed = JSON.parse(raw)
        if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
            const out: Record<string, boolean> = {}
            for (const [k, v] of Object.entries(parsed)) {
                if (typeof v === 'boolean') out[k] = v
            }
            return out
        }
    } catch {
        // ignore malformed storage
    }
    return {}
}

function persistStacksCollapsed(map: Record<string, boolean>) {
    if (typeof localStorage === 'undefined') return
    try {
        localStorage.setItem(STACKS_COLLAPSED_KEY, JSON.stringify(map))
    } catch {
        // quota / disabled storage — acceptable to lose across reloads.
    }
}

// Hash grammar (versioned):
//   #login                              → login view, manual paste
//   #login?t=<token>                    → login view, auto-exchange
//   #stack/<name>                       → box view, active version
//   #stack/<name>/v<N>                  → box view, version N
//   #stack/<name>/versions              → versions-list page
//   #ops/<stack>/<scope>/<name>         → op detail, active version
//   #ops/<stack>/v<N>/<scope>/<name>    → op detail, version N
//   #traces                             → live trace list
//   #traces/<rid>                       → per-trace detail
function readHash(): Selection {
    if (typeof window === 'undefined') return { op: '', stack: '', version: null, page: '' }
    const h = window.location.hash || ''

    // Login route first — distinct prefix, no ambiguity with other patterns.
    if (h.startsWith('#login')) {
        // Browser auth deep-link: `#login?t=<token>`. Hash-fragment query
        // (rather than a real URL query) keeps the token out of HTTP
        // server logs. We parse what comes after the optional `?`.
        const sep = h.indexOf('?')
        let token: string | undefined
        if (sep >= 0) {
            const params = new URLSearchParams(h.slice(sep + 1))
            token = params.get('t') ?? undefined
        }
        return { op: '', stack: '', version: null, page: 'login', loginToken: token }
    }

    // Traces routes — list view (#traces) or per-rid detail (#traces/<rid>).
    if (h === '#traces' || h === '#traces/') {
        return { op: '', stack: '', version: null, page: 'traces', traceRid: '' }
    }
    let tm = h.match(/^#traces\/(.+)$/)
    if (tm) {
        return { op: '', stack: '', version: null, page: 'traces', traceRid: tm[1] }
    }

    // Secrets routes — list view (#secrets) or per-name detail (#secrets/<name>).
    if (h === '#secrets' || h === '#secrets/') {
        return { op: '', stack: '', version: null, page: 'secrets', secretName: '' }
    }
    const sm = h.match(/^#secrets\/(.+)$/)
    if (sm) {
        return { op: '', stack: '', version: null, page: 'secrets', secretName: sm[1] }
    }

    // Op detail with optional version override.
    // Two forms: <stack>/<scope>/<name> or <stack>/v<N>/<scope>/<name>.
    let m = h.match(/^#ops\/(.+?)\/v(\d+)\/(\d+)\/(.+)$/)
    if (m) {
        return {
            op: `${m[1]}/${m[3]}/${m[4]}`,
            stack: m[1],
            version: Number(m[2]),
            page: '',
        }
    }
    m = h.match(/^#ops\/(.+?)\/(\d+)\/(.+)$/)
    if (m) {
        return { op: `${m[1]}/${m[2]}/${m[3]}`, stack: m[1], version: null, page: '' }
    }

    // Stack-level views: optional version pin or "versions" subpage.
    m = h.match(/^#stack\/(.+?)\/versions$/)
    if (m) return { op: '', stack: m[1], version: null, page: 'versions' }
    m = h.match(/^#stack\/(.+?)\/v(\d+)$/)
    if (m) return { op: '', stack: m[1], version: Number(m[2]), page: '' }
    m = h.match(/^#stack\/(.+)$/)
    if (m) return { op: '', stack: m[1], version: null, page: '' }

    return { op: '', stack: '', version: null, page: '' }
}

function writeHash(sel: Selection) {
    if (typeof window === 'undefined') return
    let next = ''
    if (sel.page === 'login') {
        // Don't carry the token across writes — it's a one-shot
        // credential, and readHash extracts it before this point.
        next = '#login'
    } else if (sel.page === 'traces') {
        next = sel.traceRid ? `#traces/${sel.traceRid}` : '#traces'
    } else if (sel.page === 'secrets') {
        next = sel.secretName ? `#secrets/${sel.secretName}` : '#secrets'
    } else if (sel.page === 'versions' && sel.stack) {
        next = `#stack/${sel.stack}/versions`
    } else if (sel.op) {
        // sel.op is "stack/scope/name". Splice in /v<N>/ when present.
        if (sel.version) {
            const parts = sel.op.split('/')
            // [stack, scope, ...nameParts]
            if (parts.length >= 3) {
                const stackPart = parts[0]
                const rest = parts.slice(1).join('/')
                next = `#ops/${stackPart}/v${sel.version}/${rest}`
            } else {
                next = `#ops/${sel.op}`
            }
        } else {
            next = `#ops/${sel.op}`
        }
    } else if (sel.stack) {
        next = sel.version ? `#stack/${sel.stack}/v${sel.version}` : `#stack/${sel.stack}`
    }
    if (window.location.hash !== next) {
        history.replaceState(null, '', next || window.location.pathname)
    }
}

export const store = createStore()
