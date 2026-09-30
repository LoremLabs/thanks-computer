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
GET  /logs      → the node chassis's own log
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
| `node/200/*.txcl` | the answers: setup's JSON line, a command's output as text, a 503 on a transport failure |

## Provision, cold start, warm

A workspace is frozen within seconds of its last `exec` and resumes on the next
one. It may also come back cold: its processes gone and its disk kept. So a
node is kept up by **one idempotent exec with three cases**, the same shape as
[`workspace-browser`](../workspace-browser):

- **Provision** — the marker `/var/lib/txco/version` differs from the `REQ`
  and `VERSION` a `POST /setup` carries:
  1. Make `/var/lib/txco`, mode 0700.
  2. Run the project's installer, `https://get.thanks.computer/install.sh`,
     as the workspace's own user, into a staging directory. It picks the
     architecture, resolves `VERSION` to a release and checks the tarball
     against that release's `checksums.txt`.
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
  binary and restarts the chassis, in about three seconds.
- **A provision that fails leaves the node as it was.** A tag that does not
  exist stops at the installer; the chassis is never stopped, and `GET /setup`
  answers `"ready":true` with the release it still runs and a `setup_error`
  that stays until a provision succeeds.
- **`latest` trusts the release as published.** The installer's checksum comes
  from the same release as the tarball, so it catches a damaged download, not
  a replaced one. A parent that must know exactly what its nodes run pins a
  release. Once a node and its parent speak a protocol to each other, their
  versions should match.
- **Every provision depends on `get.thanks.computer`** answering, as well as
  GitHub.

**The chassis is started like this,** from `/var/lib/txco`:

```sh
setsid /opt/txco/bin/txco serve \
  --admin-addr 127.0.0.1:8927 --web-addr 127.0.0.1:8926 \
  --auth-mode=signed --personalities=web,admin,grant \
  --structured-host-suffix=.localhost \
  --workspace-provider=local --workspace-allow-local \
  </dev/null >>node.log 2>&1 &
```

- **The working directory is the data directory.** The chassis has no single
  data flag; its paths default to `./chassis/data/*`.
- **`--auth-mode=signed`** closes open-dev, which `--env=dev` (the default)
  would otherwise leave on.
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
| Provision, from an empty workspace | Ready in 5 to 10 s. The installer takes 2 to 5 s of that (a 47 MB download). `latest` resolved to `v0.2.31`. |
| Provision again, in place (`REQ` bumped) | About 3 s. The same key id, the same stacks. |
| `VERSION` pinned to a tag (`0.2.31`, then `v0.2.31`) | Installed that release each time |
| `VERSION` naming a tag that does not exist | The node kept running, the same process. `GET /setup`: `"ready":true` with `"setup_error":"the installer could not install VERSION=v9.9.9-nope.1"`. Set back to `latest`: warm, and the error cleared. |
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
