<!-- nav: Builtins -->

# EXEC Schemes and Builtins


## The schemes

| Scheme | Runs | Reference |
|---|---|---|
| `http(s)://…` | Your service, over HTTP | [ops](../ops.md) |
| `op://NAME` | Sandboxed wasm nano-op on the chassis | [ops](../ops.md), `sdk/op` |
| `txco://…` | A chassis builtin (table below) | this page |
| `ai://chat` | A chat model via the chassis's AI registry | [ai](../ai.md) |
| `mcp+http(s)://…` | A tool on an external MCP server | [mcp](./protocols/mcp.md) |
| `<stack>/<scope>` | Unschemed stage jump (synthesized into `@goto`) | [resonators](../resonators.md) |

## The builtin registry

| Builtin | What it does |
|---|---|
| `txco://noop` | Returns `{}`. Placeholder / structural. |
| `txco://static` | Serve static files with layered lookup: the stack's `FILES/` → workspace `FILES/` → embedded defaults. Caps: 1 MiB/file, 2048 files, 64 MiB total. See `examples/quickstart-hello-world` for the rule pattern. |
| `txco://read-file` | Read a stack's `FILES/` asset(s) into the document as data (templates, fixtures, config) — the read-into-the-tree counterpart to `static`. See [read-file](./read-file.md). |
| `txco://web-render` | Read a source path, optionally render Markdown→HTML, set `@web.res.*`, halt. Pages without a backend. |
| `txco://sendmail` | Render + submit outbound email from the `_sendmail` contract — see [sendmail](./protocols/sendmail.md). |
| `txco://relay` | Forward an inbound message VERBATIM (the `.forward` primitive) — see [relay](./protocols/relay.md). Only fires from the inbound-mail path (LMTP). |
| `txco://hmac-sign` | Compute an HMAC signature (key via `WITH secrets.*`). |
| `txco://hmac-verify` | Verify an HMAC, constant-time; result lands under `@computed.*`. |
| `txco://basic-auth-encode` | Encode `user:pass` to a basic-auth header value. |
| `txco://basic-auth-verify` | Check an inbound `Authorization: Basic …` header against a user and a secret password, constant-time; only the verdict lands under `@computed.*` (`basic_auth_ok`, `basic_auth_configured`). With `secrets.password.optional = true` + `allow_unconfigured = true` an unset secret leaves the route open — the demo/dev shape. |
| `txco://copy` | Path-to-path copy inside the envelope (what `SET` can't do with computed paths). |
| `txco://kv/get` · `kv/set` · `kv/delete` · `kv/incr` · `kv/cas` | Read + write durable state across requests — counters, flags, locks, caches (`boltdb` local / `redis` shared). See [kv](./kv.md). |
| `txco://blob/put` · `blob/get` · `blob/stat` · `blob/list` · `blob/delete` | Runtime-writable BYTES under mutable, permissioned names over the content-addressed store — uploads, documents, artifacts; seeded with a stack via `BLOBS/`. See [blobs](./blobs.md). |
| `txco://imap/account` · `imap/append` · `imap/mailbox` · `imap/remove` · `imap/flags` · `imap/list` · `imap/messages` · `imap/get` | Provision an IMAP account (argon2id, its INBOX), materialize messages (a RECORD or verbatim bytes) into mailboxes the `imap` personality serves to any mail client, manage role-tagged folders with per-verb policy, and read the store back. See [imap](./protocols/imap.md). |
| `txco://websocket/accept` · `websocket/send` · `websocket/reply` · `websocket/close` | Live sessions: in the upgrade request's own run, `accept` takes the connection (WITH `state`, `origins`, `subprotocols`, `events`); then every message is one run of `<stack>/_websocket` and `reply` (or `send` by `session_id`) writes back on the socket. See [websocket](./protocols/websocket.md). |
| `txco://detect-tenant` | Boot-pipeline: hostname/listener → tenant resolution. Used by the scaffolded `_sys/boot` rules; you rarely call it directly. |
| `txco://route` | Boot-pipeline: promote a routing proposal (`@route.*`) into `@goto` + `@tenant`. Companion to `detect-tenant`. |
| `txco://continuation-result` | Poll handler behind `?_txc.continuation=<id>` ([continuations](../continuations.md)). Wired by the chassis; not called from rules. |

Builtins pay normal [fuel](./fuel.md) and appear in
[traces](./trace.md) like any other op.