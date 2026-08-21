# mcp-server-in-txcl

A complete MCP (Model Context Protocol) server written entirely in
txcl rules — no new `txco://` ops needed. Every protocol primitive
(body decode, JSON-RPC parse, dispatch, response shaping) runs
through the `&fn(...)` expression-language function registry.

This is the inverse of `examples/mcp-quickstart`: that workspace
*calls* an external MCP server (DeepWiki); this workspace *is* an
MCP server, ready for any MCP client (Claude Desktop, Cursor, IDE
plugins, the `txco mcp doctor` CLI, etc.) to connect to.

## What this shows

```
MCP client ──HTTP POST /─► chassis
                          │
                          ▼
                    scope 0:  parse body
                    .rpc = &json(&b64decode(@web.req.body))
                          │
                          ▼
                    scope 100: dispatch by method
                    ├── initialize.txcl    → server info + sid
                    ├── initialized.txcl   → 202 ack
                    ├── tools-list.txcl    → tool catalog
                    ├── tool-echo.txcl     → echo tool result
                    ├── tool-time.txcl     → time tool result
                    ├── unknown-method.txcl → JSON-RPC -32601
                    └── unknown-tool.txcl  → JSON-RPC -32602
                          │
                          ▼
                    scope 200: defensive defaults
                    (status 200, content-type application/json)
                          │
                          ▼
MCP client ◄──HTTP 200, JSON-RPC body ── chassis
```

Rule files, ordered by scope:

- **`OPS/mcp-server/0/parse.txcl`** — base64-decode `@web.req.body`,
  parse the JSON-RPC envelope, stash the result at `.rpc` so every
  later rule can address `.rpc.method`, `.rpc.params.name`,
  `.rpc.params.arguments.text` etc. as ordinary envelope paths.
- **`OPS/mcp-server/100/initialize.txcl`** — JSON-RPC `initialize`.
  Returns server info + capabilities + a session-id header
  (the demo mints a fresh `&uuid()` per init; production would
  HMAC-sign these for stateless validation).
- **`OPS/mcp-server/100/initialized.txcl`** — JSON-RPC
  `notifications/initialized`. Spec says no response body; we
  return 202 with empty body.
- **`OPS/mcp-server/100/tools-list.txcl`** — JSON-RPC `tools/list`.
  Returns the two-tool catalog with input schemas.
- **`OPS/mcp-server/100/tool-echo.txcl`** — the `echo` tool.
  Returns the input text back in a JSON-RPC `result.content` array.
- **`OPS/mcp-server/100/tool-time.txcl`** — the `time` tool.
  Returns the server's current RFC 3339 time.
- **`OPS/mcp-server/100/unknown-method.txcl`** — JSON-RPC -32601
  fallback for unrecognized methods.
- **`OPS/mcp-server/100/unknown-tool.txcl`** — JSON-RPC -32602
  fallback for `tools/call` with an unknown tool name.
- **`OPS/mcp-server/200/respond.txcl`** — defensive defaults: status
  200, content-type application/json, when nothing earlier set them.

Plus a tiny boot hook so the example works on localhost without
hostname bindings: `OPS/_sys/boot/75/auto-route.txcl`.

## What this isn't

- **Not full MCP spec coverage.** No `resources`, no `prompts`,
  no `notifications/progress`, no SSE streaming. The two tools
  (`echo`, `time`) exist to demonstrate end-to-end plumbing.
- **Not session-validated.** The session id minted on `initialize`
  is a random UUID that the server doesn't actually check on
  subsequent requests. Production would HMAC-sign sids so any
  replica can validate without shared state.
- **Not auth-gated.** The chassis's existing HTTP auth applies if
  configured, but the workspace ships nothing.

See `internal docs/todo-mcp-server-in-txcl.md` for the broader design.

## Run it

Prerequisites: `txco` on your PATH.

```sh
# 1. Copy the workspace.
cp -r examples/mcp-server-in-txcl ~/my-mcp-server
cd ~/my-mcp-server

# 2. Run the chassis.
txco dev --apply
```

The server now answers JSON-RPC on `http://localhost:8080/`.

### Dev vs production response shape

By default `txco dev` sets `--web-debug=SHOW_PRIVATE_VARS`, which keeps `_txc.*` and `_ts` in the response body — useful for inspecting the merged envelope while writing rules. Real MCP clients (Claude Desktop, etc.) tolerate the extra keys but the body isn't a strict JSON-RPC reply in that mode.

For production-shape responses (or a cleaner client experience locally), set `--web-debug=HIDE_PRIVATE_VARS` when starting the chassis. With that flag the outlet strips `_`-prefixed top-level keys and the body becomes a pure JSON-RPC envelope. The `notifications/initialized` 202 reply, for example, becomes the 2-byte `{}` instead of the full envelope dump.

### Use the doctor CLI

```sh
# In another terminal:
txco mcp doctor http://localhost:8080/
# server: txco-mcp-server-in-txcl 0.1.0  (protocol 2025-06-18)
# session: <fresh uuid>
#
# tools (2):
#   - echo  — Echo the input text back as the tool result.
#       inputs: text
#   - time  — Return the server's current time in RFC 3339 format.
```

### Call tools by hand

```sh
# initialize (mints a session id)
curl -sS -X POST http://localhost:8080/ \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","capabilities":{},
                 "clientInfo":{"name":"curl","version":"0"}}}'

# initialized notification (no response body, 202)
curl -sS -X POST http://localhost:8080/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# tools/list
curl -sS -X POST http://localhost:8080/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'

# tools/call — echo
curl -sS -X POST http://localhost:8080/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call",
       "params":{"name":"echo","arguments":{"text":"hello mcp"}}}'

# tools/call — time
curl -sS -X POST http://localhost:8080/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":4,"method":"tools/call",
       "params":{"name":"time","arguments":{}}}'

# unknown method (returns JSON-RPC -32601 error)
curl -sS -X POST http://localhost:8080/ \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":5,"method":"nope/wat"}'
```

## How the function registry replaces utility ops

The pre-PR-4 design for this workspace
(`internal docs/todo-mcp-server-in-txcl.md`) proposed four new
`txco://` utility ops to make the rules expressible:
`txco://b64-decode`, `txco://json-parse`, `txco://uuid`,
`txco://now`.

With the expression-language registry from
`internal docs/todo-txcl-expressions.md`, those ops are no longer needed —
the equivalent functionality lives as `&b64decode`, `&json`,
`&uuid`, `&now` in `chassis/txcl/funcs/`. Plus the full set of 18
strict + 5 try_ variants enable patterns the four-op design
couldn't express at all (object construction, array building,
runtime path access via `&get`/`&set`/`&has`, etc.).

Per `docs/txcl.md` §Functions, the function registry is curated
chassis-shipped code — operators don't extend it. New functions
get added with discipline; the bar is "side-effect-free,
generally useful across protocol patterns."

## How to add a new tool

1. Create `OPS/mcp-server/100/tool-<name>.txcl` modeled on
   `tool-echo.txcl`. The WHEN is
   `.rpc.method == "tools/call" && .rpc.params.name == "<name>"`.
2. Add the tool's metadata (name, description, inputSchema) to
   `OPS/mcp-server/100/tools-list.txcl` so `tools/list` advertises
   it.
3. Add the tool name to the AND chain in
   `OPS/mcp-server/100/unknown-tool.txcl` so the -32602 fallback
   doesn't fire for it.

That's it. The new tool participates in dispatch the same way
existing ones do.
