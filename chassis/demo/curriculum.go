// Package demo holds the txco demo's curriculum — the tracks, steps,
// and starting ops that `txco demo` seeds into a chassis at boot. The
// SAME data feeds two consumers:
//
//   - chassis/cli/demo.runDemo → demo.Seed(adminURL) pre-seeds every
//     stack against the loopback admin API before opening the browser,
//     so the user never sees a "step failed to seed" error on first
//     load.
//   - chassis/server/admin.handleDemoCurriculum exposes the same data
//     at GET /v1/demo/curriculum so the admin-ui Runner can render the
//     walkthrough (titles, prose, ops, request shape) without
//     duplicating the curriculum in TypeScript.
//
// Single source of truth: don't add a parallel definition in
// admin-ui/. The SPA fetches its rendering data from the endpoint
// above; the seeding is done already by the time the SPA mounts.
package demo

// Method is the HTTP method a step's fire uses. The same restricted
// set the admin-ui's TypeScript declares; serialized as the bare verb
// string ("GET", "POST", "PUT", "DELETE").
type Method string

// OpFile is one op in a step's ops list — its name, the scope it runs
// at (ops at the same scope run in parallel; later scopes run after),
// the TXCL source, and optionally a JavaScript "nano-op" compute
// source. When Js is non-empty the seed compiles it via the demo's
// /v1/demo/op/build endpoint (which bundles + javy-compiles + stores a
// `compute://sha256/<digest>` wasm artifact) and substitutes that ref
// into Txcl in place of `op://<name>` before activate. An empty-string
// Js (vs nil) opts the op into the SPA's JS textarea even when no
// source has been authored yet.
type OpFile struct {
	Name  string `json:"name"`
	Scope int    `json:"scope"`
	Txcl  string `json:"txcl"`
	Js    string `json:"js,omitempty"`
}

// Step is one stop on the walkthrough — a title + prose explaining
// what changed since the previous step, the cumulative ops list, and
// the request (method/path/body) to fire against it. Every step has
// its OWN stack (Stack), bound to `<stack>.local.thanks.computer` so
// tab switches are pure-navigation rather than re-applies.
type Step struct {
	Title  string   `json:"title"`
	Prose  string   `json:"prose"`
	Ops    []OpFile `json:"ops"`
	Method Method   `json:"method"`
	Path   string   `json:"path"`
	Body   string   `json:"body,omitempty"`
	Stack  string   `json:"stack"`
}

// Track groups a sequence of steps under a single ID + title. The ID
// is the prefix for each step's default stack name (`<id>-<i+1>`).
type Track struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Steps []Step `json:"steps"`
}

// Curriculum is the top-level shape returned by GET /v1/demo/curriculum
// and consumed by Seed. HostSuffix is the wildcard DNS suffix every
// step's bound hostname uses (e.g. `<stack>.local.thanks.computer`);
// the wildcard resolves to 127.0.0.1 publicly so loopback "subdomain"
// routing works without per-machine DNS setup.
type Curriculum struct {
	HostSuffix string  `json:"host_suffix"`
	Tracks     []Track `json:"tracks"`
}

// DefaultHostSuffix is the wildcard DNS suffix used for all demo
// stack hostnames.
const DefaultHostSuffix = "local.thanks.computer"

// --- shared op definitions (cumulative across steps within a track) -----

// The build track's ops all live at scope 0 — ops at the same scope
// run IN PARALLEL (the op-stack diagram fans them out side by side)
// and their EMITs merge by appending into one `words` list. `sentence`
// (100) and `render` (200) sit at later scopes because they must run
// AFTER the parallel ops have merged. (Order within the merged list
// follows completion order, so it can vary run to run — that's the
// parallel nature on display.)

var opHello = OpFile{
	Name:  "hello",
	Scope: 0,
	Txcl:  "WHEN @src == \"http\"\n  EMIT .words = [\"hello,\"]",
}

var opWorld = OpFile{
	Name:  "world",
	Scope: 0,
	Txcl:  "WHEN @src == \"http\"\n  EMIT .words = [\"world\"]",
}

var opThanks = OpFile{
	Name:  "thanks",
	Scope: 0,
	Txcl:  "WHEN @web.req.url.path =~ /txco/\n  EMIT .words = [\"thanks,\"]",
}

var opComputer = OpFile{
	Name:  "computer",
	Scope: 0,
	Txcl:  "WHEN @web.req.url.path =~ /txco/\n  EMIT .words = [\"computer\"]",
}

var opSentence = OpFile{
	Name:  "sentence",
	Scope: 100,
	Txcl:  "EMIT .sentence = &join(.words, \" \")",
}

