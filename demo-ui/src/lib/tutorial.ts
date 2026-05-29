// The guided walkthrough: short, CUMULATIVE series of steps grouped into
// TRACKS. Each step is a full snapshot of the demo state (ops +
// request) — so "Next" just loads the next snapshot and re-runs. Each
// step builds on the previous one.
//
// Track 1 ("Build a response") takes the user from a single op to a
// rendered page, entirely in txcl (no external apps), mirroring the
// quickstart hello-world pipeline.
//
// Track 2 ("Call an API") shows txco doing the thing it's actually for:
// EXEC'ing real external HTTP APIs and merging their results.

export type Method = 'GET' | 'POST' | 'PUT' | 'DELETE'

export interface OpFile {
    name: string
    scope: number
    txcl: string
    // Optional JavaScript compute source. When present, the Runner ships
    // it to the play build endpoint (which bundles + javy-compiles + stores
    // a `compute://sha256/<digest>` wasm artifact), then substitutes that
    // ref into this op's txcl in place of `op://<name>` before activate.
    // Defining `js: ""` (vs leaving it `undefined`) opts the op into the
    // JS textarea even when empty.
    js?: string
}

export interface Step {
    title: string
    prose: string
    ops: OpFile[]
    method: Method
    path: string
    body?: string
}

export interface Track {
    id: string
    title: string
    steps: Step[]
}

// The word ops all live at scope 0 — ops at the same scope run IN PARALLEL
// (the op-stack diagram fans them out side by side) and their EMITs merge
// by appending into one `words` list. `sentence` (100) and `render` (200)
// sit at later scopes because they must run AFTER the parallel ops have
// merged. (Order within the merged list follows completion order, so it
// can vary run to run — that's the parallel nature on display.)
const hello: OpFile = {
    name: 'hello',
    scope: 0,
    txcl: 'WHEN @src == "http"\n  EMIT .words = ["hello,"]',
}
const world: OpFile = {
    name: 'world',
    scope: 0,
    txcl: 'WHEN @src == "http"\n  EMIT .words = ["world"]',
}
const thanks: OpFile = {
    name: 'thanks',
    scope: 0,
    txcl: 'WHEN @web.req.url.path =~ /txco/\n  EMIT .words = ["thanks,"]',
}
const computer: OpFile = {
    name: 'computer',
    scope: 0,
    txcl: 'WHEN @web.req.url.path =~ /txco/\n  EMIT .words = ["computer"]',
}
const sentence: OpFile = {
    name: 'sentence',
    scope: 100,
    txcl: 'EMIT .sentence = &join(.words, " ")',
}
const render: OpFile = {
    name: 'render',
    scope: 200,
    txcl: `EMIT @web.res.status = 200,
     @web.res.body = &b64encode(.sentence),
     @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
     @halt = true`,
}

const buildSteps: Step[] = [
    {
        title: 'hello + world',
        prose:
            'Two ops at the same scope. Ops in a scope run in parallel — see them side by side in the op stack — and each EMITs a word into a shared `words` list, which merges by appending.',
        ops: [hello, world],
        method: 'GET',
        path: '/',
    },
    {
        title: 'add filtered ops',
        prose:
            'Add two more ops — `thanks` and `computer` — but each has a WHEN that only matches `/txco`. The request is still `/`, so neither resonates: they appear in the op stack but stay idle, and the `words` list is unchanged.',
        ops: [hello, world, thanks, computer],
        method: 'GET',
        path: '/',
    },
    {
        title: 'match the filter',
        prose:
            'Same ops — but the request is now `/txco`, so both `thanks` and `computer` match their WHEN (their "resonator") and fire, joining the list. Switch the path back to `/` and they go idle again.',
        ops: [hello, world, thanks, computer],
        method: 'GET',
        path: '/txco',
    },
    {
        title: 'make a sentence',
        prose:
            'The parallel ops all merge first; this op sits at a later scope, so it runs after — reading the merged `words` list and joining it with `&join`. Re-run a few times: the word order shuffles, because the ops ran concurrently.',
        ops: [hello, world, thanks, computer, sentence],
        method: 'GET',
        path: '/txco',
    },
    {
        title: 'render the response',
        prose:
            'Finally, shape the HTTP response: set the status, a content-type header, and the (base64-encoded) body — the rendered text. The response is now a real page, like the hello-world example.',
        ops: [hello, world, thanks, computer, sentence, render],
        method: 'GET',
        path: '/txco',
    },
]

