<script lang="ts">
    import type { SecretMetadata } from '../lib/api'
    import { hasCapability, store } from '../lib/store.svelte'
    import Ago from './Ago.svelte'
    import Button from './Button.svelte'
    import SetSecretValueForm from './SetSecretValueForm.svelte'

    interface Props {
        name: string
        onBack: () => void
    }
    let { name, onBack }: Props = $props()

    // Deep-link safety: a hard reload onto #secrets/<name> can land
    // here before the list loaded — warm it.
    $effect(() => {
        if (!store.state.secretsLoaded) store.refreshSecrets()
    })

    // v1 routes by name (tenant-wide names are unique). When stack
    // scoping lands (v2.a) the hash will carry scope too; for now match
    // the first row with this name, preferring tenant-wide.
    const secret = $derived.by<SecretMetadata | null>(() => {
        const matches = store.state.secrets.filter((s) => s.name === name)
        if (matches.length === 0) return null
        return matches.find((s) => !s.stack) ?? matches[0]
    })

    const canWrite = $derived(hasCapability(store.state.session, 'secret:*:write'))

    function scopeLabel(s: SecretMetadata): string {
        return s.stack && s.stack !== '' ? s.stack : 'tenant-wide'
    }

    // --- edit description ---
    let editingDesc = $state(false)
    let descDraft = $state('')
    let descSaving = $state(false)
    let descError = $state('')

    function startEditDesc() {
        descDraft = secret?.description ?? ''
        descError = ''
        editingDesc = true
    }
    async function saveDesc() {
        if (descSaving) return
        descSaving = true
        descError = ''
        try {
            await store.updateSecretDescription(name, descDraft)
            editingDesc = false
        } catch (e) {
            descError = e instanceof Error ? e.message : String(e)
        } finally {
            descSaving = false
        }
    }

    // --- rotate (with operator-supplied value) ---
    let showRotate = $state(false)
    let rotateConfirm = $state('')
    const rotateArmed = $derived(rotateConfirm === name)
    async function doRotate(value: string) {
        // Throws propagate to SetSecretValueForm's inline error and keep
        // the panel open for a retry.
        await store.rotateSecret(name, value)
        showRotate = false
        rotateConfirm = ''
    }

    // --- revoke ---
    let showRevoke = $state(false)
    let revokeConfirm = $state('')
    let revoking = $state(false)
    let revokeError = $state('')
    const revokeArmed = $derived(revokeConfirm === name)
    async function doRevoke() {
        if (!revokeArmed || revoking) return
        revoking = true
        revokeError = ''
        try {
            await store.revokeSecret(name)
            onBack() // secret is gone — back to the list
        } catch (e) {
            revokeError = e instanceof Error ? e.message : String(e)
            revoking = false
        }
    }
</script>

