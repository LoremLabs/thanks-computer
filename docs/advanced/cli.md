# CLI — The `txco` command

_The complete command surface, grouped by what you're doing. Every
command supports `--help` for full flags; common deploy flags
(`--target`, `--tenant`, `--json`) repeat across the family._

<img width="625" height="630" alt="image" src="https://github.com/user-attachments/assets/a1355625-98a4-461d-a446-d43688f14b2f" />

## Run & develop

| Command | What it does |
|---|---|
| `txco serve` | Boot the chassis ([runtime reference](./serve.md)) |
| `txco dev` | The dev loop: boots your `txco.yaml` apps + a throwaway chassis, watches `OPS/*.txcl` and compute `.js/.ts` files, re-applies on save. `--ui` adds the admin-UI Vite server; `--tcp` / `--dns` add those heads |
| `txco demo` | Throwaway chassis + browser playground with a guided curriculum (build/web/mail/async/mcp tracks) |
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
