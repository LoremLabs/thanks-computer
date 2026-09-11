# Workspaces

A **workspace** is an owned, stateful execution environment your stack
addresses by name. Where an [`op://` nano-op](./authoring/nano-ops.md) is a
pure JSON→JSON transform in a sandbox with no filesystem and no network, a
workspace keeps files between calls, has a full runtime, and (in the fleet)
reaches the public internet. Think "a machine that belongs to this stack",
without you operating a machine.

```txcl
WHEN @web.req.url.path == "/build"
  EXEC "workspace://builder/exec"
    WITH command = "git pull && make",
         into = "_build"
```

## The mental model: lambda-like, but it has a name

- **Owned.** A workspace belongs to one `(tenant, stack, name)`. `tools` in
  stack `agents` of tenant `acme` is always the same environment.
- **Stateful.** Files written by one exec are there for the next — across
  requests, across chassis restarts.
- **Lambda-shaped in cost.** It wakes on exec, sleeps when idle, and is
  destroyed after long disuse (the data is gone; the next exec starts
  fresh). You pay for runs, not residency.
- **Exit codes are data.** A command that exits 3 is a *successful*
  dispatch whose result says `exit: 3`. A transport failure — the
  workspace would not wake, the command timed out — merges as
  `workspace.error` instead. Either way the op is not dropped.

Vocabulary: a *computer* executes work; a *workspace* is the owned,
stateful environment that provides one; a *run* is one wake→sleep; a
*task* spans runs.

## The ref

```
workspace://<name>/<verb>
```

The verb is the segment after the **last** `/`, so a name may nest:
`workspace://pony/paris/exec` is workspace `pony/paris`, verb `exec`.
Names are 1–4 `/`-separated segments, each a DNS-label-shaped token
(`^[a-z0-9][a-z0-9-]{0,62}$`, no `--`), and no segment may equal a verb.

`WITH workspace = <expr>` replaces the ref's name entirely — the ref's
name is a static default, the WITH value (a literal or an envelope path)
wins:

```txcl
EXEC "workspace://pony/exec"
  WITH workspace = ._in.slug,      # e.g. "pony/paris"
       command = "npm test"
```

| Verb | What it does | Result under `into` |
|---|---|---|
| `exec` | Run a command (creating and waking the workspace as needed) | `{exit, stdout, stderr, …}` |
| `create` | Make sure the workspace exists, without waking it | `{state: "created"}` |
| `wake` | Warm it ahead of a task's first exec | `{state: "running"}` + `_txc.workspace.run` |
| `checkpoint` | Snapshot it (`WITH comment = "…"`); providers with the capability only | `{state: "checkpointed", checkpoint_ref}` |
| `sleep` | Park it (a no-op where the provider sleeps implicitly) | `{state: "sleeping"}` |
| `destroy` | Remove it — files are gone; the next exec starts fresh | `{state: "destroyed"}` |

`exec` also takes `WITH checkpoint = true` (and an optional `comment`): after
an exec that exits 0 the workspace is snapshotted and `checkpoint_ref` is
added to the result — "after cloning", "after install" are yours to name.
A checkpoint that fails does not fail the exec; `checkpoint_error {code,
message}` says why. A provider without the capability (the local one)
reports `code = "unsupported"`.

## `exec` — the request

| `WITH` key | Meaning |
|---|---|
| `command` | A shell line, run by the workspace's `/bin/sh -c` |
| `args` | An argv (array of strings), no shell — use for untrusted arguments. `command` and `args` are mutually exclusive |
| `stdin` | Bytes fed to the process (a string; a JSON value is fed as its text) |
| `cwd` | Working directory, relative to the workspace and inside it |
| `env` | An object of extra environment variables |
| `into` | Where the result lands (default `_workspace`) |
| `timeout` | Wall clock for the whole exec — create/wake, the command, output capture; the command is killed when it expires. Default `--workspace-default-timeout` (5m), capped by `--op-timeout-max` (10m) |
| `secrets.env.<NAME>.secret` / `.format` / `.optional` | A stored secret, materialized into the environment as `NAME` (`format = "Bearer {}"` templates it). The only place a workspace op takes a secret — `secrets.headers.*` / `.body.*` are refused |
| `checkpoint = true`, `comment` | Snapshot after a successful exec (see the verbs) |

The command sees a scrubbed environment — `PATH`, `HOME` (= the
workspace), `TMPDIR` (inside it), plus your `env` and any `secrets.env.*`
— never the chassis's.

**Secrets never come back out.** Every materialized secret's value is
replaced with `[REDACTED]` in stdout, stderr and error messages before
the result is built, so neither the envelope nor the trace step carries
it — even if the command prints its environment. Values shorter than 8
bytes are not scrubbed (scrubbing "1234" would shred unrelated output);
real tokens are far longer. A `format`-templated value still contains the
raw secret, so scrubbing the raw value covers it.

## `exec` — the result

