# workspace-node

A chassis running **inside** a workspace (a *node*), set up and called by the
chassis that owns the workspace (its *parent*). The parent installs the release
binary, starts it, enrolls its own public key on it, gives it stacks, and calls
it, and none of that needs a chassis change: it is built from
`workspace://<name>/exec`, the release, the admin plane's signed requests and
the package CLI.

```text
   PARENT CHASSIS                         WORKSPACE
   this stack                             ┌──────────────────────────────┐
     POST /setup    ── exec ────────────▶ │ setup.sh                     │
     POST /packages ── exec ────────────▶ │ txco install · txco apply    │
     POST /call     ── exec: curl -K - ─▶ │ NODE CHASSIS                 │
                       a signed request   │   admin 127.0.0.1:8927       │
                                          │   web   127.0.0.1:8926       │
                                          │   stacks: hello · support ·  │
                                          │           _inspect           │
                                          └──────────────────────────────┘
```

> **Requires a real workspace provider (Sprites).** Setup installs a Linux
> release of `txco` and runs it as a long-lived process. The script refuses
> anything that is not Linux, so with the local provider on a Mac it installs
> nothing. This example is therefore **not part of
> `scripts/examples-smoke.sh`**. Proven end to end on a real sprite
> (2026-09-30).

It is a demonstrator for `thanks-computer-service/docs/todo-chassis-hierarchy.md`
(spike 1). It shows the way **down**. The way up, where a program on the node
asks the parent for something by run grant, is not built.

## The shape

```
POST /setup     → {"parent_pub": …}: provision (once), cold start, or a warm
                  no-op. Runs DETACHED and returns at once.
GET  /setup     → poll until {"ready":true,"parent_key_id":…}. Also what
                  restarts a node whose workspace went cold.
GET  /status    → the node, measured on the node
POST /packages  → install packages on the node and apply them there
POST /call      → a request to the node's admin plane, signed with the
                  parent's key and carried down by exec
GET  /unsigned  → the same request with no signature: 401
GET  /facts     → what the workspace allows a node: users, control groups,
                  egress, and what it does to a process while idle
GET  /logs      → the node chassis's own log, and the browser stack's
GET  /ui        → a page: the node's admin UI, in a browser on the node's
                  workspace, shown in yours
GET  /ui/setup  → start that browser (installs it once, about two minutes)
POST /ui/open   → sign it in to the node's admin UI, at the traces view
WS   /screen    → the workspace's display, as a VNC stream
GET  /ui/shot   → a PNG of what that browser shows
GET  /terminal  → a page: a shell on the node's workspace
WS   /term      → that shell, as a PTY
POST /stop      → stop the node chassis, as a cold workspace would have; the
                  next GET /setup cold-starts it
POST /destroy   → destroy the workspace, and the node with it
```

