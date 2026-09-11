# workspace-hello — run commands in a persistent workspace

A **workspace** is an owned, stateful execution environment your stack
addresses by name: `EXEC "workspace://tools/exec"` runs a command in the
workspace called `tools`, created on first use. Files persist between
calls; the command has a real runtime and (in the fleet) public
egress. Compare `op://` nano-ops, which are pure JSON→JSON transforms in
a sandbox with no filesystem and no network.

```
OPS/ws-hello/
  100/hello.txcl        echo hello && pwd
  100/count.txcl        increment a counter file          (state persists)
  200/count-read.txcl   cat the file back in a second exec
  100/loop-reset.txcl   rm -f loop
  200/loop.txcl         increment a file … LOOP UNTIL ._ws.stdout == "3" MAX 5
  100/exit.txcl         exit 3 — a non-zero exit is data
  100/stream.txcl       WITH stream = true — stdout reaches the client live
  300/respond.txcl      copies _ws.* and @workspace onto the body
```

Run it:

```
txco dev --allow-local-workspace     # from this directory
curl http://localhost:8080/hello
```

```json
{"exit":0,"stdout":"hello\n/…/.txco/dev/workspaces/default/ws-hello/tools\n","stderr":"",
 "workspace":{"provider":"local","computer":"/…/tools","run":"…","exit":0,"duration_ms":4}}
```

Without the flag the chassis logs `workspace:// disabled: local provider
requires --workspace-allow-local` and the op is dropped (the request
still completes — a rule with no `_ws.exit` just never fires the
responder). **The local provider runs commands as your own uid with no
isolation**; that is why it is opt-in per run and never implied by the
dev posture. The fleet uses a machine-per-workspace provider instead.

Then:

```
curl http://localhost:8080/count     # "stdout":"1","read":"1" — then 2, 3, … across requests
curl http://localhost:8080/loop      # "loop":{"passes":3,"stop":"done",…}
curl http://localhost:8080/exit      # "exit":3,"stderr":"nope\n"
curl -N http://localhost:8080/stream # lines appear one at a time, not all at once
```

What the responder sees:

- `WITH into = "_ws"` puts `{exit, stdout, stderr, stdout_truncated,
  stderr_truncated}` at `._ws` (default `._workspace`).
- `@workspace` (`_txc.workspace.*`) is the chassis's provenance stamp:
  provider, computer (the provider's id for it), run id, exit, duration.
  It is authored by the chassis after the command returns — a command
  can print anything it likes to stdout and never touch it.
- A transport failure (no wake, timeout, capacity) merges as top-level
  `workspace.error = {code, message}` with `._ws = {}`; the op is not
  dropped, so a rule can gate on `WHEN .workspace.error.code == "timeout"`.

`WITH workspace = <expr>` picks the workspace from data (`WITH workspace
= ._in.slug`); names are 1–4 `/`-separated DNS-label segments. `WITH
args = [...]` runs an argv without a shell; `stdin`, `cwd` (relative,
inside the workspace) and `env` (an object) round out the request. The
per-exec default timeout is `--workspace-default-timeout` (5m); `WITH
timeout` overrides it, `--op-timeout-max` (10m) caps it. The workspace
itself is never affected by a timeout — only the command is killed.

The trace shows one step per dispatch with `transport=workspace`, and
the usage log an event with `src=workspace`:

```
txco trace <rid> --step hello
```