```json
{
  "_workspace": {
    "exit": 0,
    "stdout": "hello\n",
    "stderr": "",
    "stdout_truncated": false,
    "stderr_truncated": false
  },
  "_txc": {
    "workspace": {
      "provider": "local",
      "computer": "<the provider's id for it>",
      "run": "<run id>",
      "exit": 0,
      "duration_ms": 4
    }
  }
}
```

stdout and stderr are each capped at `--workspace-max-output-bytes`
(1 MiB); the excess is dropped and flagged `*_truncated`. On a fleet
provider, output a command produces in roughly its first 20 ms may come
back with stderr merged into `stdout` and `stderr` empty (the provider
replays the session's history, which has no stream separation); `exit` is
exact regardless. Gate on `exit`, not on `stderr` being non-empty.

`_txc.workspace.*` is the chassis's provenance stamp, written **after**
the command returns. The command's output is a string under `into`; it
never produces envelope JSON, so it can never forge the stamp (the
output sanitizer lets `_txc.workspace.*` through for the workspace
transport only, and strips every other reserved `_txc.*` path as it does
for any untrusted producer).

On a transport or provider failure:

```json
{ "_workspace": {}, "workspace": { "error": { "code": "timeout", "message": "…" } } }
```

Codes: `timeout`, `provider`, `capacity`, `bad_request`, `not_allowed`,
`unsupported_verb`. Top-level (not under `_txc`) so a rule can gate on it:
`WHEN .workspace.error.code == "timeout"`.

The op is dropped — logged at ERROR, nothing merged — only on authoring
errors: a malformed ref, a bad name, no provider configured, an
untenanted request, or a malformed `secrets` block.

## Loops, fuel, usage, trace

`workspace://` may [LOOP](./advanced/txcl/txcl.md#loop--repeat-an-op):
its failures are in-band data and its wall-clock is fuel-metered, which
is what a loop needs. Each exec pays the flat EXEC dispatch plus 1 fuel
per started 30 seconds of wall clock (2 per minute), minimum 1 — a
5 minute exec is 10 fuel. (Not the nano-op rate of 10 per millisecond:
while a command runs, the provider's machine does the work and the
chassis only waits.) The machine time itself is reported on the usage
event with `src=workspace` (duration, bytes in/out, status), and every
dispatch writes a trace step with `transport=workspace`.

## Providers

The chassis runs workspaces through a **provider** chosen by
`--workspace-provider`. Unset, `workspace://` is off and an op that fires
fails loudly.

**`local`** (bundled) — a directory per workspace under
`--workspace-local-root` (`<root>/<tenant>/<stack>/<name>`); commands run
as the chassis's own uid. **There is no isolation**: a rule author on
that chassis can run anything the chassis process can. It is therefore
refused unless `--workspace-allow-local` is also set — never implied by
`--env` — and the chassis logs a WARN pair at boot when it is on. For dev
and single-operator self-hosting only.

```
txco dev --allow-local-workspace     # sets the three flags for this run;
                                     # workspaces land in .txco/dev/workspaces/
```

Fleet providers (a machine per workspace, with network policy and
checkpoints) register the same way from the overlay and are selected by
name; the stack's txcl does not change.

| Flag | Default | Meaning |
|---|---|---|
| `--workspace-provider` | _(unset)_ | `local`, or an overlay provider |
| `--workspace-allow-local` | `false` | Required for `local`; see above |
| `--workspace-local-root` | `./chassis/data/workspaces` | Root for `local` |
| `--workspace-default-timeout` | `5m` | Per-exec default when `WITH timeout` is absent (sized for builds, tests, tool runs) |
| `--workspace-max-output-bytes` | `1048576` | stdout/stderr capture cap, each |
| `--workspace-reap` | `720h` | Idle window before the reaper destroys a workspace (fleet background service) |

## Identity, runs, and the reaper

Each `(tenant, stack, name)` has a row in the `workspaces` table of the
runtime DB: the provider's reference for it, when it was last used, the
current run id, the latest checkpoint. The row makes the identity durable
across chassis restarts and shared across fleet nodes; it records, it never
locks — concurrent execs on one workspace are allowed, and the author owns
file-level conflicts.

A **run** is one stretch of execution between wake and sleep. On the local
provider every exec is its own run. On a fleet provider that idles for a
while before it sleeps, execs closer together than that window share a run
id; `_txc.workspace.run` is how a trace tells them apart, and a task that
spans a sleep simply sees a new run id on its next exec.

**The reaper destroys.** Where the `workspace-reaper` background service
runs, a workspace idle longer than `--workspace-reap` (30 days by default)
is deleted at the provider and its row marked destroyed. Its files are
gone — no snapshot is kept — and the next exec starts fresh. Operators can
do the same by hand (`txco workspace ls --idle-days=30`,
`txco workspace rm <tenant> <stack> <name>`) or take a `checkpoint` first.

## Example

[`examples/workspace-hello`](../examples/workspace-hello) — a command,
a counter file that persists across requests, a three-pass loop, and a
non-zero exit merging as data.