| Piece | What it does |
|---|---|
| `node/090/auth.txcl` / `095/auth_reject.txcl` | the HTTP Basic gate over every route but `/healthz` (fails closed, see below) |
| `node/097/parse.txcl` | decodes the `POST /setup` body |
| `node/100/setup.txcl`, `ready.txcl` + `setup.sh` | the three cases of setup; what is installed is `setup.txcl`'s `env` (data), the script is fixed mechanism, fed on `stdin` to `bash -s` |
| `node/100/status.txcl` + `status.sh` | both planes' `/healthz`, the release, the process, the disk, and whether enrollment is closed |
| `node/100/packages.txcl` + `packages.sh` | three sources of stacks, then `txco apply` on the node |
| `node/100/call.txcl` / `unsigned.txcl` | the signed call down, and its unsigned twin |
| `node/100/facts.txcl` + `facts.sh` | measurements |
| `node/100/stop.txcl` / `logs.txcl` / `destroy.txcl` | one exec or one verb each |
| `node/100/ui.txcl` + `ui.html` | the noVNC page, served behind the gate |
| `node/100/ui_setup.txcl` + `browser.sh` | Chrome, Xvfb and x11vnc on the node's workspace: [`workspace-browser`](../workspace-browser)'s setup without its harness |
| `node/100/ui_open.txcl`, `ui_shot.txcl` + `ui.py` | drive that browser over its debugging port: sign it in, or take a screenshot |
| `node/100/screen.txcl`, `node/_websocket/200/connect.txcl` | accept the WebSocket, then `workspace://node/connect WITH service = "browser"` |
| `node/100/terminal.txcl` + `terminal.html` | the xterm.js page, served behind the gate |
| `node/100/term.txcl`, `node/_websocket/200/attach.txcl` | accept the WebSocket, then `workspace://node/attach` with `tmux` |
| `node/_websocket/100/parse.txcl` | reads a socket's first message, which says what it wants bound: `connect` or `attach` |
| `node/200/*.txcl` | the answers: setup's JSON line, a command's output as text, a PNG, a 503 on a transport failure |
| `node/300/no_route.txcl` | a 404 that lists the routes when nothing matched. Without it a wrong method gets an empty `200 {}`, which reads as success. Every rule that answers ends with `@halt = true`, so an answered request never reaches it. |

## Provision, cold start, warm

A workspace is frozen within seconds of its last `exec` and resumes on the next
one. It may also come back cold: its processes gone and its disk kept. So a
node is kept up by **one idempotent exec with three cases**, the same shape as
[`workspace-browser`](../workspace-browser):

- **Provision** — the marker `/var/lib/txco/version` differs from the `REQ`
  and `VERSION` a `POST /setup` carries:
  1. Make `/var/lib/txco`, mode 0700.
  2. Fetch the release from GitHub into a staging directory, as the
     workspace's own user: resolve `VERSION` to a tag (`latest` is whatever
     `/releases/latest` redirects to), download
     `txco_<version>_linux_<arch>.tar.gz` and that release's `checksums.txt`,
     and check the tarball's sha256 against its own line.
  3. If the staged binary runs, stop the chassis and put the binary at
     `/opt/txco/bin/txco`. If anything before this failed, the node keeps
     running what it had.
  4. Start the chassis **with** a random enroll secret.
  5. Enroll the parent's public key, and a node-local key for `txco apply`.
  6. Start the chassis again **without** the secret. Enrollment is now closed.
  7. Write the marker.
- **Cold start** — provisioned, but the admin plane's `/healthz` does not
  answer: start the chassis.
- **Warm** — `/healthz` answers: nothing to do.

Provision and cold start run **out of band** (`setsid bash
/tmp/txco-node-provision.sh`, logging `PHASE` lines to
`/tmp/txco-node-setup.log`). The hosted edge allows 20 s for response headers,
so `/setup` returns at once and `GET /setup` reports the latest phase.

**What a node runs is written in one place:** `setup.txcl`.

| Setting | Holds |
|---|---|
| `VERSION` | `latest`, or a **tag**: `0.2.31`, `v0.2.31`, `v0.2.32-rc1`. The parent decides what its nodes run. |
| `REQ` | The setup's own version. Bump it to provision again. |

- **A build of your own is a tag.** Pushing any `v*` tag builds all four
  platforms and publishes them with their checksums
  (`.github/workflows/release.yml`). A tag with a hyphen, such as
  `v0.2.32-node.1`, is a prerelease: it never becomes `latest` and does not
  move the Homebrew formula. Name it in `VERSION` and only the nodes you
  point at it run it.

- **`latest` is resolved when a node is provisioned, not after.** A
  provisioned node keeps its release until `REQ` or `VERSION` changes, or the
  workspace is re-created. Bump `REQ` to move nodes to a newer release.
- **Provisioning again keeps the node's keys and stacks.** It reinstalls the
  binary and restarts the chassis, in about three seconds. While it runs,
  `GET /setup` reports the phase, not the old chassis's "ready".
