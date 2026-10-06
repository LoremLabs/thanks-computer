# CLI — The `txco` command

_The complete command surface, grouped by what you're doing. Every
command supports `--help` for full flags. The chassis selector `--target`
(see [Selecting a chassis](#selecting-a-chassis---target)) plus `--tenant` /
`--json` repeat across the family._

<img width="625" height="630" alt="image" src="https://github.com/user-attachments/assets/a1355625-98a4-461d-a446-d43688f14b2f" />

## Selecting a chassis (`--target`)

Every command that talks to a chassis takes **`--target`** — the single flag for
*which chassis*. It accepts, highest precedence first:

1. a **workspace target** from `txco.yaml` (`targets:` — also carries that env's ops / mock policy)
2. a **profile** name — a "named chassis" carrying its own `chassis_url` **and** signing key
3. a **raw admin URL** (`--target https://host:8081`)

```sh
txco apply cloud                          # a profile (or txco.yaml target) named "cloud"
txco status --target staging
txco auth tenant secrets set OPENAI_KEY --target dev
txco apply --target https://chassis:8081  # a raw URL works too
```

On the deploy verbs (`apply`, `push`, `status`, `diff`) the target may also be a
**bare positional** — `txco apply staging`. A path-like arg (`.`, `./x`, `/x`, or
anything containing `/`) or an existing directory is taken as the workspace dir
instead, so `txco apply ./sub staging` sets both. (`txco push` takes the stack
first: `txco push api staging`.)

The mutating `auth` / `tenant` commands accept it as a trailing positional too —
`txco auth tenant secrets set OPENAI_KEY staging`, `txco auth tenant grant ACTOR staging`.
These are stdlib-flag-parsed, so any flags must come *before* the trailing target
(`secrets set NAME --tenant t staging`, not `… staging --tenant t`).

`--url` / `--addr` (raw URL) and `--profile` (signing identity) still work as
lower-level overrides — `--target` is just the one spelling unified across the
deploy and `auth` / `tenant` families. With `--target` omitted, the active
profile (after `txco login`) supplies the default; otherwise it's
`http://localhost:8081`.

### Write-guard

A command that **mutates** a **non-local** chassis first prints the resolved
target and asks to confirm — `--yes` skips it, and a non-interactive shell
without `--yes` fails closed. So a stray `secrets set` / `hostnames add` can't
silently land on prod. Local chassis (localhost / loopback / `*.localhost`)
never prompt.

### Local dev: no key required

Against a **local** chassis (`localhost` / loopback / `*.localhost` — e.g. the
`txco dev` chassis), `auth` / `tenant` commands don't need an enrolled signing key:
they send unsigned and the open dev chassis accepts. So
`txco auth tenant secrets set OPENAI_KEY` just works locally with no
`bootstrap-local`. A **remote** chassis still requires enrollment; a local chassis
running in signed mode returns a clear 401.

To save the repetition, **`txco dev` auto-registers a `dev` profile** pointing at
the chassis it just started (with `default_tenant: default`), so you can use the
named selector everywhere instead of spelling out the URL + tenant:

```sh
txco apply dev                              # deploy to the dev chassis
txco auth tenant secrets set SHH_KEY dev # set a secret on it (tenant: default)
txco ui dev                                 # open the dev admin UI
```

## Run & develop

| Command | What it does |
|---|---|
| `txco serve` | Boot the chassis ([runtime reference](./serve.md)) |
| `txco dev` | The dev loop: boots your `txco.yaml` apps + an ephemeral chassis, watches `OPS/*.txcl`, the files they [`&include`](./txcl/txcl.md#including-files--include) and compute `.js/.ts` files, re-applies on save. Registers a keyless [`dev` profile](#local-dev-no-key-required) for the chassis. Flags below |
| `txco demo` | Ephemeral chassis + browser playground with a guided curriculum (build/web/mail/async/mcp tracks) |
| `txco init <stack>` | Scaffold `OPS/<stack>/…`; `--from github:…\|oci:…\|dir:…` scaffolds from a template |
| `txco doctor` | Diagnose local setup: home dir, profile, keys, chassis reachability, version sync (`--offline` skips remote checks) |

`txco dev` starts the web, admin and cron heads, and WebSockets on the web head. The rest are opt-in, each with dev defaults:

| Flag | Adds |
|---|---|
| `--ui` | The admin-UI Vite dev server |
| `--tcp`, `--dns`, `--lmtp`, `--imap` | Those heads on their dev ports ([protocols](./protocols/README.md)) |
| `--calendar`, `--contacts`, `--webdav`, `--ipp` | CalDAV, CardDAV, the drive as a folder, a stack as a printer — on the web head |
| `--state`, `--scheduled`, `--source` | The state dispatcher, the durable-timer poller, the remote-mailbox poller |
| `--grant` | The socket `txco sandbox` reaches the chassis on ([grants](./grants.md)) |
| `--allow-local-workspace` | `workspace://` on the local provider: commands run as **your** user, unsandboxed ([workspaces](../workspaces.md)) |
| `--workspace-local-exec "<prefix>"` | With `--allow-local-workspace`: hand every workspace command to this program instead of running it here, so dev drives another machine — `"sprite exec -s dev-{name} --"`, `"docker exec -i pony-{name}"`. The machine is yours to make and remove ([workspaces](../workspaces.md#providers)) |

And to shape the loop itself: `--chassis-addr` / `--web-addr` move the chassis off `:8081` / `:8080` (to run a second
one beside yours), `--no-chassis` uses one already running, `--watch=false` and `--apply=false` turn off the reload and
the startup apply, `--watch-ignore <glob>` prunes the watcher, `--force-opstacks` rewrites `opstacks/` from the
embedded template, and `--verbose` turns the chassis log up to debug.

## Deploy & versions

Stacks change through versioned drafts ([admin-api](./admin-api.md)).
The CLI verbs map onto that flow:

| Command | What it does |
|---|---|
| `txco apply [dir]` | Deploy the whole `OPS/` tree: draft + activate per changed stack; expands [`&include`](./txcl/txcl.md#including-files--include) files, resolves `op://` refs; uploads computes. Refuses a stack the chassis moved since this workspace last synced (see below); `--force` overwrites |
| `txco push <stack>` | Like `apply`, one stack |
| `stacks:` in `txco.yaml` | Binds a stack to a [Web ABI](./web-abi.md) build: `apply`, `push`, `dev`, `status` and `lint` lay the build over the stack's tree. `--static-only` deploys a build's static half when it has a server entry |
| `txco pull <stack>` | Materialize a stack's active version (or `--version N`) into local `OPS/<stack>/` — the inverse of `push`, as deployed: `op://` refs come back resolved and `&include`s come back as the included text |
| `txco draft <stack>` | Upload a draft *without* activating (stage for review); `--activate` flips it too |
| `txco activate <stack>` | Flip the active-version pointer (defaults to newest draft). Activating an older version = rollback |
| `txco deactivate <stack>` | Retire a stack: activates an empty version, so it stops serving (HTTP 404, mail 550) and keeps its history. Use it for a stack you removed from `OPS/` — `apply` leaves a stack it no longer finds serving its last version |
| `txco stack set web=true\|false <stack>` | Turn a stack's own public web URL on or off (`--match <substr>` selects several) |
| `txco versions <stack>` | List a stack's versions, active one marked |
| `txco caps list [--json]` | The tenant's capability catalogue: what its active stacks declare under `CAPS/`, and where a call enters ([capabilities](./capabilities.md#the-catalogue)) |
| `txco runs [--stack S] [--json]` | The tenant's runs in flight on the chassis: rid, inlet, the stack it entered, the scope it is in now, its age ([aborting a run](../abort.md)) |
| `txco abort <rid>` · `txco abort --stack <name>` | End a run in flight — or every run of a stack — without ending the chassis: ops in flight are cancelled, nothing later runs, the trace says `aborted` and by whom. `--reason` for the trace; confirmation on a non-local chassis ([aborting a run](../abort.md)) |
| `txco diff [dir]` | Compare local `OPS/` against the running chassis |
| `txco lint [dir]` | Validate the `OPS/` tree **offline** (no chassis): name collisions, mis-placed files, txcl parse, unconditional-loop warnings, and each [Web ABI](./web-abi.md) build bound in `txco.yaml`; `--list` prints the op graph; exit 1 on errors (CI-friendly) |
| `txco web check <abi-dir \| stack>` | Install a [Web ABI](./web-abi.md) build on a throwaway chassis and probe it: files, a `_` file, the immutable cache, an unknown page, an unknown asset, a POST, HEAD, a conditional GET. Exit 1 on failure; `--json`, `--strict`, `--keep` |
| `txco status [dir]` | Per-stack drift summary; exit 1 on divergence (CI-friendly) |
| `txco edit <stack> <path>` | `$EDITOR` one file of a draft, PATCH it back |
| `txco data apply [dir]` | Deploy the `VECTORS/`, `KV/`, `BLOBS/`, `CALENDARS/`, `CONTACTS/` packs (code carried forward); same fast-forward rule, plus a refusal over runtime-edited seeded blobs — see [blobs](./blobs.md) |
| `txco data pull [dir]` | Bring each stack's live seeded blobs into `BLOBS/` |
| `txco data {ls,show,diff,rm}` | The tenant's vector collections: list them, show one's pin and item IDs, compare a local `VECTORS/*.jsonl` pack to the live store, drop a whole collection (`apply` never drops one) |

### Fast-forward rule

A workspace records the version it last synced (`.txco/<stack>.state.json`,
written by `pull`, `apply`/`push`, `dev`, and kept current by `data apply`
and `activate`). If the chassis's active version is no longer that one —
a teammate deployed, someone edited in the admin UI, or the stack was
rolled back — `apply`, `push` and `data apply` **refuse** rather than
silently supersede their change (git's `! [rejected] non-fast-forward`):

```
apply: web: refused — chassis active is v7 but this workspace last synced v5 — someone deployed (or edited in the admin UI) since.
  `txco diff web` shows what changed; `txco pull web --force` takes the chassis version (discards local edits); `apply --force` overwrites the chassis.
```

The chassis enforces the same rule under its lock (the activate carries the
version the client expects; a mismatch is `409 stack_moved`), so two applies
racing can't both win. A workspace with no baseline — a fresh checkout, CI —
cannot detect a move and applies unconditionally; `txco pull` first to
establish one.

## Nano-ops

| Command | What it does |
|---|---|
| `txco op init <path>` | Scaffold a `.js`/`.ts` compute next to its rule |
| `txco op build <path>` | Bundle + compile to wasm (auto-fetches the pinned `javy`) |
| `txco op run <path> --input <json\|@file>` | Execute locally on the same engine production uses |
| `txco op test <path>` | Run against the scope's `mock-request.json`, diff vs `mock-response.json` |

## Packages

`txco install <ref> --as <stack>`, `txco package
{init,validate,inspect,pull,publish,key,list,upgrade,remove}`, and `txco packages`
(short for `package list`) — see [packages](./txco-oci-packages.md). A ref names its
scheme (`oci://host/name:tag`, `github:owner/repo@ref/dir`, `dir:./path`); a bare name
is a package on the default registry ([sharing stacks](../packages.md)).

## Identity & access

| Command | What it does |
|---|---|
| `txco auth bootstrap-local` | First-run: generate a key + enroll it ([admin-api](./admin-api.md)) |
| `txco auth init` / `enroll` / `rotate-key` / `revoke-key` / `revoke-actor` | Key lifecycle |
| `txco auth whoami` (alias `txco whoami`) | What the chassis thinks you are |
| `txco auth invite` / `invitations` / `revoke-invitation` / `accept --token …` | Teammate onboarding |
| `txco auth profiles` / `profile {use,show,remove}` (alias `txco use <profile>`) | Named identities; also aliased under `txco config` |
| `txco auth tenants` / `tenant {create,members,grant,revoke}` | Tenant management |
| `txco auth tenant hostnames {add,attach,verify,challenge,list,remove}` | Hostname bindings ([ingress](../routing.md)) |
| `txco auth tenant secrets {set,generate,list,show,describe,policy,rotate,revoke}` | ([Secret store](./runbook-secret-store.md)); `policy NAME --pull none\|reviewed\|any` says whether dispatched work may be handed the secret ([grants](./grants.md#the-pull-policy)) |
| `txco sandbox NAME [NAME…] -- PROGRAM` | Inside a workspace: start a program inside named sandboxes, opened with the command's run grant ([grants](./grants.md#txco-sandbox)) |
| `txco auth login` (alias `txco ui`) | Mint a signed browser session, open the admin UI |
| `txco auth sessions {list,revoke}` / `logout` | Browser sessions / stop signing |
| `txco login` / `logout` / `cloud {…}` | **Cloud** account OAuth — distinct from `auth login`, which targets your own chassis |

## Diagnose

| Command | What it does |
|---|---|
| `txco trace [rid\|last]` | Step-by-step trace explorer ([trace](./trace.md)); bare `txco trace` is interactive |
| `txco inspect <stack> [noun] [id]` | Ask a stack what its current state is: the request becomes an `@src == "inspect"` event routed to the tenant's `_inspect` stack, and the matching inspector op answers with a card this renders. Where `trace` says what just happened, `inspect` says what is true now ([inspect](./inspect.md)) |
| `txco cat <stack> <path>` | Print a deployed stack's `FILES/` asset (active version), resolved the way `txco://read-file` resolves it, and say where resolution failed if it did |
| `txco source status` | Each declared remote source (`SOURCES/`) and its poll state; read-only |
| `txco mcp doctor <url>` | Probe an MCP server: handshake + tool list ([mcp](./protocols/mcp.md)) |
| `txco kv list <namespace>` | List the keys an op accumulated in the KV store ([kv](./kv.md)) |
| `txco notebook {list,read,tail,export}` | Read the append-only notebooks a stack writes, oldest first ([notebooks](./notebooks.md)) |

## Operator & misc

| Command | What it does |
|---|---|
| `txco admin resync --tenant <slug>` | Re-emit control-plane state to the fleet (super-admin) ([fleet](./fleet.md)) |
| `txco admin tenant {show,suspend,resume,limits}` | A tenant's state, its kill switch (`suspend --status 402 --reason …`), and its admission limits (super-admin) |
| `txco snapshot {export,import,publish}` | Runtime-DB snapshots for fleet bootstrap: export one, safely restore one from a file (`import`), publish one ([fleet](./fleet.md)) |
| `txco dns {zone,record,config,render}` | Delegated DNS zones (requires the `dns` personality) |
| `txco cron config {show,set}` | The tenant's cron timezone — `set timezone <IANA zone>` localizes `@cron.*` (default UTC) |
| `txco version` / `update check` / `upgrade` | Version info / check / self-update |
| `txco completion <shell>` | bash/zsh/fish completion script |
| `txco plugin` | List external `txco-<name>` plugins (kubectl convention) |
| `txco room [--room <name>] <message>` (also `thanks …`) | Send a message to a room: it becomes an `@src == "room"` event that runs through the tenant's `_room` stack like any other (default room: `general`); with no message, the live feed ([rooms](./protocols/room.md)) |

## Environment

| Variable | Effect |
|---|---|
| `TXCO_HOME` | Where the CLI keeps profiles and keys. Default `$XDG_CONFIG_HOME/txco`, else `~/.config/txco` |
| `TXCO_PROFILE` | The profile to use when `--profile` is not given; beats the workspace's and the active one |
| `TXCO_TENANT` | The tenant when `--tenant` is not given; beats the profile's default tenant |
| `TXCO_PRIVATE_KEY_PATH` | Sign with this key file instead of the profile's (its `<path>.meta.json` must sit beside it) |
| `TXCO_NO_BROWSER` | Any value: a command the chassis provides (forwarded to it) prints a page's URL instead of opening it. `txco ui` has `--no-open` for the same |

The chassis reads its own settings from `TXCO_*` variables too: each `txco serve` flag has one
(`--trace-mode` is `TXCO_TRACE_MODE`); see [the runtime reference](./serve.md) and
[every flag](./flags.md).
