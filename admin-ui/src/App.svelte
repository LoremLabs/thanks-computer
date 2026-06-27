<script lang="ts">
    import { onMount } from 'svelte'
    import { ValidationFailedError } from './lib/api'
    import { isAuthed, requiresLogin, store } from './lib/store.svelte'
    import { opId, type Op } from './lib/types'
    import { groupOps } from './lib/tree'
    import Ago from './components/Ago.svelte'
    import Button from './components/Button.svelte'
    import Login from './components/Login.svelte'
    import OpDetail from './components/OpDetail.svelte'
    import StackNav from './components/StackNav.svelte'
    import SecretDetail from './components/SecretDetail.svelte'
    import SecretsList from './components/SecretsList.svelte'
    import SessionIndicator from './components/SessionIndicator.svelte'
    import SidebarNav from './components/SidebarNav.svelte'
    import StackView from './components/StackView.svelte'
    import TraceDetail from './components/TraceDetail.svelte'
    import TracesList from './components/TracesList.svelte'
    import VersionsList from './components/VersionsList.svelte'

    // DemoView is loaded on-demand: it owns ~50 KB of source (Runner +
    // Walkthrough + 411-line tutorial curriculum) that admin-only
    // sessions never need. The const + `{#await}` pattern fires the
    // import the first time the `showDemo` branch renders and never
    // again (browser-cached after the first hit). Vite emits this as a
    // separate chunk because it's the only dynamic import reaching
    // DemoView. Same pattern is used at three other lazy points
    // (TxclEditor, DiffPanel, demo CodeEditor) to split CodeMirror and
    // `diff` out of the main bundle.
    const DemoViewPromise = import('./views/DemoView.svelte')

    onMount(() => {
        // Auth-first boot: probe /auth/browser/session before any
        // tenant/stacks fetches. In open-dev mode the chassis returns
        // {open_dev: true} and we proceed normally; in signed mode
        // refreshSession sets session=null and the Login view takes
        // over without firing downstream fetches that would 401 and
        // noisily error in the console.
        ;(async () => {
            await store.refreshSession()
            // Probe `/v1/demo/info` once — its existence signals the
            // chassis is running under `txco demo`. Sets state.demoMode
            // so the App ladder + syncFromHash can branch on it before
            // we land the user on a route.
            await store.probeDemoMode()
            // First-visit landing for demo-mode chassis: if the URL has
            // no hash yet, send the user straight to #demo (which
            // syncFromHash picks up below). Subsequent visits whose
            // history already carries a hash are left alone.
            if (store.state.demoMode && !window.location.hash) {
                window.location.hash = 'demo'
            }
            // If the URL we landed on has #login?t=<token>, syncFromHash
            // picks it up and calls tryExchange. Trigger it here too
            // so the deep-link flow works on a clean reload (not just
            // hashchange).
            store.syncFromHash()
            // If syncFromHash kicked off a browser-auth token exchange
            // (#login?t=…), loginPending is set synchronously — defer the
            // initial data load to tryExchange so the view loads as the
            // POST-exchange identity. Without this an already-logged-in
            // browser would load the previous user's tenants/ops here and
            // `txco ui` as a different user would look like a no-op. The
            // load itself (tenants → ops + best-effort trace/secret warmups)
            // now lives in store.loadAuthedData, shared with tryExchange.
            if (!store.state.loginPending && isAuthed(store.state.session)) {
                await store.loadAuthedData()
            }
        })()
        const onHash = () => store.syncFromHash()
        window.addEventListener('hashchange', onHash)
        // Any 401 from a downstream fetch routes the user back to
        // login without losing what they were looking at. The store's
        // captureIntendedHash records the current URL so re-login
        // can return them there.
        const onSessionLost = () => {
            store.captureIntendedHash()
            store.refreshSession()
        }
        window.addEventListener('txco:session-lost', onSessionLost)
        return () => {
            window.removeEventListener('hashchange', onHash)
            window.removeEventListener('txco:session-lost', onSessionLost)
        }
    })

    const showLogin = $derived(
        requiresLogin(store.state.sessionLoaded, store.state.session)
    )

    const selected = $derived<Op | null>(
        store.state.ops.find((o) => opId(o) === store.state.selectedId) ?? null
    )

    // The first stack as the sidebar tree orders it (boot first, then
    // alphabetical) — the same groupOps the tree uses, so "first" here
    // matches what the user sees highlighted.
    const firstStack = $derived(groupOps(store.state.ops)[0]?.stack ?? '')

    // The Ops view must never render blank: with no stack/op/versions
    // selected the main panel falls back to an empty OpDetail, which
    // reads as "the service is broken". Whenever we're on Ops (not
    // traces) with nothing in focus and at least one stack exists,
    // pre-select the first stack so StackView renders and the tree
    // highlights it. Setting selectedStack makes the guard's own
    // condition false, so this can't loop.
    $effect(() => {
        if (
            !store.state.showTraces &&
            !store.state.showSecrets &&
            !store.state.showVersionsList &&
            !store.state.selectedStack &&
            !store.state.selectedId &&
            firstStack
        ) {
            store.selectStack(firstStack)
        }
    })

    // The stack currently "in focus" for header context — explicit
    // stack-view selection, the versions-list page's stack, or the
    // parent stack of a selected op.
    const focusedStack = $derived(
        store.state.showVersionsList ||
            store.state.selectedStack ||
            (selected ? selected.stack : '')
    )
    const focusedStackRow = $derived(
        focusedStack ? store.state.stacks.find((s) => s.name === focusedStack) : undefined
    )
    const focusedVersion = $derived(
        focusedStack ? store.state.currentVersionByStack[focusedStack] : undefined
    )
    const isActiveVersion = $derived(
        focusedStackRow &&
            typeof focusedVersion === 'number' &&
            focusedStackRow.active_version === focusedVersion
    )
    const focusedVersions = $derived(
        focusedStack ? store.state.versionsByStack[focusedStack] ?? [] : []
    )
    const focusedVersionRow = $derived(
        focusedVersion
            ? focusedVersions.find((v) => v.version_number === focusedVersion)
            : undefined
    )
    const focusedVersionStatus = $derived(
        focusedVersionRow?.status ?? (isActiveVersion ? 'active' : '')
    )
    const focusedIsDraft = $derived(focusedVersionStatus === 'draft')
    const focusedIsNonDraft = $derived(
        focusedVersionStatus !== '' && focusedVersionStatus !== 'draft'
    )

    let creatingDraft = $state(false)
    let createDraftError = $state('')
    async function onCreateDraft() {
        if (!focusedStack || creatingDraft) return
        creatingDraft = true
        createDraftError = ''
        try {
            await store.createDraftForStack(focusedStack)
        } catch (e) {
            createDraftError = e instanceof Error ? e.message : String(e)
        } finally {
            creatingDraft = false
        }
    }

    let activating = $state(false)
    let activateError = $state('')
    async function onActivateDraft() {
        if (!focusedStack || typeof focusedVersion !== 'number' || activating) return
        if (!confirm(`Activate ${focusedStack} v${focusedVersion}?`)) return
        activating = true
        activateError = ''
        try {
            await store.activateVersion(focusedStack, focusedVersion)
        } catch (e) {
            if (e instanceof ValidationFailedError) {
                const head = e.errors
                    .slice(0, 3)
                    .map((er) => `${er.path}: ${er.err}`)
                    .join('; ')
                activateError = `validation failed (${e.errors.length}): ${head}`
            } else {
                activateError = e instanceof Error ? e.message : String(e)
            }
        } finally {
            activating = false
        }
    }

    // Lazy-load version history whenever a new stack comes into focus,
    // so the header label can show the right status ("active", "draft",
    // …) for deep-links and version pins. Create-draft / Activate /
    // re-activate-an-older-version all live on the history page now.
    $effect(() => {
        if (focusedStack && !store.state.versionsByStack[focusedStack]) {
            store.refreshVersions(focusedStack)
        }
    })

    // Slide-over sidebar state for narrow viewports. Desktop layout
    // doesn't observe this — the md+ sidebar is statically visible.
    // Local component state is plenty; nothing else reads this.
    let sidebarOpen = $state(false)

    function selectOpAndClose(op: Op) {
        store.selectOp(op)
        sidebarOpen = false
    }
    function selectStackAndClose(s: string) {
        store.selectStack(s)
        sidebarOpen = false
    }

    // Sidebar nav: top-level "ops" vs "traces". Clicking ops returns
    // to the default (no traces); clicking traces opens the list page.
    const currentNav = $derived<'ops' | 'traces' | 'secrets'>(
        store.state.showSecrets
            ? 'secrets'
            : store.state.showTraces
              ? 'traces'
              : 'ops'
    )
    function selectNav(view: 'ops' | 'traces' | 'secrets') {
        if (view === 'traces') {
            store.showTraces()
        } else if (view === 'secrets') {
            store.showSecrets()
        } else {
            // Returning to Ops: land on the first stack if there is
            // one (StackView, tree highlights it) so the panel is
            // never blank; the guard $effect keeps it that way and
            // also covers the post-login initial load. Fall back to
            // clearing the op selection when there are no stacks yet.
            if (firstStack) {
                store.selectStack(firstStack)
            } else {
                store.selectOp(null)
            }
        }
        sidebarOpen = false
    }

    // Stack-scoped header chrome (version chip, Clone/Activate, etc.)
    // only makes sense when a stack is actually in focus. Traces and
    // the login view both have no stack context.
    const onTracesView = $derived(!!store.state.showTraces)
    const onSecretsView = $derived(!!store.state.showSecrets)
