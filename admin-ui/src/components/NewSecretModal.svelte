<script lang="ts">
    import { store } from '../lib/store.svelte'
    import SetSecretValueForm from './SetSecretValueForm.svelte'

    interface Props {
        onClose: () => void
    }
    let { onClose }: Props = $props()

    // Name mirrors the server's shape rule: start with a letter, then
    // letters/digits/underscore. Case is NOT enforced — UPPER_SNAKE is
    // conventional but not required, so we store whatever the user types
    // verbatim.
    let name = $state('')
    let description = $state('')

    const NAME_RE = /^[A-Za-z][A-Za-z0-9_]*$/
    const nameValid = $derived(NAME_RE.test(name))
    const nameError = $derived(
        name !== '' && !nameValid
            ? 'start with a letter, then letters, digits, underscores'
            : ''
    )

    async function submit(value: string) {
        // Throws propagate to SetSecretValueForm's inline error (e.g.
        // secret_exists) and keep the modal open for a retry.
        await store.createSecret(name, value, description)
        onClose()
    }
</script>

<div
    class="fixed inset-0 z-40 flex items-center justify-center bg-black/40 p-4"
    onclick={(e) => {
        if (e.target === e.currentTarget) onClose()
    }}
    role="presentation"
>
    <div
        class="w-full max-w-md rounded-lg border border-neutral-200 bg-white p-5 shadow-xl"
        role="dialog"
        aria-modal="true"
        aria-label="new secret"
        tabindex="-1"
    >
        <header class="mb-3 flex items-center justify-between">
            <h3 class="font-mono text-sm font-semibold text-neutral-900">new secret</h3>
            <button
                type="button"
                onclick={onClose}
                class="inline-flex h-7 w-7 items-center justify-center rounded text-neutral-500 hover:bg-neutral-100"
                aria-label="close"
            >
                ✕
            </button>
        </header>

        <p class="mb-4 text-xs text-neutral-500">
            stores a value you already have — an API key, a webhook secret, etc.
            the value is sent once and never shown again.
        </p>

        <SetSecretValueForm
            onSubmit={submit}
            submitLabel="create secret"
            disabled={!nameValid}
            valueLabel="value"
            placeholder="paste the secret value"
        >
            {#snippet header()}
                <label class="flex flex-col gap-1 text-xs text-neutral-500">
                    name
                    <input
                        type="text"
                        bind:value={name}
                        spellcheck="false"
                        placeholder="STRIPE_API_KEY"
                        class="rounded border border-neutral-300 bg-white px-2 py-1 font-mono text-sm text-neutral-800 placeholder:text-neutral-400 focus:border-brand-cyan focus:outline-none"
                    />
                </label>
                {#if nameError}
                    <p class="text-xs text-red-700">{nameError}</p>
                {/if}
                <label class="flex flex-col gap-1 text-xs text-neutral-500">
                    description <span class="text-neutral-400">(optional)</span>
                    <input
                        type="text"
                        bind:value={description}
                        placeholder="what this is for"
                        class="rounded border border-neutral-300 bg-white px-2 py-1 text-sm text-neutral-800 placeholder:text-neutral-400 focus:border-brand-cyan focus:outline-none"
                    />
                </label>
            {/snippet}
        </SetSecretValueForm>
    </div>
</div>
