<script lang="ts">
    import { onMount } from 'svelte'
    import Runner from '../components/demo/Runner.svelte'
    import Walkthrough from '../components/demo/Walkthrough.svelte'
    import { getCurriculum, type Curriculum } from '../lib/api'

    // Run lives in the demo header (primary CTA); it triggers the Runner's
    // exported run(). `running` is bound from the Runner so the button
    // reflects in-flight state. The URL-action button (Open + break-at)
    // lives in the Runner itself, next to the path field.
    //
    // This view renders its OWN header + sidebar (no admin chrome) — see
    // App.svelte's `{#if store.state.page !== 'demo'}` gate around the
    // shared header/aside that suppresses admin chrome on this route. The
    // visual result matches the standalone demo-ui App.svelte byte-for-byte.
    let runner = $state<Runner | undefined>()
    let running = $state(false)

    // The walkthrough curriculum (tracks, steps, ops, request shape)
    // lives on the chassis at /v1/demo/curriculum — single source of
    // truth shared with the Go-side pre-seed (chassis/cli/demo →
    // demo.Seed). Fetch on mount; gate the Runner + Walkthrough on it
    // being loaded so neither has to defend against null data.
    let curriculum = $state<Curriculum | null>(null)
    let loadError = $state('')
    onMount(async () => {
        try {
            curriculum = await getCurriculum()
        } catch (e) {
            loadError = e instanceof Error ? e.message : String(e)
        }
    })

    // Guided walkthrough: this view owns the track + step index and drives
    // the Runner (which loads + runs the step's ops). The Runner auto-runs
    // track 0 / step 0 on mount, so we only call load() on navigation.
    let trackIdx = $state(0)
    let stepIdx = $state(0)
    const tracks = $derived(curriculum?.tracks ?? [])
    const steps = $derived(tracks[trackIdx]?.steps ?? [])

    function goTo(i: number) {
        if (i < 0 || i >= steps.length) return
        stepIdx = i
        runner?.load(steps[i])
    }
    function selectTrack(i: number) {
        if (i < 0 || i >= tracks.length || i === trackIdx) return
        trackIdx = i
        stepIdx = 0
        runner?.load(tracks[i].steps[0])
    }
</script>

<div class="flex h-full flex-col bg-neutral-50 text-neutral-900">
    <!-- Header: brand + primary CTA. Admin / utility links live in the
         sidebar below (so the sidebar can grow with more links over time
         without crowding the header). -->
    <header class="flex shrink-0 items-center justify-between gap-4 border-b border-neutral-200 bg-white px-6 py-3">
        <div>
            <h1 class="flex items-baseline gap-2 text-sm text-neutral-900">
                <span class="font-semibold tracking-tight">thanks, c<span class="text-brand-cyan">o</span><span class="text-brand-magenta">o</span><span class="text-brand-yellow">o</span>mputer.</span>
            </h1>
            <p class="text-xs text-neutral-500">demo</p>
        </div>
        <div class="flex shrink-0 items-center gap-3">
            <button
                type="button"
                disabled={running || !curriculum}
                onclick={() => runner?.run()}
                class="rounded bg-neutral-900 px-4 py-2 text-sm font-semibold text-white hover:bg-neutral-800 disabled:opacity-50"
            >
                {running ? 'running…' : 'Run ▶'}
            </button>
        </div>
    </header>

    <!-- Body: sidebar (utility links + walkthrough) + main (Runner).
         Mirrors admin-ui's App.svelte shell shape so the two surfaces
         feel of-a-piece. w-56 is a touch narrower than admin's w-72
         because the Runner panels eat horizontal space. -->
    <div class="flex flex-1 overflow-hidden">
        <aside class="flex w-56 shrink-0 flex-col overflow-y-auto border-r border-neutral-200 bg-white">
            <!-- Top section: utility / external links. Grows over time —
                 same row vocabulary as admin-ui's SidebarNav (colored `o`
                 glyph + label). `#traces` is a same-SPA navigation now
                 (was: opens in a new tab via /admin/#traces); the admin
                 chrome shows, demo chrome disappears, browser back returns. -->
            <nav class="flex flex-col gap-0.5 px-2 pt-3 pb-2">
                <a
                    href="#traces"
                    title="navigate to the admin's traces view"
                    class="flex items-center gap-2 rounded px-2 py-1 text-sm text-neutral-700 hover:bg-neutral-100"
                >
                    <span class="inline-block w-4 shrink-0 text-center font-semibold tracking-tight text-brand-magenta" aria-hidden="true">o</span>
                    traces
                </a>
            </nav>

            <!-- Divider between utility links and the walkthrough. -->
            <div class="mx-2 border-t border-neutral-200"></div>

            {#if curriculum && steps[stepIdx]}
                <Walkthrough
                    tracks={tracks.map((t) => t.title)}
                    trackIndex={trackIdx}
                    onSelectTrack={selectTrack}
                    stepTitles={steps.map((s) => s.title)}
                    step={steps[stepIdx]}
                    index={stepIdx}
                    total={steps.length}
                    onSelectStep={goTo}
                    onPrev={() => goTo(stepIdx - 1)}
                    onNext={() => goTo(stepIdx + 1)}
                />
            {/if}
        </aside>

        <main class="flex-1 overflow-auto px-6 py-6">
            {#if loadError}
                <div class="rounded border border-red-300 bg-red-50 p-4 text-sm text-red-800">
                    Failed to load demo curriculum: {loadError}
                </div>
            {:else if curriculum}
                <Runner
                    bind:this={runner}
                    bind:running
                    tracks={curriculum.tracks}
                    hostSuffix={curriculum.host_suffix}
                />
            {:else}
                <div class="text-sm italic text-neutral-400">loading walkthrough…</div>
            {/if}
        </main>
    </div>
</div>
