<!-- nav: Builtins -->

# EXEC Schemes and Builtins


## The schemes

| Scheme | Runs | Reference |
|---|---|---|
| `http(s)://…` | Your service, over HTTP | [ops](../ops.md) |
| `op://NAME` | Sandboxed wasm nano-op on the chassis | [ops](../ops.md), `sdk/op` |
| `txco://…` | A chassis builtin (table below) | this page |
| `ai://chat` | A chat model via the chassis's AI registry | [ai](../ai.md) |
| `ai://embed` | Text to vectors with an embedding model, to store with `txco://vector/*` | [ai](../ai.md#embeddings--exec-aiembed) |
| `ai://decide` | A typed decision from a model: one of your labels, with probabilities | [ai](../ai.md#decisions--exec-aidecide) |
| `workspace://<name>/<verb>` | A command, a service or a terminal in an owned, stateful environment (`exec`, `connect`, `attach`, and the lifecycle verbs `create`, `wake`, `sleep`, `checkpoint`, `destroy`) | [workspaces](../workspaces.md) |
| `outlet://<name>/<op>` | A declared external service, such as Postgres, through a pool the chassis holds | [outlets](../outlets.md) |
| `mcp+http(s)://…` | A tool on an external MCP server | [mcp](./protocols/mcp.md) |
| `cap://NAME` | On a node: a capability of its parent chassis, decided there under the run's grant | [capabilities](./capabilities.md#cap) |
| `goto://<stack>/<scope>`, `goto://<scope>` | Stage jump, the same as `EMIT @goto`; a bare scope is in the current stack | [txcl](./txcl/txcl.md#control-flow-via-_txc) |
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
| `txco://copy` | Path-to-path copy inside the envelope (what `SET` can't do with computed paths). `to` must be your own key or a writable `_txc` field (`@web.res.body`); a reserved one fails the op. |
| `txco://kv/get` · `kv/set` · `kv/delete` · `kv/incr` · `kv/cas` · `kv/mget` · `kv/mset` · `kv/mdelete` · `kv/list` | Read + write durable state across requests — counters, flags, locks, caches (`boltdb` local / `redis` shared); read, write or delete many keys across namespaces in one dispatch (writes and deletes atomically), or list a namespace a sorted page at a time, with values. See [kv](./kv.md). |
| `txco://blob/put` · `blob/get` · `blob/stat` · `blob/list` · `blob/delete` | Runtime-writable BYTES under mutable, permissioned names over the content-addressed store — uploads, documents, artifacts; seeded with a stack via `BLOBS/`. See [blobs](./blobs.md). |
| `txco://notebook/append` · `notebook/read` · `notebook/export` · `notebook/list` · `notebook/delete` | An append-only record per (tenant, namespace, name) — task history, conversation history, audit breadcrumbs — read back by cursor, time window or tail (always oldest first) and exported as NDJSON. A duplicate `object_key` returns the original entry. See [notebooks](./notebooks.md). |
| `txco://state/create` · `state/get` · `state/transition` | A durable state record per (machine, id) — a state, a version and opaque JSON data — changed only by compare-and-swap on both the state and the version; every committed transition writes an event in the same transaction that the `state` personality presents into the tenant's `_state` stack, at least once and in version order. See [state](./protocols/state.md). |
| `txco://drive/collection` · `drive/account` · `drive/put` · `drive/get` · `drive/stat` · `drive/list` · `drive/delete` · `drive/mkdir` · `drive/move` · `drive/copy` · `drive/sign` | A mutable document space — folder trees a client mounts over WebDAV (the `webdav` personality) and a stack writes, reads, lists (`since` a sync token, deletions included), moves and copies, and hands out by reference (`sign`: a short-lived URL for one exact document, so a large file never rides an envelope); a resource keeps its `resource_id` across renames and every mutation is a `_scheduled` event. See [drive](./drive.md). |
| `txco://imap/account` · `imap/append` · `imap/mailbox` · `imap/remove` · `imap/flags` · `imap/list` · `imap/messages` · `imap/get` | Provision an IMAP account (argon2id, its INBOX), materialize messages (a RECORD or verbatim bytes) into mailboxes the `imap` personality serves to any mail client, manage role-tagged folders with per-verb policy, and read the store back. See [imap](./protocols/imap.md). |
| `txco://calendar/account` · `calendar/calendar` · `calendar/put` · `calendar/get` · `calendar/list` · `calendar/delete` | Provision a calendar account and calendars, and put events into them; the `calendar` personality serves them to CalDAV clients and as ICS feeds. See [calendar](./protocols/calendar.md). |
| `txco://contacts/account` · `contacts/addressbook` · `contacts/put` · `contacts/get` · `contacts/list` · `contacts/delete` · `contacts/sync` | The same for address books: cards a stack puts in, served to CardDAV clients by the `contacts` personality. See [contacts](./protocols/contacts.md). |
| `txco://ipp/printer` | Register a printer the `ipp` personality serves: the label in its URL, the principal that may print to it, and the name a computer gives the queue. A printer holds no password — its principal signs in with a credential whose scopes cover `ipp:<printer>:print` — and a label with no active printer is a 404. Idempotent; `status = "disabled"` and `delete = true` turn one off. See [IPP](./protocols/ipp.md#the-printer-op). |
| `txco://user/create` · `user/get` · `user/disable` · `credential/create` · `credential/list` · `credential/revoke` | Your product's users and the revocable, scoped passwords they (and ponies, services) sign in with: one person is one user across the tenant, with a credential per device. A password is generated, shown once, and carries its credential's id. Only the stack that created a principal may change it. See [users & credentials](./users.md). |
| `txco://grant/put` · `grant/list` · `grant/revoke` · `delegate/mint` · `delegate/get` · `delegate/revoke` · `delegate/close` | What a principal, and one piece of work dispatched for it, may be handed by this chassis. A standing grant names one capability or secret a principal may ever ask for; a run grant names the sandboxes (`SANDBOXES/<name>.yaml`: what a program holds, under which names) one piece of work may open, until when and within what budget, and can only narrow the standing grants behind it. Ending a run grant takes effect on the work's next request. See [grants](./grants.md). |
| `txco://vector/collection` · `vector/upsert` · `vector/update` · `vector/delete` · `vector/search` | The tenant's vector store: collections pinned to a model and dimension, items with embeddings and metadata, nearest-neighbour search with filters. Producing the vectors is `ai://embed`'s job. See [vectors](../vectors.md). |
| `txco://search/collection` · `search/upsert` · `search/update` · `search/delete` · `search/query` | The tenant's lexical search store: index text records and find them by the words they contain — names, filenames, identifiers, quoted phrases. See [search](../search.md). |
| `txco://dataset` | Run a named query against a read-only SQLite dataset the stack ships under `DATASETS/`. See [datasets](../datasets.md). |
| `txco://schedule` | Enqueue an event to fire into the tenant's `_scheduled` stack no earlier than a given time; the `scheduled` personality fires it. See [scheduled](./protocols/scheduled.md). |
| `txco://mock` | Serve this scope's `mock-response.json` verbatim, for working on one step without its real service; with no fixture it fails loudly. See [mocks](../authoring/mocks.md). |
| `txco://run/abort` | End a run in flight on this chassis, from a rule: `WITH rid = <rid>` or `stack = <name>`, `reason` (optional), `into` (default `_abort`). The calling tenant's runs only; a hard stop, as `txco abort` — ops in flight cancelled, nothing later runs, the trace says `aborted` by `<tenant>/<stack>`. A run may abort itself. Answers `{aborted: n, rid|stack}` or `<into>.error.{code,message}` (`txco_run_invalid_arg`, `txco_run_not_live`, `txco_run_no_tenant`). This process only: on a fleet, a run on another node is reached through the admin plane's abort. What lets a stack build its own stop with its bookkeeping first — see [aborting a run](../abort.md). |
| `txco://caps/list` | The tenant's capability catalogue: what its active stacks declare under `CAPS/<name>.yaml` (a description, its input, where a call enters). `WITH into` (default `_caps`), `prefix` (optional, narrows by name). Answers `{count, items: [{name, stack, entry, stage, description, input, params, timeout}]}`; the same list as `txco caps list`. What exists, not what a run may call: that is the run grant's. See [capabilities](./capabilities.md#the-catalogue). |
| `txco://websocket/accept` · `websocket/send` · `websocket/reply` · `websocket/close` | Live sessions: in the upgrade request's own run, `accept` takes the connection (WITH `state`, `origins`, `subprotocols`, `events`); then every message is one run of `<stack>/_websocket` and `reply` (or `send` by `session_id`) writes back on the socket. See [websocket](./protocols/websocket.md). |
| `txco://detect-tenant` | Boot-pipeline: hostname/listener → tenant resolution. Used by the scaffolded `_sys/boot` rules; you rarely call it directly. |
| `txco://route` | Boot-pipeline: promote a routing proposal (`@route.*`) into `@goto` + `@tenant`. Companion to `detect-tenant`. |
| `txco://continuation-result` | Poll handler behind `?_txc.continuation=<id>` ([continuations](../continuations.md)). Wired by the chassis; not called from rules. |

Builtins pay normal [fuel](./fuel.md) and appear in
[traces](./trace.md) like any other op.

A builtin writes where you point it (`WITH into`, `to`, `output_path`), but
not over the chassis's own `_txc` fields — see
[what you may write under `_txc`](./txcl/txcl.md#control-flow-via-_txc).