// --- Track 3: Call an API (timeapi.io) ------------------------------------
// Each op EXECs an external HTTPS GET. The http transport defaults to
// POST-with-body, so `WITH method = "GET"` issues a body-less GET (the
// query params carry the request); `WITH into = "<key>"` nests that
// call's JSON response under its own key so two parallel calls merge
// cleanly into `{ london: {…}, tokyo: {…} }` instead of colliding.
const london: OpFile = {
    name: 'london',
    scope: 0,
    txcl:
        'WHEN @src == "http"\n' +
        '  EXEC "https://timeapi.io/api/Time/current/zone?timeZone=Europe/London"\n' +
        '    WITH method = "GET", into = "london"',
}
const tokyo: OpFile = {
    name: 'tokyo',
    scope: 0,
    txcl:
        'WHEN @src == "http"\n' +
        '  EXEC "https://timeapi.io/api/Time/current/zone?timeZone=Asia/Tokyo"\n' +
        '    WITH method = "GET", into = "tokyo"',
}
const summary: OpFile = {
    name: 'summary',
    scope: 100,
    txcl: 'EMIT .summary = &concat("London ", .london.time, " · Tokyo ", .tokyo.time)',
}
const trim: OpFile = {
    name: 'trim',
    scope: 200,
    txcl: 'EMIT @delete = ["london", "tokyo"]',
}

const apiSteps: Step[] = [
    {
        title: 'call an API',
        prose:
            'EXEC an external HTTPS endpoint. timeapi.io is a GET API, so `WITH method = "GET"` sends a body-less GET; `WITH into = "london"` nests the JSON response under a `london` key. Open the merged result to see the live time come back.',
        ops: [london],
        method: 'GET',
        path: '/',
    },
    {
        title: 'merge two calls',
        prose:
            'Add a second op for Tokyo at the same scope — the two calls run in parallel. Because each nests under its own key (`into`), their responses merge into one envelope: `{ london: {…}, tokyo: {…} }`.',
        ops: [london, tokyo],
        method: 'GET',
        path: '/',
    },
    {
        title: 'use the merged result',
        prose:
            'A later scope reads the merged envelope. This op runs after both calls return and EMITs a `summary` joining each city’s `time` field — combining two live APIs into one derived value.',
        ops: [london, tokyo, summary],
        method: 'GET',
        path: '/',
    },
    {
        title: 'return only the summary',
        prose:
            'Now drop the raw API responses with `EMIT @delete = ["london", "tokyo"]`. Because it runs at a later scope (200) — after `summary` has already read them — the merged envelope keeps just `summary`. `@delete` removes trees from the envelope itself, so the trace shows both calls still happened.',
        ops: [london, tokyo, summary, trim],
        method: 'GET',
        path: '/',
    },
]