<div class="flex h-full flex-col overflow-auto p-4">
    <header class="mb-4 flex items-center gap-3">
        <Button variant="icon" title="back to secrets" onclick={onBack}>←</Button>
        <div>
            <h2 class="font-mono text-base font-semibold text-neutral-900">{name}</h2>
            <p class="text-xs text-neutral-500">
                secret metadata — the value is never shown.
            </p>
        </div>
    </header>

    {#if !store.state.secretsLoaded}
        <p class="text-sm italic text-neutral-400">loading…</p>
    {:else if !secret}
        <p class="rounded border border-neutral-200 bg-white p-4 text-sm italic text-neutral-400">
            no secret named
            <span class="font-mono not-italic text-neutral-600">{name}</span>
            in this tenant — it may have been revoked.
        </p>
    {:else}
        <dl
            class="grid max-w-xl grid-cols-[10rem_1fr] gap-x-4 gap-y-2 rounded border border-neutral-200 bg-white p-4 text-sm"
        >
            <dt class="text-neutral-500">scope</dt>
            <dd class="font-mono text-neutral-800">{scopeLabel(secret)}</dd>

            <dt class="text-neutral-500">description</dt>
            <dd class="text-neutral-800">
                {#if editingDesc}
                    <div class="flex flex-col gap-1">
                        <input
                            type="text"
                            bind:value={descDraft}
                            placeholder="what this is for"
                            class="rounded border border-neutral-300 bg-white px-2 py-1 text-sm text-neutral-800 placeholder:text-neutral-400 focus:border-brand-cyan focus:outline-none"
                        />
                        {#if descError}
                            <p class="text-xs text-red-700">{descError}</p>
                        {/if}
                        <div class="flex gap-2">
                            <button
                                type="button"
                                onclick={saveDesc}
                                disabled={descSaving}
                                class="rounded border border-brand-cyan/50 bg-brand-cyan/10 px-2 py-0.5 text-xs text-neutral-900 hover:bg-brand-cyan/20 disabled:opacity-50"
                            >
                                {descSaving ? 'saving…' : 'save'}
                            </button>
                            <button
                                type="button"
                                onclick={() => (editingDesc = false)}
                                class="px-2 py-0.5 text-xs text-neutral-500 hover:text-neutral-700"
                            >
                                cancel
                            </button>
                        </div>
                    </div>
                {:else}
                    <span>{secret.description || '—'}</span>
                    {#if canWrite}
                        <button
                            type="button"
                            onclick={startEditDesc}
                            class="ml-2 text-xs text-brand-cyan hover:underline"
                        >
                            edit
                        </button>
                    {/if}
                {/if}
            </dd>

            <dt class="text-neutral-500">key version</dt>
            <dd class="font-mono text-neutral-800">v{secret.key_version}</dd>

            <dt class="text-neutral-500">current version</dt>
            <dd class="font-mono text-neutral-800">{secret.version_no}</dd>

            <dt class="text-neutral-500">created</dt>
            <dd class="font-mono text-neutral-800"><Ago at={secret.created_at} /></dd>

            {#if secret.created_by}
                <dt class="text-neutral-500">created by</dt>
                <dd class="font-mono text-neutral-800">{secret.created_by}</dd>
            {/if}

            <dt class="text-neutral-500">last rotated</dt>
            <dd class="font-mono text-neutral-800">
                {#if secret.last_rotated_at}<Ago at={secret.last_rotated_at} />{:else}never{/if}
            </dd>

            <dt class="text-neutral-500">secret id</dt>
            <dd class="font-mono text-xs text-neutral-500">{secret.secret_id}</dd>
        </dl>

        {#if canWrite}
            <section class="mt-6 max-w-xl">
                <h3 class="mb-2 text-xs font-semibold uppercase tracking-wide text-neutral-500">
                    actions
                </h3>
                <div class="rounded border border-neutral-200 bg-white">
                    <!-- rotate -->
                    <div class="border-b border-neutral-100 p-3">
                        {#if !showRotate}
                            <button
                                type="button"
                                onclick={() => {
                                    showRotate = true
                                    rotateConfirm = ''
                                }}
                                class="rounded border border-neutral-300 bg-white px-2 py-0.5 text-sm text-neutral-700 hover:bg-neutral-50"
                            >
                                rotate value…
                            </button>
                            <p class="mt-1 text-xs text-neutral-400">
                                replace the stored value with a new one you supply.
                            </p>
                        {:else}
                            <p class="mb-2 text-xs text-neutral-600">
                                rotating replaces the value immediately. any op handler
                                still using the old value will fail until you redeploy
                                with the new one.
                            </p>
                            <label class="mb-2 flex flex-col gap-1 text-xs text-neutral-500">
                                type <span class="font-mono text-neutral-700">{name}</span> to
                                confirm
                                <input
                                    type="text"
                                    bind:value={rotateConfirm}
                                    spellcheck="false"
                                    class="rounded border border-neutral-300 bg-white px-2 py-1 font-mono text-sm text-neutral-800 focus:border-brand-cyan focus:outline-none"
                                />
                            </label>
                            <SetSecretValueForm
                                onSubmit={doRotate}
                                submitLabel="rotate value"
                                disabled={!rotateArmed}
                                valueLabel="new value"
                                placeholder="paste the new value"
                            />
                            <button
                                type="button"
                                onclick={() => (showRotate = false)}
                                class="mt-2 text-xs text-neutral-500 hover:text-neutral-700"
                            >
                                cancel
                            </button>
                        {/if}
                    </div>

                    <!-- revoke -->
                    <div class="p-3">
                        {#if !showRevoke}
                            <button
                                type="button"
                                onclick={() => {
                                    showRevoke = true
                                    revokeConfirm = ''
                                    revokeError = ''
                                }}
                                class="rounded border border-red-300 bg-white px-2 py-0.5 text-sm text-red-700 hover:bg-red-50"
                            >
                                revoke secret…
                            </button>
                            <p class="mt-1 text-xs text-neutral-400">
                                soft-delete this secret. ops using it will fail until updated.
                            </p>
                        {:else}
                            <p class="mb-2 text-xs text-red-700">
                                revoking removes this secret. this can't be undone — you'd
                                re-create it from scratch.
                            </p>
                            <label class="mb-2 flex flex-col gap-1 text-xs text-neutral-500">
                                type <span class="font-mono text-neutral-700">{name}</span> to
                                confirm
                                <input
                                    type="text"
                                    bind:value={revokeConfirm}
                                    spellcheck="false"
                                    class="rounded border border-neutral-300 bg-white px-2 py-1 font-mono text-sm text-neutral-800 focus:border-brand-cyan focus:outline-none"
                                />
                            </label>
                            {#if revokeError}
                                <p class="mb-2 text-xs text-red-700">{revokeError}</p>
                            {/if}
                            <div class="flex gap-2">
                                <button
                                    type="button"
                                    onclick={doRevoke}
                                    disabled={!revokeArmed || revoking}
                                    class="rounded border border-red-400 bg-red-50 px-2 py-0.5 text-sm text-red-800 hover:bg-red-100 disabled:cursor-not-allowed disabled:opacity-50"
                                >
                                    {revoking ? 'revoking…' : 'revoke'}
                                </button>
                                <button
                                    type="button"
                                    onclick={() => (showRevoke = false)}
                                    class="px-2 py-0.5 text-xs text-neutral-500 hover:text-neutral-700"
                                >
                                    cancel
                                </button>
                            </div>
                        {/if}
                    </div>
                </div>
            </section>
        {:else}
            <p class="mt-4 max-w-xl text-xs text-neutral-400">
                your role can read secrets but not modify them. write actions are
                gated on the
                <span class="font-mono text-neutral-600">secret:*:write</span>
                capability.
            </p>
        {/if}
    {/if}
</div>
