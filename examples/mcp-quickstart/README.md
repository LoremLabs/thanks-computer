# mcp-quickstart

Compose a real public MCP server inside a txcl rule, render its
markdown answer as HTML, and return it to the browser — all in
three small rule files. The chassis EXECs into
[DeepWiki](https://mcp.deepwiki.com) (a free, no-auth, AI-powered
docs server for public GitHub repositories) and renders the
result.

## What this shows

```
client ──HTTP─► chassis ──mcp+https://mcp.deepwiki.com/mcp#ask_question──► DeepWiki
                  │                                                            │
                  │  initialize → notifications/initialized → tools/call       │
                  │◄─────────────  result.content[0].text  ────────────────────│
                  │
                  ▼
              .text = "# Answer …"     (markdown from DeepWiki)
                  │
                  │  scope 200: txco://web-render markdown → HTML
                  ▼
              200 OK, text/html
              <h1>Answer</h1> …
```

Rule files, ordered by scope:

- `OPS/mcp-demo/50/repo.txcl` and `…/50/question.txcl` — resolve
  each field from `?repoName=…` / `?question=…` query param,
  falling back to a default. Same scope, parallel-safe (different
  destination paths).
- `OPS/mcp-demo/100/query.txcl` — invoke the MCP tool.
- `OPS/mcp-demo/200/render.txcl` — when DeepWiki returned text,
  render it as HTML.
- `OPS/mcp-demo/200/notfound.txcl` — when it didn't, return 404.

Plus a tiny boot hook so the example works without a hostname
bind: `OPS/_sys/boot/75/auto-route.txcl`.

## Run it

Prerequisites: `txco` on your PATH.

```sh
# 1. Copy the workspace.
cp -r examples/mcp-quickstart ~/my-workspace
cd ~/my-workspace

# 2. Look at what DeepWiki exposes (no chassis needed for this).
txco mcp doctor https://mcp.deepwiki.com/mcp
# server: DeepWiki 2.14.3  (protocol 2025-06-18)
# session: (stateless)
#
# tools (3):
#   - read_wiki_structure  — Get a list of documentation topics …
#   - read_wiki_contents   — View documentation about a GitHub repository.
#   - ask_question         — Ask any question about a GitHub repository …
#       inputs: repoName, question

# 3. Run the chassis.
txco dev --apply

# 4. Three ways to ask a question, all returning rendered HTML:

# Defaults (no body, no query) — answers "What is kudos?" about loremlabs/kudos:
curl -s http://localhost:8080/

# Query params (browser-friendly — paste this URL into a browser too):
curl -sG http://localhost:8080/ \
  --data-urlencode 'repoName=facebook/react' \
  --data-urlencode 'question=What is JSX?'

# JSON body (programmatic — the original shape):
curl -s http://localhost:8080/ \
  -H 'Content-Type: application/json' \
  -d '{"repoName":"facebook/react","question":"What is JSX?"}'

# Any of the three returns:
# <!doctype html><html><body><h1>Answer</h1>
# <p>JSX is a syntax extension to JavaScript that …</p>
# …
```

The first request takes ~10–20s because DeepWiki runs an LLM to
answer your question. Subsequent questions to the same repo are
faster (their cache, not ours).

## What the rules do

### scope 50 — resolve inputs (query param or default)

A request can arrive three ways: bare `GET /` (no inputs), a `GET`
with query params (`?repoName=…&question=…`), or a `POST` with a
JSON body. Scope 50 handles the first two with one rule per
field. `OPS/mcp-demo/50/repo.txcl`:

```txcl
SELECT @web.req.url.query.repoName.0
    AS .repoName
    DEFAULT "facebook/react"
```

`SELECT` copies the value at the source path into the destination
path. When the source resolves empty / missing, `DEFAULT`'s literal
is substituted. One rule expresses "use the query param if present,
else fall back."

A few things to know about the path syntax:

- `@` is sugar for `_txc.` — the same convention `WHEN @src == …`
  uses. So `@web.req.url.query.repoName.0` reads
  `_txc.web.req.url.query.repoName.0`.
- The web inlet parses the URL query string into
  `_txc.web.req.url.query.<key>` — each value is a **JSON array**
  because HTTP allows `?repoName=a&repoName=b`. `.0` picks the
  first entry.
- When no query param is present, that path is missing → empty
  string → `default` kicks in.

`question.txcl` mirrors this for `.question`. The two scope-50
rules write different paths so they're safe to run in parallel
within the same scope; no ordering needed.

For a JSON POST, none of this matters: the body is base64-encoded
into `_txc.web.req.body` by the inlet, and the chassis decodes it
directly as `params.arguments` at scope 100 — the body wins,
scope-50 envelope work is bypassed for the MCP call.

### scope 100 — invoke the tool (asynchronously)

`OPS/mcp-demo/100/query.txcl`:

```txcl
EXEC "mcp+https://mcp.deepwiki.com/mcp#ask_question"
  WITH mode = "async",
       timeout = "60s",
       debug = true
```