// --- Track 2: With nano-op (the build track, but a JS compute does the sort)
// A pipeline that mirrors the build track's final shape — two txcl-EMIT ops
// put words on the envelope, a sort op turns them into a sentence, a render
// op shapes the HTTP response — but the sort step is authored as a JavaScript
// "nano-op" via `@txco/op` instead of a txcl `EMIT`. The chassis treats it
// as a peer: the downstream txcl `render` reads `.sentence` the same way it
// would from a pure-txcl pipeline. The demo bundles + sandboxes the
// JS (no node_modules, no servers) and resolves `op://sort` →
// `compute://sha256/<digest>` before activate.
const helloWorldT: OpFile = {
    name: 'hello-world',
    scope: 0,
    txcl: 'WHEN @src == "http"\n  EMIT .words = ["hello,", "world"]',
}
const thanksComputerT: OpFile = {
    name: 'thanks-computer',
    scope: 0,
    txcl: 'WHEN @src == "http"\n  EMIT .words = ["thanks,", "computer"]',
}
const sortNanoOp: OpFile = {
    name: 'sort',
    scope: 100,
    txcl: 'EXEC "op://sort"',
    js:
        'import { op } from "@txco/op";\n' +
        '\n' +
        '// Read the merged `words` array (assembled by the two scope-0 ops),\n' +
        '// sort it into the canonical sentence order, then join into a single\n' +
        '// string. The downstream `render` op reads `.sentence` — it doesn\'t\n' +
        '// know or care that this op happens to be JavaScript.\n' +
        'const ORDER = ["hello,", "world", "thanks,", "computer"];\n' +
        '\n' +
        'export default op(async ({ input }) => ({\n' +
        '  sentence: [...(input.words ?? [])]\n' +
        '    .sort((a, b) => ORDER.indexOf(a) - ORDER.indexOf(b))\n' +
        '    .join(" "),\n' +
        '}));\n',
}
const renderNanoOp: OpFile = {
    name: 'render',
    scope: 200,
    txcl: `EMIT @web.res.status = 200,
     @web.res.body = &b64encode(.sentence),
     @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
     @halt = true`,
}
const computeSteps: Step[] = [
    {
        title: 'two ops, four words',
        prose:
            "Two plain txcl ops at scope 0 — each EMITs a couple of words into a shared `words` array (merge-appends). Same parallel-merge story as the build track's `hello`/`world`/`thanks`/`computer`, just consolidated into two ops to keep the focus on what comes next.",
        ops: [helloWorldT, thanksComputerT],
        method: 'GET',
        path: '/',
    },
    {
        title: 'sort with a nano-op',
        prose:
            "Add a JavaScript compute at scope 100 — a *nano-op*. It reads `input.words` (the merged array), sorts into the canonical order, joins into a sentence, and returns `{ sentence: \"…\" }`. The demo bundles + sandboxes the JS (no node_modules, no servers); it's a peer of the txcl ops, not a foreign attachment.",
        ops: [helloWorldT, thanksComputerT, sortNanoOp],
        method: 'GET',
        path: '/',
    },
    {
        title: 'render the response',
        prose:
            "Finally, shape the HTTP response — the same render op as the build track. It reads `.sentence` (which our nano-op just produced) and base64-encodes it into the body. Nano-ops are first-class citizens in the pipeline: a downstream txcl op consumes the result transparently, just like it would from another `EMIT`.",
        ops: [helloWorldT, thanksComputerT, sortNanoOp, renderNanoOp],
        method: 'GET',
        path: '/',
    },
]

// --- Track 4: Async + continuations ---------------------------------------
// `WITH mode = "continuable"` lets the chassis bridge the worker-callback
// contract LOCALLY for upstreams that don't speak it. The op starts SYNC;
// the chassis races the call against a `continue_after` timer (default 5s,
// from `continue-after-default`). If the upstream answers in time, you get
// the response inline — no 202, no continuation, no fuss. If the timer
// wins, the chassis suspends the stage durably, emits `202 Accepted` +
// continuation token to the client (or `303` to the wait page for
// browsers), and keeps the in-flight upstream goroutine running. When the
// upstream eventually returns, the chassis records its body as the
// completion terminal and the rest of the stack runs through to its final
// result — same `Resume` machinery the worker-callback `mode = "async"`
// path uses. Distinct from `mode = "async"`, which requires the upstream
// to BE txco-aware (return `202` + POST back to /_txc/continuations/…).
const slowContinuable: OpFile = {
    name: 'slow',
    scope: 0,
    txcl:
        'WHEN @src == "http"\n' +
        '  EXEC "https://httpbin.org/delay/10"\n' +
        '    WITH mode = "continuable", timeout = "30s", method = "GET"',
}
const continuationSteps: Step[] = [
    {
        title: 'fire and continue',
        prose:
            "`mode = \"continuable\"` is the chassis bridging the worker contract LOCALLY for upstreams that don't speak it. The op starts sync; the chassis races against the default 5-second `continue_after` timer. If the upstream answers in time you get the response inline — no 202, no continuation. If it overruns, the chassis returns `202` + a continuation token to your client, browsers land on a polite \"working…\" wait page that polls until the run completes, and the still-in-flight goroutine treats the eventual upstream response as the completion — the rest of the stack (downstream scopes, render ops, anything else) runs to its final result. Here we wait on `httpbin.org/delay/10` for 10 real seconds through the wait page. `timeout = \"30s\"` caps the upstream work both before and after promotion; `method = \"GET\"` keeps the chassis from POSTing the envelope as the request body so cookies / private state don't leak upstream.",
        ops: [slowContinuable],
        method: 'GET',
        path: '/',
    },
]