- **A provision that fails leaves the node as it was.** A tag that does not
  exist stops at the download; the chassis is never stopped, and `GET /setup`
  answers `"ready":true` with the release it still runs and a `setup_error`
  that stays until a provision succeeds.
- **The checksum trusts the release as published.** It comes from the same
  release as the tarball, so it catches a damaged download, not a replaced
  one. Once a node and its parent speak a protocol to each other, their
  versions should match.
- **A provision depends on GitHub and nothing else.** The install is a few
  lines of `setup.sh`; no installer script is fetched.

**The chassis is started like this,** from `/var/lib/txco`:

```sh
setsid /opt/txco/bin/txco serve \
  --env=prod \
  --admin-addr 127.0.0.1:8927 --web-addr 127.0.0.1:8926 \
  --auth-mode=signed --personalities=web,admin,grant \
  --structured-host-suffix=.localhost \
  --trace-mode=full \
  --workspace-provider=local --workspace-allow-local \
  </dev/null >>node.log 2>&1 &
```

- **The working directory is the data directory.** The chassis has no single
  data flag; its paths default to `./chassis/data/*`.
- **`--env=prod` is the posture, not a label.** The default, `dev`, is for a
  laptop. It answers with the chassis's private keys (`_txc`, `_ts`) in every
  response, logs every envelope in full at debug, and leaves the admin plane
  open unless something else closes it. A node is a deployment, so it runs as
  one: a stack's response is what the stack emitted (`{"say":"hello world"}`),
  and the log is JSON at info.
- **The environment also names the two database files.** A node that moves
  from one environment to another starts with empty stores: a new key id for
  the parent's key, a fresh node-local key, and no stacks until `/packages`
  runs again.
- **`--auth-mode=signed`**: every admin request is signed, or carries a
  browser session the node issued.
- **`--trace-mode=full`**, so the node's admin UI has traces to show.
- **Loopback only.** The workspace has no inbound route, and the parent reaches
  the node through the provider's `exec`.
- **`local`** is the node's own workspace provider, so a node stack can act on
  the machine it runs on. The `_inspect` stack below does.
- **Detached from the exec's stdio.** A process that keeps the exec's output
  open holds the exec until its timeout.

## Run it (against a real provider)

Point a chassis at a real workspace provider (see
`thanks-computer-service/docs/runbook-workspaces-prod.md`), apply this stack,
and set the password:

```sh
txco auth tenant secrets set EXAMPLE_NODE_PW --tenant <this stack's tenant>
```

**Setup.** `parent_pub` is the base64 of an ed25519 public key whose private
half you hold. It stands in for the parent's key.

```sh
curl -u human:$PW -X POST $URL/setup -d '{"parent_pub":"<base64 public key>"}'
curl -u human:$PW $URL/setup      # until {"ready":true,"parent_key_id":"key_…"}
curl -u human:$PW $URL/status
```

**Stacks.**

```sh
curl -u human:$PW -X POST $URL/packages
```

**A call down.** Sign one request for the address the node hears
(`127.0.0.1:8927`) with the key id setup returned, write it as a curl
configuration, and post that:

```text
url = "http://127.0.0.1:8927/v1/tenants/default/inspect"
request = "POST"
header = "Content-Type: application/json"
header = "Content-Digest: sha-256=:…:"
header = "Signature-Input: sig1=(\"@method\" \"@path\" \"@query\" \"@authority\" \"content-digest\");created=…;keyid=\"key_…\";alg=\"ed25519\";nonce=\"…\""
header = "Signature: sig1=:…:"
data-binary = "{\"stack\":\"node\",\"noun\":\"machine\",\"id\":\"1\"}"
```

```sh
curl -u human:$PW -X POST $URL/call --data-binary @request.cfg
curl -u human:$PW $URL/unsigned
```

