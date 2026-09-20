<script lang="ts">
    import { onMount } from 'svelte'

    // What the ipp head injects (see chassis/server/personality/ipp/ui):
    // the printer's label, its IPP address with the port spelled out, and
    // the queue path — the three fields macOS's "IP" tab asks for.
    type Printer = { name: string; uri: string; address: string; queue: string }

    function fromPage(): Printer {
        const el = document.getElementById('txco-printer')
        if (el?.textContent) {
            try {
                return JSON.parse(el.textContent) as Printer
            } catch {
                // fall through to the URL
            }
        }
        // vite dev (no head to inject): the page's own URL is the printer's.
        const secure = location.protocol === 'https:'
        const address = `${location.hostname}:${location.port || (secure ? '443' : '80')}`
        const path = location.pathname.replace(/\/+$/, '')
        return {
            name: path.split('/').pop() || 'printer',
            uri: `${secure ? 'ipps' : 'ipp'}://${address}${path}`,
            address,
            queue: path.replace(/^\//, ''),
        }
    }

    const printer = fromPage()
    // The ipps:// link opens Add Printer, which only a Mac has; anywhere
    // else the page is the manual setup sheet.
    const onMac = /Macintosh/.test(navigator.userAgent) && navigator.maxTouchPoints < 2

    let copied = $state(false)
    async function copyURI() {
        try {
            await navigator.clipboard.writeText(printer.uri)
            copied = true
            setTimeout(() => (copied = false), 1500)
        } catch {
            // clipboard refused (insecure context, permissions): the address
            // is on the page to select by hand
        }
    }

    onMount(() => {
        document.title = `${printer.name} · virtual printer`
    })
</script>

<main class="flex min-h-full items-center justify-center bg-neutral-50 px-4 py-10 text-neutral-900">
    <div class="w-full max-w-md rounded-lg border border-neutral-200 bg-white p-8 shadow-sm">
        <a
            href="https://www.thanks.computer/?utm_source=printer_setup"
            class="block text-center text-2xl font-semibold tracking-tight text-neutral-900"
        >
            thanks, c<span class="o1">o</span><span class="o2">o</span><span class="o3">o</span
            >mputer.
        </a>

        <div class="mt-8">
            <div class="text-xs tracking-wide text-neutral-400 uppercase">virtual printer</div>
            <h1 class="mt-1 text-xl font-semibold break-all">{printer.name}</h1>
            <div class="h-12" aria-hidden="true"></div>
        </div>

        {#if onMac}
            <a
                href={printer.uri}
                class="mt-6 block rounded-md bg-neutral-900 px-4 py-3 text-center text-sm font-semibold text-white hover:bg-neutral-700"
                >Add printer</a
            >
            <p class="mt-2 text-center text-xs text-neutral-400">
                Opens Add Printer. It asks once for the printer's password; any user name will do.
            </p>
        {/if}

        <div class="mt-8 border-t border-neutral-200 pt-6">
            <div class="text-xs tracking-wide text-neutral-400 uppercase">
                Manual Setup
            </div>
            <dl class="mt-3 grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-sm p-4">
                <dt class="text-neutral-500">Address</dt>
                <dd class="text-right break-all">{printer.address}</dd>
                <dt class="text-neutral-500">Protocol</dt>
                <dd class="text-right">IPP (Internet Printing Protocol)</dd>
                <dt class="text-neutral-500">Queue</dt>
                <dd class="text-right break-all">{printer.queue}</dd>
            </dl>
            <p class="mt-4 text-xs text-neutral-500">
                On a Mac: System Settings › Printers &amp; Scanners › Add Printer › IP. Elsewhere,
                add a printer by its address:
            </p>
            <button
                type="button"
                onclick={copyURI}
                class="mt-2 w-full rounded-md border border-neutral-200 bg-neutral-50 px-3 py-2 text-left text-xs break-all hover:border-neutral-300"
                title="Copy"
            >
                {printer.uri}
                <span class="float-right pl-2 text-neutral-400">{copied ? 'copied' : 'copy'}</span>
            </button>
        </div>
    </div>
</main>