// --- Track 5: Call an MCP server ------------------------------------------
// The chassis as an MCP CLIENT. `mcp+https://host/path#tool-name` triggers
// the MCP-over-HTTP session lifecycle (initialize →
// notifications/initialized → tools/call) on every EXEC; the URL fragment
// carries the tool name and is never sent over the wire. We point at
// DeepWiki — a free, no-auth public MCP server that exposes AI-powered
// Q&A tools for any GitHub repo — so the demo doesn't need credentials.
// The query op uses `mode = "continuable"` because AI inference usually
// takes several seconds: the chassis returns 202 + continuation token
// after `continue_after` (default 5 s), browsers land on the wait page,
// and the still-running goroutine records DeepWiki's response as the
// terminal — the same lifecycle as the previous track, but with a real
// MCP upstream instead of httpbin.
const repoSelect: OpFile = {
    name: 'repo',
    scope: 50,
    txcl:
        '# Resolve `.repoName` from `?repoName=…` or default.\n' +
        'SELECT @web.req.url.query.repoName.0\n' +
        '    AS .repoName\n' +
        '    DEFAULT "facebook/react"',
}
const questionSelect: OpFile = {
    name: 'question',
    scope: 50,
    txcl:
        '# Resolve `.question` from `?question=…` or default.\n' +
        'SELECT @web.req.url.query.question.0\n' +
        '    AS .question\n' +
        '    DEFAULT "What is JSX used for?"',
}
const mcpQuery: OpFile = {
    name: 'query',
    scope: 100,
    txcl:
        'WHEN @src == "http"\n' +
        '  EXEC "mcp+https://mcp.deepwiki.com/mcp#ask_question"\n' +
        '    WITH mode = "continuable", timeout = "60s"',
}
const mcpRender: OpFile = {
    name: 'render',
    scope: 200,
    txcl:
        '# `txco://web-render` shapes a stored field into the HTTP response.\n' +
        '# `content_type = "text/markdown"` runs DeepWiki\'s markdown answer\n' +
        '# through the chassis\'s built-in markdown→HTML converter; the iframe\n' +
        '# shows the rendered page, not raw markdown.\n' +
        'WHEN .text != ""\n' +
        '  EXEC "txco://web-render"\n' +
        '    WITH source = ".text",\n' +
        '         content_type = "text/markdown; charset=utf-8"',
}
const mcpSteps: Step[] = [
    {
        title: 'ask an MCP server',
        prose:
            "The chassis as an MCP CLIENT. `mcp+https://host/path#tool-name` triggers the MCP-over-HTTP session lifecycle (`initialize` → `notifications/initialized` → `tools/call`) on every EXEC; the URL fragment carries the tool name and is never sent over the wire. We point at DeepWiki — a free, no-auth, public MCP server that answers AI questions about any GitHub repo. The two SELECTs at scope 50 pull `.repoName` and `.question` from `?repoName=…` / `?question=…` query params, with sensible defaults. The MCP call at scope 100 uses `mode = \"continuable\"`: AI inference is slow, so the chassis returns `202` + continuation after the default 5-second `continue_after`, the browser lands on the wait page, and the goroutine treats DeepWiki's eventual answer as the completion. The raw envelope (after merge) carries the markdown reply in `.text` — see it in the merged-result panel.",
        ops: [repoSelect, questionSelect, mcpQuery],
        method: 'GET',
        path: '/',
    },
    {
        title: 'render the answer as HTML',
        prose:
            "Add `txco://web-render` at scope 200 — a built-in chassis op that shapes a stored field into the HTTP response. With `content_type = \"text/markdown\"` it runs DeepWiki's markdown reply through the chassis's built-in markdown→HTML converter, so the iframe shows the rendered page (headings, code blocks, lists) instead of raw markdown. Try editing the path to `/?repoName=tldraw/tldraw&question=How%20do%20I%20draw%20a%20line%3F` — the two SELECTs at scope 50 pick it up and the demo re-runs against the new question.",
        ops: [repoSelect, questionSelect, mcpQuery, mcpRender],
        method: 'GET',
        path: '/',
    },
]

export const tracks: Track[] = [
    { id: 'build', title: 'Build a response', steps: buildSteps },
    { id: 'compute', title: 'With nano-op', steps: computeSteps },
    { id: 'api', title: 'Call an API', steps: apiSteps },
    { id: 'continuations', title: 'Async + continuations', steps: continuationSteps },
    { id: 'mcp', title: 'Call an MCP server', steps: mcpSteps },
]