var opRender = OpFile{
	Name:  "render",
	Scope: 200,
	Txcl: `EMIT @web.res.status = 200,
     @web.res.body = &b64encode(.sentence),
     @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
     @halt = true`,
}

// --- API track: external HTTPS calls via EXEC -----------------------

// The http transport defaults to POST-with-body, so `WITH method =
// "GET"` issues a body-less GET (the query params carry the request);
// `WITH into = "<key>"` nests that call's JSON response under its own
// key so two parallel calls merge cleanly into `{ london: {…}, tokyo:
// {…} }` instead of colliding.

var opLondon = OpFile{
	Name:  "london",
	Scope: 0,
	Txcl: "WHEN @src == \"http\"\n" +
		"  EXEC \"https://timeapi.io/api/Time/current/zone?timeZone=Europe/London\"\n" +
		"    WITH method = \"GET\", into = \"london\"",
}

var opTokyo = OpFile{
	Name:  "tokyo",
	Scope: 0,
	Txcl: "WHEN @src == \"http\"\n" +
		"  EXEC \"https://timeapi.io/api/Time/current/zone?timeZone=Asia/Tokyo\"\n" +
		"    WITH method = \"GET\", into = \"tokyo\"",
}

var opSummary = OpFile{
	Name:  "summary",
	Scope: 100,
	Txcl:  "EMIT .summary = &concat(\"London \", .london.time, \" · Tokyo \", .tokyo.time)",
}

var opTrim = OpFile{
	Name:  "trim",
	Scope: 200,
	Txcl:  "EMIT @delete = [\"london\", \"tokyo\"]",
}

// --- Compute track: nano-op (JS compiled to wasm) -------------------

var opHelloWorldT = OpFile{
	Name:  "hello-world",
	Scope: 0,
	Txcl:  "WHEN @src == \"http\"\n  EMIT .words = [\"hello,\", \"world\"]",
}

var opThanksComputerT = OpFile{
	Name:  "thanks-computer",
	Scope: 0,
	Txcl:  "WHEN @src == \"http\"\n  EMIT .words = [\"thanks,\", \"computer\"]",
}

var opSortNanoOp = OpFile{
	Name:  "sort",
	Scope: 100,
	Txcl:  "EXEC \"op://sort\"",
	Js: `import { op } from "@txco/op";

// Read the merged ` + "`words`" + ` array (assembled by the two scope-0 ops),
// sort it into the canonical sentence order, then join into a single
// string. The downstream ` + "`render`" + ` op reads ` + "`.sentence`" + ` — it doesn't
// know or care that this op happens to be JavaScript.
const ORDER = ["hello,", "world", "thanks,", "computer"];

export default op(async ({ input }) => ({
  sentence: [...(input.words ?? [])]
    .sort((a, b) => ORDER.indexOf(a) - ORDER.indexOf(b))
    .join(" "),
}));
`,
}

// renderNanoOp is the same shape as opRender but defined separately
// so a future track can diverge without coupling.
var opRenderNanoOp = OpFile{
	Name:  "render",
	Scope: 200,
	Txcl: `EMIT @web.res.status = 200,
     @web.res.body = &b64encode(.sentence),
     @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
     @halt = true`,
}

// --- Continuations track: continuable upstream call -----------------

var opSlowContinuable = OpFile{
	Name:  "slow",
	Scope: 0,
	Txcl: "WHEN @src == \"http\"\n" +
		"  EXEC \"https://httpbin.org/delay/10\"\n" +
		"    WITH mode = \"continuable\", timeout = \"30s\", method = \"GET\"",
}

// --- MCP track: chassis as MCP CLIENT -------------------------------

var opRepoSelect = OpFile{
	Name:  "repo",
	Scope: 50,
	Txcl: "# Resolve `.repoName` from `?repoName=…` or default.\n" +
		"SELECT @web.req.url.query.repoName.0\n" +
		"    AS .repoName\n" +
		"    DEFAULT \"facebook/react\"",
}

var opQuestionSelect = OpFile{
	Name:  "question",
	Scope: 50,
	Txcl: "# Resolve `.question` from `?question=…` or default.\n" +
		"SELECT @web.req.url.query.question.0\n" +
		"    AS .question\n" +
		"    DEFAULT \"What is JSX used for?\"",
}

var opMcpQuery = OpFile{
	Name:  "query",
	Scope: 100,
	Txcl: "WHEN @src == \"http\"\n" +
		"  EXEC \"mcp+https://mcp.deepwiki.com/mcp#ask_question\"\n" +
		"    WITH mode = \"continuable\", timeout = \"60s\"",
}

