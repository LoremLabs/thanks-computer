<script lang="ts">
    import Runner from './components/Runner.svelte'
    import Walkthrough from './components/Walkthrough.svelte'
    import { tracks } from './lib/tutorial'

    // Run lives in the header (primary CTA); it triggers the Runner's
    // exported run(). `running` is bound from the Runner so the button
    // reflects in-flight state. The URL-action button (Open + break-at)
    // lives in the Runner itself, next to the path field.
    let runner = $state<Runner | undefined>()
    let running = $state(false)

    // Guided walkthrough: App owns the track + step index and drives the
    // Runner (which loads + runs the step's ops). The Runner auto-runs
    // track 0 / step 0 on mount, so we only call load() on navigation.
    let trackIdx = $state(0)
    let stepIdx = $state(0)
    const steps = $derived(tracks[trackIdx].steps)

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
                disabled={running}
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
                 glyph + label). -->
            <nav class="flex flex-col gap-0.5 px-2 pt-3 pb-2">
                <a
                    href="/admin/#traces"
                    target="_blank"
                    rel="noopener"
                    title="open the admin's traces view in a new tab — open auth in demo mode, no login needed"
                    class="flex items-center gap-2 rounded px-2 py-1 text-sm text-neutral-700 hover:bg-neutral-100"
                >
                    <span class="inline-block w-4 shrink-0 text-center font-semibold tracking-tight text-brand-magenta" aria-hidden="true">o</span>
                    traces ↗
                </a>
            </nav>

            <!-- Divider between utility links and the walkthrough. -->
            <div class="mx-2 border-t border-neutral-200"></div>

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
        </aside>

        <main class="flex-1 overflow-auto px-6 py-6">
            <Runner bind:this={runner} bind:running />
        </main>
    </div>
</div>
