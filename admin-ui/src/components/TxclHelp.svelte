<script lang="ts">
    // Collapsible cheatsheet anchored to the bottom of the Resonator
    // tab. Default: collapsed. Click the header to expand; click
    // again to collapse. Open/closed state persists in localStorage
    // so the user's preference survives reloads.
    const KEY = 'admin-ui:txcl-help-open'
    let open = $state(readOpen())

    function readOpen(): boolean {
        if (typeof localStorage === 'undefined') return false
        try {
            return localStorage.getItem(KEY) === '1'
        } catch {
            return false
        }
    }

    function toggle() {
        open = !open
        if (typeof localStorage === 'undefined') return
        try {
            localStorage.setItem(KEY, open ? '1' : '0')
        } catch {
            // ignore quota / disabled storage
        }
    }
</script>

<div class="rounded border border-neutral-200 bg-white">
    <button
        type="button"
        class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs text-neutral-600 hover:bg-neutral-50"
        onclick={toggle}
    >
        <span class="inline-block w-3 text-neutral-400">{open ? '▾' : '▸'}</span>
        <span class="font-medium">txcl syntax</span>
        <span class="ml-auto text-[11px] text-neutral-400">quick reference</span>
    </button>

    {#if open}
        <div class="max-h-[50vh] overflow-auto border-t border-neutral-200 px-4 py-3 text-xs leading-relaxed text-neutral-700">
            <section class="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
                <div class="col-span-2 mb-1 text-[10px] font-semibold uppercase tracking-wide text-neutral-400">keywords</div>
                <code class="text-sky-700">WHEN &lt;expr&gt;</code>
                <span class="text-neutral-500">predicate — resonator matches, operations runs only when true. [default = <code class="text-neutral-700">true</code>]</span>
                <code class="text-sky-700">EXEC &quot;&lt;stack&gt;/&lt;scope&gt;&quot;</code>
                <span class="text-neutral-500">jump to another op by scope path (e.g. <code class="text-neutral-700">&quot;hello-world/0&quot;</code>) [default = go to next step sequentially]</span>
                <code class="text-sky-700">EXEC &quot;op://NAME&quot;</code>
                <span class="text-neutral-500">jump to a named operation (resolved at <code class="text-neutral-700">txco apply</code> time)</span>
                <code class="text-sky-700">SELECT &lt;src&gt; AS &lt;dst&gt; [DEFAULT &lt;lit&gt;]</code>
                <span class="text-neutral-500">copy a value from one envelope path to another. When <code class="text-neutral-700">&lt;src&gt;</code> resolves empty/missing, <code class="text-neutral-700">DEFAULT</code>'s literal is substituted. Multiple assignments: <code class="text-neutral-700">SELECT .a AS .x, .b AS .y</code>. No EXEC required — SELECT commits on its own.</span>
                <code class="text-sky-700">SET &lt;path&gt; = &lt;literal&gt;</code>
                <span class="text-neutral-500">inject a literal field on the op's input (before EXEC) or response (after EXEC). RHS is literal-only — use <code class="text-neutral-700">SELECT</code> to copy from envelope paths. Values can be scalars or arrays (<code class="text-neutral-700">[1, 2, &quot;x&quot;]</code>); <code class="text-neutral-700">b64&quot;&hellip;&quot;</code> base64-encodes (handy for response bodies).</span>
                <code class="text-sky-700">EMIT &lt;path&gt; = &lt;literal&gt;</code>
                <span class="text-neutral-500">overlay literal values onto THIS rule's response before the scope merge. Overwrite semantics. Works alone (no EXEC needed) or paired with EXEC to enrich the dispatched response.</span>

                <div class="col-span-2 mt-3 mb-1 text-[10px] font-semibold uppercase tracking-wide text-neutral-400">paths</div>
                <code class="text-emerald-700">._txc.src</code>
                <span class="text-neutral-500">incoming source: <code class="text-neutral-700">&quot;http&quot;</code>, <code class="text-neutral-700">&quot;tcp&quot;</code>, &hellip;</span>
                <code class="text-emerald-700">.foo.bar</code>
                <span class="text-neutral-500">nested key access</span>
                <code class="text-emerald-700">.items[0]</code>
                <span class="text-neutral-500">array index</span>
                <code class="text-emerald-700">@web.res.status</code>
                <span class="text-neutral-500">shorthand for <code class="text-neutral-700">._txc.web.res.status</code></span>
                <code class="text-emerald-700">.headers.content-type</code>
                <span class="text-neutral-500">hyphens are fine in keys; quote a segment for dots / spaces: <code class="text-neutral-700">.&quot;a.b&quot;</code></span>

                <div class="col-span-2 mt-3 mb-1 text-[10px] font-semibold uppercase tracking-wide text-neutral-400">operators</div>
                <code class="text-amber-700">== != &lt; &lt;= &gt; &gt;=</code>
                <span class="text-neutral-500">comparison</span>
                <code class="text-amber-700">=~ /re/ &nbsp; !~ /re/</code>
                <span class="text-neutral-500">regex match / non-match</span>
                <code class="text-amber-700">&amp;&amp; &nbsp; || &nbsp; ! &nbsp; ( )</code>
                <span class="text-neutral-500">combine / negate / group conditions in <code class="text-neutral-700">WHEN</code> (comma = <code class="text-neutral-700">&amp;&amp;</code>)</span>

                <div class="col-span-2 mt-3 mb-1 text-[10px] font-semibold uppercase tracking-wide text-neutral-400">refs &amp; comments</div>
                <code class="text-purple-700">op://NAME</code>
                <span class="text-neutral-500">declared under <code class="text-neutral-700">operations:</code> in <code class="text-neutral-700">txco.yaml</code>; <code class="text-neutral-700">txco apply</code> substitutes the URL before ship. The chassis (and this view) only sees the resolved form.</span>
                <code class="text-purple-700">&quot;&lt;stack&gt;/&lt;scope&gt;&quot;</code>
                <span class="text-neutral-500">jump target — e.g. <code class="text-neutral-700">&quot;hello-world/0&quot;</code></span>
                <code class="text-purple-700">txco://noop</code>
                <span class="text-neutral-500">placeholder EXEC; returns an empty object. Most rules want <code class="text-neutral-700">EMIT</code> instead (commits overlays without dispatching).</span>
                <code class="text-neutral-500"># a note</code>
                <span class="text-neutral-500">line comments run to end-of-line</span>
            </section>

            <section class="mt-4">
                <div class="mb-1 text-[10px] font-semibold uppercase tracking-wide text-neutral-400">examples</div>
                <ol class="space-y-2.5">
                    <li>
                        <div class="mb-0.5 text-[11px] text-neutral-500">gate by source, then jump</div>
<pre class="overflow-auto rounded bg-neutral-900 p-2 text-[11px] leading-snug text-neutral-100"><code><span class="text-sky-300">WHEN</span> <span class="text-emerald-300">._txc.src</span> <span class="text-amber-300">==</span> <span class="text-emerald-200">&quot;http&quot;</span>   <span class="text-neutral-400"># only match HTTP requests</span>
<span class="text-sky-300">EXEC</span> <span class="text-emerald-200">&quot;hello-world/0&quot;</span>       <span class="text-neutral-400"># jump to that stack at scope 0</span></code></pre>
                    </li>
                    <li>
                        <div class="mb-0.5 text-[11px] text-neutral-500">jump to a named operation</div>
<pre class="overflow-auto rounded bg-neutral-900 p-2 text-[11px] leading-snug text-neutral-100"><code><span class="text-sky-300">WHEN</span> <span class="text-amber-300">*</span>                     <span class="text-neutral-400"># match anything</span>
<span class="text-sky-300">EXEC</span> <span class="text-emerald-200">&quot;op://WORLD&quot;</span>          <span class="text-neutral-400"># declared in txco.yaml; resolved at apply</span></code></pre>
                    </li>
                    <li>
                        <div class="mb-0.5 text-[11px] text-neutral-500">copy a query param onto the envelope with a default — no EXEC needed</div>
<pre class="overflow-auto rounded bg-neutral-900 p-2 text-[11px] leading-snug text-neutral-100"><code><span class="text-sky-300">SELECT</span> <span class="text-emerald-300">@web.req.url.query.repoName.0</span>
    <span class="text-sky-300">AS</span> <span class="text-emerald-300">.repoName</span>
    <span class="text-sky-300">DEFAULT</span> <span class="text-emerald-200">&quot;facebook/react&quot;</span></code></pre>
                    </li>
                    <li>
                        <div class="mb-0.5 text-[11px] text-neutral-500">regex match + inject a field, then dispatch to a named op</div>
<pre class="overflow-auto rounded bg-neutral-900 p-2 text-[11px] leading-snug text-neutral-100"><code><span class="text-sky-300">WHEN</span> <span class="text-emerald-300">@web.path</span> <span class="text-amber-300">=~</span> <span class="text-purple-300">/^\/api\//</span>   <span class="text-neutral-400"># path-based match</span>
<span class="text-sky-300">SET</span> <span class="text-emerald-300">.tenant</span> <span class="text-amber-300">=</span> <span class="text-emerald-200">&quot;default&quot;</span>             <span class="text-neutral-400"># inject before dispatch</span>
<span class="text-sky-300">EXEC</span> <span class="text-emerald-200">&quot;op://API_HANDLER&quot;</span>             <span class="text-neutral-400"># resolved at apply time</span></code></pre>
                    </li>
                    <li>
                        <div class="mb-0.5 text-[11px] text-neutral-500">contribute a literal value to the scope merge — EMIT alone, no EXEC</div>
<pre class="overflow-auto rounded bg-neutral-900 p-2 text-[11px] leading-snug text-neutral-100"><code><span class="text-sky-300">EMIT</span> <span class="text-emerald-300">.words</span> <span class="text-amber-300">=</span> <span class="text-emerald-200">[&quot;cruel&quot;]</span>    <span class="text-neutral-400"># overlay onto this rule's response</span></code></pre>
                    </li>
                    <li>
                        <div class="mb-0.5 text-[11px] text-neutral-500">enrich a real EXEC response — call out, then EMIT additional fields</div>
<pre class="overflow-auto rounded bg-neutral-900 p-2 text-[11px] leading-snug text-neutral-100"><code><span class="text-sky-300">EXEC</span> <span class="text-emerald-200">&quot;http://localhost:4100/words/hello&quot;</span>
<span class="text-sky-300">EMIT</span> <span class="text-emerald-300">.tagged_by</span> <span class="text-amber-300">=</span> <span class="text-emerald-200">&quot;cruel-rule&quot;</span></code></pre>
                    </li>
                    <li>
                        <div class="mb-0.5 text-[11px] text-neutral-500">synthesize a plain-text 404 for any non-root path — boolean WHEN, <code class="text-neutral-700">@</code> sugar, <code class="text-neutral-700">b64</code> body</div>
<pre class="overflow-auto rounded bg-neutral-900 p-2 text-[11px] leading-snug text-neutral-100"><code><span class="text-sky-300">WHEN</span> <span class="text-emerald-300">@web.req.url.path</span> <span class="text-amber-300">!=</span> <span class="text-emerald-200">&quot;/&quot;</span> <span class="text-amber-300">&amp;&amp;</span> <span class="text-emerald-300">@web.req.url.path</span> <span class="text-amber-300">!=</span> <span class="text-emerald-200">&quot;/another/path&quot;</span>
<span class="text-sky-300">EMIT</span> <span class="text-emerald-300">@halt</span> <span class="text-amber-300">=</span> <span class="text-emerald-200">true</span>,
    <span class="text-emerald-300">@web.res.status</span> <span class="text-amber-300">=</span> <span class="text-emerald-200">404</span>,
    <span class="text-emerald-300">@web.res.body</span> <span class="text-amber-300">=</span> <span class="text-emerald-200">b64&quot;Sorry, not here.&quot;</span>,
    <span class="text-emerald-300">@web.res.headers.content-type</span> <span class="text-amber-300">=</span> <span class="text-emerald-200">[&quot;text/plain&quot;]</span>   <span class="text-neutral-400"># no EXEC — EMIT commits on its own</span></code></pre>
                    </li>
                </ol>
            </section>
        </div>
    {/if}
</div>

<style>
    code {
        font-family: var(--font-mono);
    }
</style>
