<script lang="ts">
    import { colorizer } from '../lib/colorizer'
    import { store } from '../lib/store.svelte'

    let signingOut = $state(false)

    async function onSignOut() {
        if (signingOut) return
        signingOut = true
        try {
            await store.signOut()
        } finally {
            signingOut = false
        }
    }

    // Three render shapes, decided here so the template stays flat:
    //   open_dev  — small amber badge, no sign-out
    //   browser   — profile chip (colorized actor_id) + sign-out
    //   other     — defensive minimal badge (signed/basic on a UI fetch
    //               shouldn't happen but we don't want to crash if it does)
    const session = $derived(store.state.session)
    const isOpenDev = $derived(session?.open_dev === true)
    const isBrowser = $derived(session?.source === 'browser')
    // Placeholder for the not-yet-fetched profile image: a solid-color
    // disc keyed on the actor_id. Same id → same color across sessions
    // and devices, so the user gets a consistent visual fingerprint.
    const actorColor = $derived(
        session?.actor_id ? colorizer(session.actor_id) : ''
    )
</script>

{#if isOpenDev}
    <span
        class="inline-flex items-center gap-1 rounded border border-amber-300 bg-amber-50 px-1.5 py-0.5 font-mono text-[10px] text-amber-800"
        title="The chassis is running in --auth-mode=both; the UI is not enforcing auth. Don't run a production chassis this way."
    >
        <span
            class="inline-block h-3 w-3 shrink-0 rounded-full bg-amber-600"
        ></span>

 open
    </span>
{:else if isBrowser}
    <span class="flex items-center gap-2 text-xs text-neutral-500">
        <span
            class="inline-block h-4 w-4 shrink-0 rounded-full"
            style="background-color: {actorColor};"
            title="signed in as {session?.actor_id ?? ''}"
            aria-label="signed in as {session?.actor_id ?? ''}"
        ></span>
        <button
            type="button"
            class="rounded border border-neutral-300 bg-white px-2 py-0.5 text-xs text-neutral-700 hover:bg-neutral-50 disabled:opacity-50"
            onclick={onSignOut}
            disabled={signingOut}
            title="sign out — revokes this session on the chassis"
        >
            {signingOut ? 'signing out…' : 'Sign out'}
        </button>
    </span>
{:else if session}
    <!-- signed / basic — shouldn't normally hit the UI but defensive. -->
    <span class="font-mono text-[10px] text-neutral-500">
        {session.source}
    </span>
{/if}
