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
| `txco dev` | The dev loop: boots your `txco.yaml` apps + an ephemeral chassis, watches `OPS/*.txcl` and compute `.js/.ts` files, re-applies on save. Registers a keyless [`dev` profile](#local-dev-no-key-required) for the chassis. `--ui` adds the admin-UI Vite server; `--tcp` / `--dns` add those heads |
| `txco demo` | Ephemeral chassis + browser playground with a guided curriculum (build/web/mail/async/mcp tracks) |
| `txco init <stack>` | Scaffold `OPS/<stack>/…`; `--from github:…\|oci:…\|dir:…` scaffolds from a template |
| `txco doctor` | Diagnose local setup: home dir, profile, keys, chassis reachability, version sync (`--offline` skips remote checks) |

## Deploy & versions

Stacks change through versioned drafts ([admin-api](./admin-api.md)).
The CLI verbs map onto that flow:

| Command | What it does |
|---|---|
| `txco apply [dir]` | Deploy the whole `OPS/` tree: draft + activate per changed stack; resolves `op://` refs; uploads computes |
| `txco push <stack>` | Like `apply`, one stack |
| `txco pull <stack>` | Materialize a stack's active version (or `--version N`) into local `OPS/<stack>/` — the inverse of `push` |
| `txco draft <stack>` | Upload a draft *without* activating (stage for review); `--activate` flips it too |
| `txco activate <stack>` | Flip the active-version pointer (defaults to newest draft). Activating an older version = rollback |
| `txco versions <stack>` | List a stack's versions, active one marked |
| `txco diff [dir]` | Compare local `OPS/` against the running chassis |
| `txco status [dir]` | Per-stack drift summary; exit 1 on divergence (CI-friendly) |
| `txco edit <stack> <path>` | `$EDITOR` one file of a draft, PATCH it back |

## Nano-ops

| Command | What it does |
|---|---|
| `txco op init <path>` | Scaffold a `.js`/`.ts` compute next to its rule |
| `txco op build <path>` | Bundle + compile to wasm (auto-fetches the pinned `javy`) |
| `txco op run <path> --input <json\|@file>` | Execute locally on the same engine production uses |
| `txco op test <path>` | Run against the scope's `mock-request.json`, diff vs `mock-response.json` |

## Packages

`txco install <ref> --as <stack>`, `txco package
{init,validate,inspect,pull,publish,key,list,upgrade,remove}` — see
[packages](./txco-oci-packages.md).

## Identity & access

| Command | What it does |
|---|---|
| `txco auth bootstrap-local` | First-run: generate a key + enroll it ([admin-api](./admin-api.md)) |
| `txco auth init` / `enroll` / `rotate-key` / `revoke-key` / `revoke-actor` | Key lifecycle |
| `txco auth whoami` (alias `txco whoami`) | What the chassis thinks you are |
| `txco auth invite` / `invitations` / `revoke-invitation` / `accept --token …` | Teammate onboarding |
| `txco auth profiles` / `profile {use,show,remove}` | Named identities (AWS-style); also aliased under `txco config` |
| `txco auth tenants` / `tenant {create,members,grant,revoke}` | Tenant management |
| `txco auth tenant hostnames {add,attach,verify,challenge,list,remove}` | Hostname bindings ([ingress](../routing.md)) |
| `txco auth tenant secrets {set,generate,list,show,describe,rotate,revoke}` | ([Secret store](./runbook-secret-store.md)) |
| `txco auth login` (alias `txco ui`) | Mint a signed browser session, open the admin UI |
| `txco auth sessions {list,revoke}` / `logout` | Browser sessions / stop signing |
| `txco login` / `logout` / `cloud {…}` | **Cloud** account OAuth — distinct from `auth login`, which targets your own chassis |

## Diagnose

| Command | What it does |
|---|---|
| `txco trace [rid\|last]` | Step-by-step trace explorer ([trace](./trace.md)); bare `txco trace` is interactive |
| `txco mcp doctor <url>` | Probe an MCP server: handshake + tool list ([mcp](./protocols/mcp.md)) |

## Operator & misc

| Command | What it does |
|---|---|
| `txco admin resync --tenant <slug>` | Re-emit control-plane state to the fleet (super-admin) |
| `txco admin tenant {suspend,resume}` | Tenant kill switch (super-admin) |
| `txco snapshot {export,restore,publish}` | Runtime-DB snapshots for fleet bootstrap |
| `txco dns {zone,record,config,render}` | Delegated DNS zones (requires the `dns` personality) |
| `txco cron config {show,set}` | The tenant's cron timezone — `set timezone <IANA zone>` localizes `@cron.*` (default UTC) |
| `txco version` / `update check` / `upgrade` | Version info / check / self-update |
| `txco completion <shell>` | bash/zsh/fish completion script |
| `txco plugin` | List external `txco-<name>` plugins (kubectl convention) |
