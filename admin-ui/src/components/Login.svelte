<script lang="ts">
    import { store } from '../lib/store.svelte'

    let pasteValue = $state('')
    let pasteError = $state('')

    // The CLI's `txco auth login` prints either a full URL
    // (https://chassis/admin/#login?t=...) or — when --no-open is set —
    // expects the user to paste it into a browser. We accept either
    // the URL or the bare token. The regex pulls `?t=...` or `&t=...`
    // out of any URL shape; otherwise the input is treated as the
    // token itself if it has the expected prefix.
    function extractToken(input: string): string {
        const trimmed = input.trim()
        if (!trimmed) return ''
        const match = trimmed.match(/[?&]t=([^&\s]+)/)
        if (match) return decodeURIComponent(match[1])
        // Bare token paste — accept anything that looks like a chassis
        // bootstrap token. The server will reject malformed input
        // anyway, so we don't gatekeep too aggressively here.
        if (trimmed.startsWith('btk_')) return trimmed
        return ''
    }

    async function onSubmit(e: Event) {
        e.preventDefault()
        pasteError = ''
        const token = extractToken(pasteValue)
        if (!token) {
            pasteError =
                "couldn't find a token in that. Paste the full URL printed by `txco auth login`, or just the btk_… token."
            return
        }
        await store.tryExchange(token)
    }

    // chassisHost mirrors the value App.svelte shows in the header
    // when authed, so the example command on the login page points
    // at the same chassis the browser is talking to.
    const chassisHost = $derived(
        typeof window !== 'undefined' ? window.location.host : ''
    )
    const chassisURL = $derived(
        typeof window !== 'undefined'
            ? `${window.location.protocol}//${chassisHost}`
            : ''
    )
</script>

<div class="flex h-screen items-center justify-center bg-neutral-50 px-4 text-neutral-900">
    <div class="w-full max-w-xl rounded-lg border border-neutral-200 bg-white p-8 shadow-sm">
        <div class="mb-6 flex items-center gap-2">
            <span class="text-xl font-semibold tracking-tight">
                thanks, c<span class="text-brand-cyan">o</span><span class="text-brand-magenta">o</span><span class="text-brand-yellow">o</span>mputer.
            </span>
        </div>

        {#if store.state.loginPending}
            <!-- Auto-exchange in flight from #login?t=<token>. The
                 spinner is short-lived (a single network round-trip),
                 but worth surfacing so an interrupted exchange isn't
                 mistaken for a hung UI. -->
            <div class="flex items-center gap-3 py-6">
                <div
                    class="h-5 w-5 animate-spin rounded-full border-2 border-brand-cyan border-t-transparent"
                ></div>
                <span class="text-sm text-neutral-600">Signing you in…</span>
            </div>
        {:else}
            <h1 class="mb-2 text-lg font-medium">Sign in</h1>
            <p class="mb-4 text-sm text-neutral-600">
                This chassis requires authentication. Run
                <code class="rounded bg-neutral-100 px-1.5 py-0.5 font-mono text-xs"
                    >txco auth login{chassisURL ? ` --url ${chassisURL}` : ''}</code
                >
                on the CLI machine. Your browser opens here automatically, or
                you can paste the resulting URL below.
            </p>

            <form onsubmit={onSubmit} class="space-y-3">
                <label class="block text-sm font-medium text-neutral-700" for="login-paste">
                    Paste login URL or token
                </label>
                <textarea
                    id="login-paste"
                    bind:value={pasteValue}
                    placeholder="https://{chassisHost}/admin/#login?t=btk_…"
                    rows="3"
                    class="block w-full rounded border border-neutral-300 bg-white px-2 py-1.5 font-mono text-xs text-neutral-800 focus:border-brand-cyan focus:outline-none focus:ring-1 focus:ring-brand-cyan"
                ></textarea>
                {#if pasteError}
                    <p class="text-xs text-red-600">{pasteError}</p>
                {/if}
                {#if store.state.loginError}
                    <p class="text-xs text-red-600">{store.state.loginError}</p>
                {/if}
                <div class="flex justify-end">
                    <button
                        type="submit"
                        class="inline-flex items-center gap-1.5 rounded border border-brand-cyan/40 bg-brand-cyan/10 px-3 py-1.5 text-sm font-medium text-neutral-900 hover:bg-brand-cyan/20 disabled:opacity-50"
                        disabled={!pasteValue.trim() || store.state.loginPending}
                    >
                        Sign in
                    </button>
                </div>
            </form>

            <details class="mt-6">
                <summary
                    class="cursor-pointer text-xs text-neutral-500 hover:text-neutral-700"
                >
                    Don't have a CLI handy?
                </summary>
                <div class="mt-2 space-y-2 text-xs text-neutral-600">
                    <p>
                        The chassis only accepts browser sessions derived from
                        a signed CLI request. If you don't have <code
                            class="rounded bg-neutral-100 px-1 py-0.5 font-mono">txco</code
                        > installed, ask someone with admin access to run
                        <code class="rounded bg-neutral-100 px-1 py-0.5 font-mono"
                            >txco auth invite</code
                        > and share the invitation token with you — then
                        <code class="rounded bg-neutral-100 px-1 py-0.5 font-mono"
                            >txco auth accept</code
                        > enrols your own key and
                        <code class="rounded bg-neutral-100 px-1 py-0.5 font-mono"
                            >txco auth login</code
                        > gets you here.
                    </p>
                </div>
            </details>
        {/if}
    </div>
</div>