The chassis has no command that signs a request without sending it. The
signing code is `chassis/cli/signer`: `NewFileKeySignerFromKey(keyID,
key).Sign(req, body)` sets the three headers on an `*http.Request`. The
signature covers `@authority`, so the request must be signed for the host and
port the node receives.

## Looking at the node: `/ui`

The node's admin UI is at `http://127.0.0.1:8927/admin/`, on the node's own
loopback, and the workspace has no inbound route. Nobody outside can open it.
What can is a browser running beside it, and the chassis can already show a
workspace's browser to a person: that is `workspace-browser`.

```text
your browser ── WebSocket /screen ──▶ parent ── connect "browser" ──▶ WORKSPACE
   noVNC                                                              x11vnc :5900
                                                                         │
                                                                       Chrome ──▶ node admin UI
                                                                                  127.0.0.1:8927/admin/
```

Open `/ui` in a browser and press **open the node's UI** (or open `/ui#go`,
which starts at once). While it works the page shows a spinner, the step it is
on, and the last line the workspace reported. The page:

1. Checks the node is up (`GET /setup`).
2. Starts the browser on the workspace (`GET /ui/setup`). The first time it
   installs Chrome, Xvfb, x11vnc and openbox, which took about two minutes.
3. Signs that browser in (`POST /ui/open`). On the node, `txco ui --no-open`
   asks the node's admin plane for a one-time sign-in link, signed with the
   node-local key, and `ui.py` points Chrome at it. The link is used on the
   node and never leaves it.
4. Opens `/screen` and binds the workspace's display. From then on the socket
   is the VNC stream, and you are using the node's admin UI: its stacks, its
   secrets, and its **traces**, which is where the browser lands.

The node keeps full traces (`--trace-mode=full` on its start line). Tracing is
off by default, and there would be nothing to look at.

**What the person at the screen holds** is an admin session on the node's
chassis, with the node-local key's authority, behind this stack's one
password. The parent's stacks and secrets are not reachable from it. Two things
follow from how it is built:

- **The display has no password of its own** (`x11vnc -nopw`). Port 5900 is
  reachable only through the chassis's `connect`, which this stack's gate
  authorizes. The page names the service `browser`; it cannot name a port, so
  it cannot ask for the node's admin port directly.
- **Chrome's debugging port is open on the workspace's loopback.** Anything
  running on the workspace can drive the signed-in browser. A workspace is one
  trust zone already.
- **Chrome runs with its own sandbox.** The workspace allows unprivileged user
  namespaces, so `--no-sandbox` is not needed here, and Chrome's warning bar
  about it does not appear.

**This is the screen, not the byte path.** It needs no chassis change, and it
works for anything with a window. The better path for the admin UI itself is
for the parent to carry HTTP to the node and sign it, so the UI runs in your
own browser under your own identity on the parent. That is the request verb of
`todo-chassis-hierarchy.md` §8, and it is not built.

## A shell on the node: `/terminal`

Open `/terminal` and press **connect** (or open `/terminal#go`). This is
[`workspace-terminal`](../workspace-terminal) on the node's workspace: the page
opens `/term`, its first message runs `workspace://node/attach`, and from then
on the socket is a pseudo-terminal running `tmux new -A -s main`. Close the tab
and come back, and `tmux` resumes the same screen.

The shell is set up to talk to the node. The distribution is on the path and
the CLI signs as the node-local key, so these reach the node's own chassis:

```sh
txco auth whoami          # profile: node, chassis: http://127.0.0.1:8927, source: signed
txco trace last           # the node's most recent request
txco versions hello       # a stack the node was given
```

`/screen` and `/term` share the stack's `_websocket` rules. What a socket gets
is decided by its first message: `{"type":"connect"}` binds the display,
`{"type":"attach",…}` binds a terminal.

**What the person at the terminal holds** is a shell as the workspace's own
user, which can become root, on the machine the node runs on. It is the same
reach `/ui` and `/call` already give, in a more direct form, behind the same
one password. An attach refuses secrets by design, so nothing of the parent's
arrives with it.

