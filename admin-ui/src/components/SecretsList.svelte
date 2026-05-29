<script lang="ts">
    import type { SecretMetadata } from '../lib/api'
    import { hasCapability, store } from '../lib/store.svelte'
    import Ago from './Ago.svelte'
    import NewSecretModal from './NewSecretModal.svelte'

    interface Props {
        onSelectSecret: (name: string) => void
    }
    let { onSelectSecret }: Props = $props()

    let showNew = $state(false)
    const canWrite = $derived(hasCapability(store.state.session, 'secret:*:write'))

    // Refresh on every mount of the list view, so revisiting the tab
    // shows current data without a manual refresh control. Cheap — one
    // metadata request, no values. Writes also refresh via the store
    // actions, so the list stays current after create/rotate/revoke.
    $effect(() => {
        store.refreshSecrets()
    })

    // Tenant-wide rows first, then by name. Stable within each group so
    // the table doesn't reshuffle on refresh.
    const sorted = $derived.by<SecretMetadata[]>(() => {
        const xs = [...store.state.secrets]
        xs.sort((a, b) => {
            const as = a.stack ?? ''
            const bs = b.stack ?? ''
            if (as !== bs) {
                return as === '' ? -1 : bs === '' ? 1 : as.localeCompare(bs)
            }
            return a.name.localeCompare(b.name)
        })
        return xs
    })
</script>

<div class="flex h-full flex-col p-4">
    <header class="mb-3 flex items-center justify-between gap-3">
        <div>
            <h2 class="font-mono text-base font-semibold text-neutral-900">secrets</h2>
            <p class="text-xs text-neutral-500">
                per-tenant secret store — metadata only; values are never shown.
            </p>
        </div>
        {#if canWrite && !store.state.secretsUnavailable}
            <button
                type="button"
                onclick={() => (showNew = true)}
                class="rounded border border-brand-cyan/50 bg-brand-cyan/10 px-2 py-0.5 font-mono text-xs text-neutral-900 hover:bg-brand-cyan/20 focus:border-brand-cyan focus:outline-none"
            >
                + new
            </button>
        {/if}
    </header>

    {#if store.state.secretsUnavailable}
        <p class="rounded border border-amber-300 bg-amber-50 p-4 text-sm text-amber-900">
            the secret store isn't configured on this chassis. set
            <span class="font-mono">--secret-master-key</span>
            (or <span class="font-mono">TXCO_SECRET_MASTER_KEY</span>) to enable it.
        </p>
    {:else if !store.state.secretsLoaded}
        <p class="text-sm italic text-neutral-400">loading…</p>
    {:else if sorted.length === 0}
        <p class="rounded border border-neutral-200 bg-white p-4 text-sm italic text-neutral-400">
            no secrets yet — create one from the CLI with
            <span class="font-mono not-italic text-neutral-600"
                >txco auth tenant secrets set &lt;NAME&gt;</span
            >
            or
            <span class="font-mono not-italic text-neutral-600"
                >… generate &lt;NAME&gt;</span
            >.
        </p>
    {:else}
        <div class="overflow-auto rounded border border-neutral-200 bg-white">
            <table class="w-full text-sm">
                <thead
                    class="border-b border-neutral-200 bg-neutral-50 text-left text-xs uppercase tracking-wide text-neutral-500"
                >
                    <tr>
                        <th class="px-3 py-2 font-medium">name</th>
                        <th class="px-3 py-2 font-medium">scope</th>
                        <th class="px-3 py-2 font-medium">description</th>
                        <th class="px-3 py-2 font-medium">created</th>
                        <th class="px-3 py-2 font-medium">rotated</th>
                        <th class="px-3 py-2 font-medium">key</th>
                    </tr>
                </thead>
                <tbody class="divide-y divide-neutral-100">
                    {#each sorted as s (s.secret_id)}
                        <tr
                            class="cursor-pointer hover:bg-neutral-50"
                            onclick={() => onSelectSecret(s.name)}
                        >
                            <td
                                class="px-3 py-2 align-top font-mono text-xs font-medium text-neutral-900"
                            >
                                {s.name}
                            </td>
                            <td class="px-3 py-2 align-top text-xs">
                                {#if s.stack}
                                    <span class="font-mono text-neutral-700">{s.stack}</span>
                                {:else}
                                    <span class="italic text-neutral-400">tenant-wide</span>
                                {/if}
                            </td>
                            <td
                                class="px-3 py-2 align-top text-xs text-neutral-600"
                                title={s.description ?? ''}
                            >
                                {s.description || '—'}
                            </td>
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-400">
                                <Ago at={s.created_at} />
                            </td>
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-400">
                                {#if s.last_rotated_at}
                                    <Ago at={s.last_rotated_at} />
                                {:else}
                                    never
                                {/if}
                            </td>
                            <td class="px-3 py-2 align-top font-mono text-xs text-neutral-500">
                                v{s.key_version}
                            </td>
                        </tr>
                    {/each}
                </tbody>
            </table>
        </div>
    {/if}

    {#if showNew}
        <NewSecretModal onClose={() => (showNew = false)} />
    {/if}
</div>
