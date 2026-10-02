<script lang="ts">
    import { getComputeSource, type ComputeSource } from '../lib/api'
    import { store } from '../lib/store.svelte'
    import CopyButton from './CopyButton.svelte'
    import Tabs from './Tabs.svelte'

    interface Props {
        // The stack whose COMPUTES/<digest>.json row the source is read
        // through, and the compute's wasm digest.
        stack: string
        digest: string
        onBack: () => void
    }
    let { stack, digest, onBack }: Props = $props()

    // CodeMirror + its JavaScript language load only when a source is shown.
    const JsCodeViewPromise = import('./JsCodeView.svelte')

    let source = $state<ComputeSource | null>(null)
    let loading = $state(true)
    let error = $state('')
    let activePath = $state('')

    $effect(() => {
        const s = stack
        const d = digest
        const tenant = store.state.currentTenant
        loading = true
        error = ''
        source = null
        ;(async () => {
            try {
                const got = await getComputeSource(tenant, s, d)
                if (s !== stack || d !== digest) return
                source = got
                activePath = got?.entry ?? ''
            } catch (e) {
                error = e instanceof Error ? e.message : String(e)
            } finally {
                loading = false
            }
        })()
    })

    // Entry first, then the files it imports.
    const paths = $derived(
        source ? [source.entry, ...source.files.map((f) => f.path).filter((p) => p !== source?.entry)] : []
    )
    const activeFile = $derived(source?.files.find((f) => f.path === activePath))
    const title = $derived(source ? source.entry : 'compute')
</script>

<div class="flex h-full flex-col p-4">
    <header class="mb-3 flex items-start gap-3">
        <button
            type="button"
            class="rounded border border-neutral-300 bg-white px-2 py-0.5 text-xs text-neutral-700 hover:bg-neutral-50"
            onclick={onBack}
        >
            ← back
        </button>
        <div class="min-w-0 flex-1">
            <h2 class="flex items-center gap-2 font-mono text-base font-semibold text-neutral-900">
                <span>{title}</span>
            </h2>
            <p class="flex flex-wrap items-center gap-x-2 text-xs text-neutral-500">
                <span class="font-mono" title={digest}>compute://sha256/{digest.slice(0, 12)}…</span>
                <CopyButton text={`compute://sha256/${digest}`} title="copy compute ref" class="!text-neutral-400 hover:!bg-neutral-200 hover:!text-neutral-700" />
                <span class="text-neutral-300">·</span>
                <button
                    type="button"
                    class="font-mono underline decoration-neutral-300 underline-offset-2 hover:text-neutral-900 hover:decoration-neutral-700"
                    onclick={() => store.selectStack(stack)}
                >{stack}</button>
                {#if source}
                    <span class="text-neutral-300">·</span>
                    <span>v{source.version}</span>
                {/if}
            </p>
        </div>
    </header>

    {#if loading}
        <p class="text-sm italic text-neutral-400">loading…</p>
    {:else if error}
        <p class="rounded border border-red-300 bg-red-50 p-2 text-xs text-red-800">{error}</p>
    {:else if !source}
        <div class="max-w-prose rounded border border-neutral-200 bg-white p-3 text-sm text-neutral-700">
            <p class="mb-2 font-medium text-neutral-900">No source is stored for this compute.</p>
            <ul class="list-disc space-y-1 pl-5 text-xs text-neutral-600">
                <li>It came from a prebuilt <code class="font-mono">.wasm</code> (a package), which has no source.</li>
                <li>The stack <span class="font-mono">{stack}</span> doesn't use this compute.</li>
            </ul>
        </div>
    {:else}
        {#if paths.length > 1}
            <div class="mb-3">
                <Tabs tabs={paths} active={activePath} onSelect={(p) => (activePath = p)} />
            </div>
        {/if}
        {#if activeFile}
            {#await JsCodeViewPromise}
                <div class="text-sm italic text-neutral-400">loading viewer…</div>
            {:then m}
                {@const JsCodeView = m.default}
                <JsCodeView value={activeFile.content} path={activeFile.path} />
            {/await}
        {/if}
    {/if}
</div>