## What this example is not

- **`/call` is a stand-in.** A rule here carries a request someone else signed.
  The built form is a request verb, where the parent chassis signs what it
  sends with a key of its own and no rule touches a signature.
- **The node-local key is within reach of every program on the workspace.**
  A command there runs as a user with passwordless `sudo`, so the key that
  signs `txco apply` on the node, and the node's whole data directory, can be
  read by anything running there. A workspace is one trust zone.
- **Nothing here goes up.** A node that needs something from its parent, such
  as a secret or a capability, asks over HTTPS with a run grant. That is
  designed (`todo-chassis-hierarchy.md` §3.5) and not built.

## Restricting access

**This stack fails CLOSED.** Every route runs a command on a computer, and
`/setup` enrolls a key that may administer the chassis inside it, so every
route answers 401 until `EXAMPLE_NODE_PW` is set (only `/healthz` stays open).
Once set, every route requires HTTP Basic auth: user `human`, password = that
secret. The gate (`node/090`, `node/095`) uses `txco://basic-auth-verify`,
which consumes the secret inside the op and compares it constant-time, and it
fails closed three ways: no secret set, a wrong password, or a chassis without
the op.

The one value a request may put into a command's environment is `parent_pub`,
a public key. The script refuses anything that is not base64.

## Verified on a real sprite (2026-09-30)

One workspace, driven from a chassis with the Sprites provider. Release
`v0.2.31`, Ubuntu 26.04.1, glibc 2.43, kernel 6.12.

| What | Result |
|---|---|
| Provision, from an empty workspace | Ready in 5 to 13 s. The install takes 1 to 7 s of that (a 47 MB download). `latest` resolved to `v0.2.31`. |
| Provision again, in place (`REQ` bumped) | About 3 s. The same key id, the same stacks. |
| `VERSION` pinned to a tag (`0.2.31`, then `v0.2.31`) | Installed that release each time |
| `VERSION` naming a tag that does not exist | The node kept running, the same process. `GET /setup`: `"ready":true` with `"setup_error":"there is no release v9.9.9-nope.1 for linux/amd64"`. Set back to `latest`: warm, and the error cleared. |
| The chassis, from start to `/healthz` | 0.24 to 0.47 s |
| Warm `/setup` | A no-op: 0.3 s round trip |
| The node at idle | 54 to 64 MB of memory. The binary is 96 MB on disk; an empty data directory is under 1 MB. |
| Enrollment after setup | Closed: the route answers 404 |
| `hello-world` from the registry, `--require-signature` | Installed. Signed, and verified against the built-in `txco` key. |
| `support-basic` from GitHub, which bundles a compute | Installed and applied on the stock image. `apply` fetched the javy toolchain itself (42 MB, once) and built the compute. The first `apply` of three stacks took 2 s. |
| A stack written on the node (`_inspect`) | Applied with the node-local key |
| The signed call down (`/call`) | 200 with the node's card, in 0.25 s. The `_inspect` stack ran a command on the node through the node's own `local` provider. |
| The same call unsigned, from the workspace | 401 |
| The same signed request, sent twice | 401 `nonce_replay` |
| The data plane on the node's loopback port | `hello` answers 200 |
| Idle for 100 s | Frozen for 94 of them. The next call took 0.4 s, and the chassis was the same process. |
| Idle for 11 minutes | Frozen throughout. The next call took 8.4 s, and the chassis was **still the same process**: the workspace came back with its memory. No cold start was needed. |
| The chassis stopped by hand (`/stop`), then `GET /setup` | Cold start: ready on the next poll, 0.26 s to `/healthz`. Keys and stacks were kept. |
| Destroy, then setup and the steps above again | The same results from nothing, twice |

**The node's admin UI, through `/ui`:**

