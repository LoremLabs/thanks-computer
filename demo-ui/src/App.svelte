<script lang="ts">
    import Runner from './components/Runner.svelte'
    import Walkthrough from './components/Walkthrough.svelte'
    import { tracks } from './lib/tutorial'

    // Run lives in the header (primary CTA); it triggers the Runner's
    // exported run(). `running` is bound from the Runner so the button
    // reflects in-flight state. `urlCopied` is bound the same way so the
    // Copy URL button can show a 1.5s "copied!" pulse after click.
    let runner = $state<Runner | undefined>()
    let running = $state(false)
    let urlCopied = $state(false)

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
    <header class="flex items-center justify-between gap-4 border-b border-neutral-200 bg-white px-6 py-3">
        <div>
            <h1 class="font-mono text-lg font-semibold text-neutral-900">
                txco <span class="text-brand-cyan">demo</span>
            </h1>
            <p class="text-xs text-neutral-500">
                Write a txcl op, define a request, and run it against a scratch stack.
            </p>
        </div>
        <div class="flex shrink-0 items-center gap-2">
            <button
                type="button"
                onclick={() => runner?.copyUrl()}
                title="copy the test URL — paste into a browser, curl, or anything that speaks HTTP"
                class="rounded border border-neutral-300 bg-white px-3 py-2 text-sm text-neutral-700 hover:bg-neutral-50"
            >
                {urlCopied ? 'copied!' : 'copy URL'}
            </button>
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

    <Walkthrough
        tracks={tracks.map((t) => t.title)}
        trackIndex={trackIdx}
        onSelectTrack={selectTrack}
        step={steps[stepIdx]}
        index={stepIdx}
        total={steps.length}
        onPrev={() => goTo(stepIdx - 1)}
        onNext={() => goTo(stepIdx + 1)}
    />

    <main class="flex-1 overflow-auto px-6 py-6">
        <Runner bind:this={runner} bind:running bind:urlCopied />
    </main>
</div>