`mode = "async"` is the important bit — DeepWiki's LLM round-trip
takes 10–30 seconds, and holding the client's HTTP connection open
for that whole time is poor UX (and a stress test for every proxy
between them). With async on, the chassis:

1. Synthesizes a continuation handle (`rcid`) and returns a
   `202 Accepted` to the client immediately, with a polling URL:
   `Location: /?_txc.continuation=<rcid>`. Browsers see a redirect
   to a built-in waiting page that polls the same URL.
2. Spawns a background goroutine that runs the actual MCP
   lifecycle:
   - POST `initialize` to `https://mcp.deepwiki.com/mcp` with
     `Accept: application/json, text/event-stream`. DeepWiki
     responds in SSE; the chassis transparently unwraps.
   - POST `notifications/initialized`.
   - POST `tools/call` with `params.name = "ask_question"` and
     `params.arguments` = your envelope (minus `_txc.*`).
   - Read the JSON-RPC reply; project the text content block
     into `{"text": "..."}` on the envelope.
3. When the goroutine finishes, it writes the result to the
   continuation store and triggers the suspended pipeline to
   resume from scope 200.
4. The client's next poll sees the run completed and gets the
   rendered HTML response from scope 200 — same envelope, same
   result, just delivered after the round-trip rather than over
   one held-open connection.

The whole lifecycle remains three HTTPS requests per `EXEC` —
correct per the MCP spec but deliberately unoptimized. A session
cache (per tenant + endpoint) is a future enhancement that
doesn't change the rule.

Drop `mode = "async"` and you're back to the synchronous path:
the chassis holds the request open until DeepWiki answers.

### scope 200 — render OR not-found

`OPS/mcp-demo/200/render.txcl`:

```txcl
WHEN .text != ""
  EXEC "txco://web-render" WITH source = ".text", wrap = "markdown-to-html"
```

`txco://web-render` is a chassis core op (alongside
`txco://route`, `txco://static`). It reads `.text` from the
envelope, renders the markdown as HTML via
[goldmark](https://github.com/yuin/goldmark), base64-encodes
the bytes, and writes the web-response shape:
`_txc.web.res.body`, `_txc.web.res.status = 200`,
`_txc.web.res.headers.content-type = "text/html; charset=utf-8"`,
plus `_txc.halt = true` to end the pipeline.

`OPS/mcp-demo/200/notfound.txcl` is the gated complement:

```txcl
WHEN .text == ""
  EMIT @web.res.status = 404, …, @halt = true
```

Same scope, opposite `WHEN`. Exactly one of the two rules
fires per request.

## Try a different tool

Edit `txco.yaml` and change the fragment:

```yaml
operations:
  ASK_REPO:
    url: mcp+https://mcp.deepwiki.com/mcp#read_wiki_structure
```

…then POST `{"repoName":"facebook/react"}` for the list of
topics. `txco mcp doctor https://mcp.deepwiki.com/mcp` shows
every tool and its required inputs.

The `txco://web-render` op accepts `wrap = "raw"`,
`wrap = "html"` (escape and wrap in `<pre>`), and
`wrap = "markdown-to-html"` — pick the one that fits your
tool's output shape.

## Generalizing: `SELECT`

`SELECT … AS … DEFAULT …` is the path→path copy primitive that
closes the "txcl SET RHS is literal-only" gap:

```txcl
SELECT .text AS @computed.answer
```

Useful any time you need to move an envelope value somewhere
the surrounding rule's `SET`s can't reach. Multiple assignments
work in one statement:

```txcl
SELECT .a AS .x, .b AS .y DEFAULT "fallback"
```

No `EXEC` is required — `SELECT` is an envelope mutation that
commits on its own.

## Adding bearer-token auth (for non-public servers)

DeepWiki needs no authentication. For private MCP servers,
declare a per-tenant secret and reference it from the rule:

```sh
# Mint the secret (one-time).
txco auth secrets put my-mcp-key --value "live_sk_abc..."
```

Then `OPS/mcp-demo/100/query.txcl` becomes:

```txcl
EXEC "mcp+https://api.example.com/mcp#tool-name" WITH timeout = "60s",
     secrets.headers.authorization.secret = "my-mcp-key",
     secrets.headers.authorization.format = "Bearer {}"
```

The chassis materializes the cleartext only into the outgoing
request's `Authorization` header. It never lands in the
JSON-RPC body, the trace, or the run state. (Body-path secret
refs are silent no-ops on this scheme — see
`internal docs/todo-mcp.md` §3.2.)

## See also

- `internal docs/todo-mcp.md` — the MCP feasibility / shape design doc.
- `internal docs/todo-mcp-implementation.md` — PR-shaped implementation
  plan.
- `txco mcp doctor --help` — discovery / health-check CLI.
- [DeepWiki](https://mcp.deepwiki.com) — the MCP server this
  example talks to. Their backend uses [Devin](https://devin.ai)
  to generate answers from indexed GitHub repository contents.