| What | Result |
|---|---|
| `GET /ui/setup`, first time | Ready in 134 s (the package install). It reports ready only once Chrome's debugging port answers, so the sign-in that follows does not race it. |
| `GET /ui/setup` after its `REQ` is bumped | The browser stack is retired and relaunched in 10 s |
| A node moved from `--env=dev` to `--env=prod` in place | Empty stores, as expected. The parent's key enrolled under a new id; the old node-local key was refused, dropped and replaced; `/packages` applied again. `hello` then answered `{"say":"hello world"}` and nothing else. The signed call, the unsigned refusal and `/ui` all worked as before. |
| Chrome without `--no-sandbox` | Runs sandboxed. Under `dbus-run-session` its log went from hundreds of "Failed to connect to the bus" lines to five, all about the system bus the workspace does not run. |
| The page, in a real browser through the prod edge | The node's admin UI, usable: stacks, ops and traces |
| `POST /ui/open` | 3 s. A screenshot then shows the node's admin UI signed in, at the traces view, listing the node's own requests: `hello/0` from the data plane and `_inspect/0` from the signed call down. |
| `WS /screen` with the password | `101`, then `{"type":"attached"}`, then the display's `RFB 003.008` greeting |
| `WS /screen` with a wrong password | `401` |

**A shell on the node, through `/terminal`:**

| What | Result |
|---|---|
| `WS /term` with the password | `101`, then `{"type":"attached"}`, then the terminal's bytes |
| `WS /term` with a wrong password | `401` |
| Commands typed into it | Run as `sprite` in `/home/sprite`; `txco` resolves to `/opt/txco/bin/txco` |
| `txco auth whoami`, `txco trace last`, `txco versions hello` | Signed as the node-local key, answered by the node's chassis |
| The real page in a browser, at three window sizes, no manual resize | The PTY on the workspace had the page's size each time: 188×46, 137×32, 226×54. The page fits again shortly after load and a second or so later, and tells the PTY its size the moment the attachment is live. A resize sent while the attach is still starting is lost, which left the first version of the page at 80×24. |
| Idle for 75 s with the socket open | Still attached; the next command ran at once. An open terminal keeps the workspace awake, so it needs no keep-alive. |

**What the workspace allows** (`/facts`):

| Question | Answer |
|---|---|
| Can a command become root? | Yes. The default user (uid 1001) has passwordless `sudo`. |
| A command as another user | Yes, with `sudo -u`. The provider's `exec` has no user parameter. |
| A control group for one run | It can be created, can hold a process, and can be killed as a group. It gets **no controllers**, and the root refuses to hand any down, so it cannot limit memory or CPU. |
| UDP out, by name | A DNS query to a public resolver was answered |
| HTTPS up to the fleet's web head | 200. 0.19 to 2.4 s on a first connection, 0.07 s after. |
| HTTPS to a raw address | Refused |

**Learned on the way:**

1. **`apply` returns before the new version is served.** A call made in the
   first second after `txco apply` finds the old stacks: the data plane
   answered 404 and the inspect inlet `no_inspector`. `hello` answered 1.1 s
   after `apply` returned. `packages.sh` asks until it answers. The parent
   behaves the same way: a new rule is live a second or three after `apply`.
2. **A package that names external operations blocks the whole `apply`**
   until `txco.yaml` resolves them (`unresolved op://AUDIT`). `packages.sh`
   writes placeholders.
3. **A missing value compares equal to 0.** `WHEN ._setup.exit == 0` is true
   on a route that ran no setup. The response rules test `!= null` first.
4. **Ports 8926 and 8927 were free** on the stock image.
5. **A body written before the last scope is sent at once.** The chassis
   streams it and clears it from the envelope, so a later rule cannot ask
   whether a body was written. A fallback rule that did ran after every answer
   and appended itself to the page. `txco dev` does not stream
   (`TXCO_DEBUG_BREAKPOINTS`), so it only showed on a hosted chassis. Answer
   and `@halt` together.