</script>

{#if showLogin}
    <Login />
{:else if store.state.showDemo}
    <!-- Demo route: renders today's standalone demo-ui inside the admin
         SPA shell. No admin header, no admin sidebar — DemoView brings
         its own. Activated when the user clicks #demo in their history
         OR when probeDemoMode set state.demoMode=true and onMount
         redirected the empty hash. Lazy-loaded — see DemoViewPromise. -->
    {#await DemoViewPromise}
        <div class="flex h-screen items-center justify-center text-sm italic text-neutral-400">
            loading demo…
        </div>
    {:then m}
        {@const DemoView = m.default}
        <DemoView />
    {:catch err}
        <div class="flex h-screen items-center justify-center p-4 text-sm text-red-600">
            failed to load demo: {err instanceof Error ? err.message : String(err)}
        </div>
    {/await}
{:else}
<div class="flex h-screen flex-col bg-neutral-50 text-neutral-900">
    <header class="flex shrink-0 items-center gap-2 border-b border-neutral-200 bg-white px-2 py-2 text-sm sm:gap-4 sm:px-4">
        <button
            type="button"
            class="inline-flex h-8 w-8 shrink-0 items-center justify-center rounded text-neutral-600 hover:bg-neutral-100 md:hidden"
            aria-label="toggle sidebar"
            onclick={() => (sidebarOpen = !sidebarOpen)}
        >
            ☰
        </button>
        <div class="flex items-center gap-2">
            <span class="font-semibold tracking-tight">thanks, c<span class="text-brand-cyan">o</span><span class="text-brand-magenta">o</span><span class="text-brand-yellow">o</span>mputer.</span>
        </div>
        {#if store.state.tenants.length}
            <label class="flex items-center gap-1.5 text-xs text-neutral-500">
                <select
                    class="rounded border border-neutral-300 bg-white px-1.5 py-0.5 font-mono text-xs text-neutral-800"
                    value={store.state.currentTenant}
                    onchange={(e) => store.setTenant((e.currentTarget as HTMLSelectElement).value)}
                >
                    {#each store.state.tenants as t (t.slug)}
                        <option value={t.slug}>{t.slug}</option>
                    {/each}
                </select>
            </label>
        {/if}
        {#if focusedStack && focusedVersion && !onTracesView && !onSecretsView}
            <span class="hidden items-center gap-4 text-xs text-neutral-500 sm:flex">
                <button
                    type="button"
                    class="font-mono text-xs text-neutral-700 underline decoration-neutral-400 underline-offset-2 hover:text-neutral-900 hover:decoration-neutral-700 cursor-pointer"
                    title="open version history for {focusedStack} — create a draft, activate, or roll back to any prior version"
                    onclick={() => store.showVersions(focusedStack)}
                >
                    v{focusedVersion}
                </button>
                {#if focusedIsNonDraft}
                    <button
                        type="button"
                        class="rounded border border-brand-cyan/40 bg-brand-cyan/10 px-2 py-0.5 text-xs text-neutral-900 hover:bg-brand-cyan/20 disabled:opacity-50"
                        title="clone the active version into a new draft you can edit"
                        disabled={creatingDraft}
                        onclick={onCreateDraft}
                    >
                        {creatingDraft ? 'cloning…' : 'Clone'}
                    </button>
                {/if}
                {#if focusedIsDraft}
                    <button
                        type="button"
                        class="rounded border border-brand-red/50 bg-brand-red/10 px-2 py-0.5 text-xs text-neutral-900 hover:bg-brand-red/20 disabled:opacity-50"
                        title="validate this draft and make it the active version"
                        disabled={activating}
                        onclick={onActivateDraft}
                    >
                        {activating ? 'activating…' : 'Activate'}
                    </button>
                {/if}
            </span>
        {/if}
        {#if createDraftError}
            <span class="hidden text-xs text-red-600 sm:inline" title={createDraftError}>
                draft failed: {createDraftError}
            </span>
        {/if}
        {#if activateError}
            <span class="hidden text-xs text-red-600 sm:inline" title={activateError}>
                activate failed: {activateError}
            </span>
        {/if}
        <span class="ml-auto text-xs text-neutral-500">
            {#if store.state.loading}
                loading…
            {:else if store.state.error}
                <span class="text-red-600">error: {store.state.error}</span>
            {:else}
                {#if store.state.loadedAt}
                    <span class="hidden sm:inline font-mono text-xs text-neutral-400" title={store.state.loadedAt.toISOString()}>
                        <Ago at={store.state.loadedAt} />
                    </span>
                {/if}
            {/if}
        </span>
        <Button
            variant="icon"
            title="reload"
            onclick={() => {
                store.refresh()
                store.refreshLastDurations()
            }}
        >↻</Button>
        <SessionIndicator />
    </header>

    <main class="relative flex flex-1 overflow-hidden">
        <!-- Desktop sidebar: in-flow at md+ only. -->
        <aside class="hidden w-72 shrink-0 overflow-y-auto border-r border-neutral-200 bg-white md:block">
            <SidebarNav current={currentNav} onSelect={selectNav} showDemo={store.state.demoMode} />
            <StackNav
                ops={store.state.ops}
                selectedId={store.state.selectedId}
                selectedStack={store.state.selectedStack}
                onSelectOp={selectOpAndClose}
                onSelectStack={selectStackAndClose}
            />
        </aside>

        <!-- Mobile slide-over: hidden at md+. Backdrop + panel are
             both inside <main>, so the layout never reflows when the
             panel opens or closes — it overlays on top instead. -->
        {#if sidebarOpen}
            <div
                class="fixed inset-0 z-20 bg-black/40 md:hidden"
                onclick={() => (sidebarOpen = false)}
                role="presentation"
            ></div>
        {/if}
        <aside
            class="fixed inset-y-0 left-0 z-30 w-72 transform overflow-y-auto border-r border-neutral-200 bg-white shadow-lg transition-transform duration-200 md:hidden {sidebarOpen
                ? 'translate-x-0'
                : '-translate-x-full'}"
            aria-hidden={!sidebarOpen}
        >
            <SidebarNav current={currentNav} onSelect={selectNav} showDemo={store.state.demoMode} />
            <StackNav
                ops={store.state.ops}
                selectedId={store.state.selectedId}
                selectedStack={store.state.selectedStack}
                onSelectOp={selectOpAndClose}
                onSelectStack={selectStackAndClose}
            />
        </aside>
        <section class="flex-1 overflow-auto">
            {#if store.state.showSecrets === '__list__'}
                <SecretsList onSelectSecret={(name) => store.showSecrets(name)} />
            {:else if store.state.showSecrets}
                <SecretDetail
                    name={store.state.showSecrets}
                    onBack={() => store.showSecrets()}
                />
            {:else if store.state.showTraces === '__list__'}
                <TracesList onSelectTrace={(rid) => store.showTraces(rid)} />
            {:else if store.state.showTraces}
                <TraceDetail
                    rid={store.state.showTraces}
                    onBack={() => store.showTraces()}
                />
            {:else if store.state.showVersionsList}
                <VersionsList
                    stack={store.state.showVersionsList}
                    versions={store.state.versionsByStack[store.state.showVersionsList] ?? []}
                    onSelectVersion={(n) => {
                        store.setStackVersion(store.state.showVersionsList, n)
                        store.selectStack(store.state.showVersionsList)
                    }}
                    onActivate={(n) => store.activateVersion(store.state.showVersionsList, n)}
                    onCreateDraft={() => store.createDraftForStack(store.state.showVersionsList)}
                />
            {:else if store.state.selectedStack}
                <StackView
                    stack={store.state.selectedStack}
                    ops={store.state.ops}
                    lastDurations={store.state.lastDurations}
                    stackTotalMs={store.state.stackLastDurations[
                        store.state.selectedStack
                    ]}
                    version={store.state.currentVersionByStack[store.state.selectedStack]}
                    isDraft={focusedIsDraft}
                    onSelectOp={(op) => store.selectOp(op)}
                />
            {:else}
                <div class="p-4 h-full">
                    <OpDetail
                        op={selected}
                        version={selected
                            ? store.state.currentVersionByStack[selected.stack]
                            : undefined}
                        isDraft={focusedIsDraft}
                        lastInput={selected
                            ? store.state.lastInputs[
                                  store.state.selectedId
                              ]
                            : undefined}
                        lastOutput={selected
                            ? store.state.lastOutputs[
                                  store.state.selectedId
                              ]
                            : undefined}
                    />
                </div>
            {/if}
        </section>
    </main>
</div>
{/if}
