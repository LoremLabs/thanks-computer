<!-- nav: Builtins -->

# EXEC Schemes and Builtins


## The schemes

| Scheme | Runs | Reference |
|---|---|---|
| `http(s)://…` | Your service, over HTTP | [ops.md](../ops.md) |
| `op://NAME` | Sandboxed wasm nano-op on the chassis | [ops.md](../ops.md), `sdk/op` |
| `txco://…` | A chassis builtin (table below) | this page |
| `ai://chat` | A chat model via the chassis's AI registry | [ai.md](../ai.md) |
| `mcp+http(s)://…` | A tool on an external MCP server | [mcp.md](./protocols/mcp.md) |
| `<stack>/<scope>` | Unschemed stage jump (synthesized into `@goto`) | [resonators.md](../resonators.md) |

## The builtin registry

| Builtin | What it does |
|---|---|
| `txco://noop` | Returns `{}`. Placeholder / structural. |
| `txco://static` | Serve static files with layered lookup: the stack's `FILES/` → workspace `FILES/` → embedded defaults. Caps: 1 MiB/file, 2048 files, 64 MiB total. See `examples/quickstart-hello-world` for the rule pattern. |
| `txco://web-render` | Read a source path, optionally render Markdown→HTML, set `@web.res.*`, halt. Pages without a backend. |
| `txco://sendmail` | Render + submit outbound email from the `_sendmail` contract — see [sendmail.md](./protocols/sendmail.md). |
| `txco://hmac-sign` | Compute an HMAC signature (key via `WITH secrets.*`). |
| `txco://hmac-verify` | Verify an HMAC, constant-time; result lands under `@computed.*`. |
| `txco://basic-auth-encode` | Encode `user:pass` to a basic-auth header value. |
| `txco://copy` | Path-to-path copy inside the envelope (what `SET` can't do with computed paths). |
| `txco://detect-tenant` | Boot-pipeline: hostname/listener → tenant resolution. Used by the scaffolded `_sys/boot` rules; you rarely call it directly. |
| `txco://route` | Boot-pipeline: promote a routing proposal (`@route.*`) into `@goto` + `@tenant`. Companion to `detect-tenant`. |
| `txco://continuation-result` | Poll handler behind `?_txc.continuation=<id>` ([continuations](../continuations.md)). Wired by the chassis; not called from rules. |

Builtins pay normal [fuel](./fuel.md) and appear in
[traces](./trace.md) like any other op.