var opMcpRender = OpFile{
	Name:  "render",
	Scope: 200,
	Txcl: "# `txco://web-render` shapes a stored field into the HTTP response.\n" +
		"# `content_type = \"text/markdown\"` runs DeepWiki's markdown answer\n" +
		"# through the chassis's built-in markdown→HTML converter; the iframe\n" +
		"# shows the rendered page, not raw markdown.\n" +
		"WHEN .text != \"\"\n" +
		"  EXEC \"txco://web-render\"\n" +
		"    WITH source = \".text\",\n" +
		"         content_type = \"text/markdown; charset=utf-8\"",
}

// Get returns the full demo curriculum. Stack names are derived
// (`<trackID>-<i+1>` for each step) and populated here so consumers
// don't have to know the convention. Returning a fresh value rather
// than a package-level pointer so callers (the seed loop, the HTTP
// handler) can mutate locally without affecting each other.
func Get() Curriculum {
	c := Curriculum{
		HostSuffix: DefaultHostSuffix,
		Tracks: []Track{
			{
				ID:    "build",
				Title: "Build a response",
				Steps: []Step{
					{
						Title:  "hello + world",
						Prose:  "Two ops at the same scope. Ops in a scope run in parallel — see them side by side in the op stack — and each EMITs a word into a shared `words` list, which merges by appending.",
						Ops:    []OpFile{opHello, opWorld},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "add filtered ops",
						Prose:  "Add two more ops — `thanks` and `computer` — but each has a WHEN that only matches `/txco`. The request is still `/`, so neither resonates: they appear in the op stack but stay idle, and the `words` list is unchanged.",
						Ops:    []OpFile{opHello, opWorld, opThanks, opComputer},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "match the filter",
						Prose:  "Same ops — but the request is now `/txco`, so both `thanks` and `computer` match their WHEN (their \"resonator\") and fire, joining the list. Switch the path back to `/` and they go idle again.",
						Ops:    []OpFile{opHello, opWorld, opThanks, opComputer},
						Method: "GET",
						Path:   "/txco",
					},
					{
						Title:  "make a sentence",
						Prose:  "The parallel ops all merge first; this op sits at a later scope, so it runs after — reading the merged `words` list and joining it with `&join`. Re-run a few times: the word order shuffles, because the ops ran concurrently.",
						Ops:    []OpFile{opHello, opWorld, opThanks, opComputer, opSentence},
						Method: "GET",
						Path:   "/txco",
					},
					{
						Title:  "render the response",
						Prose:  "Finally, shape the HTTP response: set the status, a content-type header, and the (base64-encoded) body — the rendered text. The response is now a real page, like the hello-world example.",
						Ops:    []OpFile{opHello, opWorld, opThanks, opComputer, opSentence, opRender},
						Method: "GET",
						Path:   "/txco",
					},
				},
			},
			{
				ID:    "compute",
				Title: "With nano-op",
				Steps: []Step{
					{
						Title:  "two ops, four words",
						Prose:  "Two plain txcl ops at scope 0 — each EMITs a couple of words into a shared `words` array (merge-appends). Same parallel-merge story as the build track's `hello`/`world`/`thanks`/`computer`, just consolidated into two ops to keep the focus on what comes next.",
						Ops:    []OpFile{opHelloWorldT, opThanksComputerT},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "sort with a nano-op",
						Prose:  "Add a JavaScript compute at scope 100 — a *nano-op*. It reads `input.words` (the merged array), sorts into the canonical order, joins into a sentence, and returns `{ sentence: \"…\" }`. The demo bundles + sandboxes the JS (no node_modules, no servers); it's a peer of the txcl ops, not a foreign attachment.",
						Ops:    []OpFile{opHelloWorldT, opThanksComputerT, opSortNanoOp},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "render the response",
						Prose:  "Finally, shape the HTTP response — the same render op as the build track. It reads `.sentence` (which our nano-op just produced) and base64-encodes it into the body. Nano-ops are first-class citizens in the pipeline: a downstream txcl op consumes the result transparently, just like it would from another `EMIT`.",
						Ops:    []OpFile{opHelloWorldT, opThanksComputerT, opSortNanoOp, opRenderNanoOp},
						Method: "GET",
						Path:   "/",
					},
				},
			},
			{
				ID:    "api",
				Title: "Call an API",
				Steps: []Step{
					{
						Title:  "call an API",
						Prose:  "EXEC an external HTTPS endpoint. timeapi.io is a GET API, so `WITH method = \"GET\"` sends a body-less GET; `WITH into = \"london\"` nests the JSON response under a `london` key. Open the merged result to see the live time come back.",
						Ops:    []OpFile{opLondon},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "merge two calls",
						Prose:  "Add a second op for Tokyo at the same scope — the two calls run in parallel. Because each nests under its own key (`into`), their responses merge into one envelope: `{ london: {…}, tokyo: {…} }`.",
						Ops:    []OpFile{opLondon, opTokyo},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "use the merged result",
						Prose:  "A later scope reads the merged envelope. This op runs after both calls return and EMITs a `summary` joining each city's `time` field — combining two live APIs into one derived value.",
						Ops:    []OpFile{opLondon, opTokyo, opSummary},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "return only the summary",
						Prose:  "Now drop the raw API responses with `EMIT @delete = [\"london\", \"tokyo\"]`. Because it runs at a later scope (200) — after `summary` has already read them — the merged envelope keeps just `summary`. `@delete` removes trees from the envelope itself, so the trace shows both calls still happened.",
						Ops:    []OpFile{opLondon, opTokyo, opSummary, opTrim},
						Method: "GET",
						Path:   "/",
					},
				},
			},
			{
				ID:    "continuations",
				Title: "Async + continuations",
				Steps: []Step{
					{
						Title:  "fire and continue",
						Prose:  "`mode = \"continuable\"` is the chassis bridging the worker contract LOCALLY for upstreams that don't speak it. The op starts sync; the chassis races against the default 5-second `continue_after` timer. If the upstream answers in time you get the response inline — no 202, no continuation. If it overruns, the chassis returns `202` + a continuation token to your client, browsers land on a polite \"working…\" wait page that polls until the run completes, and the still-in-flight goroutine treats the eventual upstream response as the completion — the rest of the stack (downstream scopes, render ops, anything else) runs to its final result. Here we wait on `httpbin.org/delay/10` for 10 real seconds through the wait page. `timeout = \"30s\"` caps the upstream work both before and after promotion; `method = \"GET\"` keeps the chassis from POSTing the envelope as the request body so cookies / private state don't leak upstream.",
						Ops:    []OpFile{opSlowContinuable},
						Method: "GET",
						Path:   "/",
					},
				},
			},
			{
				ID:    "mcp",
				Title: "Call an MCP server",
				Steps: []Step{
					{
						Title:  "ask an MCP server",
						Prose:  "The chassis as an MCP CLIENT. `mcp+https://host/path#tool-name` triggers the MCP-over-HTTP session lifecycle (`initialize` → `notifications/initialized` → `tools/call`) on every EXEC; the URL fragment carries the tool name and is never sent over the wire. We point at DeepWiki — a free, no-auth, public MCP server that answers AI questions about any GitHub repo. The two SELECTs at scope 50 pull `.repoName` and `.question` from `?repoName=…` / `?question=…` query params, with sensible defaults. The MCP call at scope 100 uses `mode = \"continuable\"`: AI inference is slow, so the chassis returns `202` + continuation after the default 5-second `continue_after`, the browser lands on the wait page, and the goroutine treats DeepWiki's eventual answer as the completion. The raw envelope (after merge) carries the markdown reply in `.text` — see it in the merged-result panel.",
						Ops:    []OpFile{opRepoSelect, opQuestionSelect, opMcpQuery},
						Method: "GET",
						Path:   "/",
					},
					{
						Title:  "render the answer as HTML",
						Prose:  "Add `txco://web-render` at scope 200 — a built-in chassis op that shapes a stored field into the HTTP response. With `content_type = \"text/markdown\"` it runs DeepWiki's markdown reply through the chassis's built-in markdown→HTML converter, so the iframe shows the rendered page (headings, code blocks, lists) instead of raw markdown. Try editing the path to `/?repoName=tldraw/tldraw&question=How%20do%20I%20draw%20a%20line%3F` — the two SELECTs at scope 50 pick it up and the demo re-runs against the new question.",
						Ops:    []OpFile{opRepoSelect, opQuestionSelect, opMcpQuery, opMcpRender},
						Method: "GET",
						Path:   "/",
					},
				},
			},
		},
	}
	// Inject stack names: `<trackID>-<i+1>`, e.g. build-1, api-3.
	// Done here rather than statically so authors who add steps don't
	// have to remember to also set the stack on each.
	for ti := range c.Tracks {
		for si := range c.Tracks[ti].Steps {
			c.Tracks[ti].Steps[si].Stack =
				c.Tracks[ti].ID + "-" + itoa(si+1)
		}
	}
	return c
}

// itoa converts a small positive int (step index, 1..N) to its decimal
// string. Inlined to avoid pulling in fmt / strconv just for this.
// Callers are bounded by tutorial size, so a few hundred is the
// realistic upper bound; we keep it simple and base-10.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
