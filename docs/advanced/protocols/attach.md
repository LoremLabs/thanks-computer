# Attach — a workspace on a machine you own

_A workspace is normally a machine the chassis makes. `attach` lets it be
one you already have: your laptop, a server, a checkout with your logins
in it. You run one command there, and from then on `workspace://<name>/exec`
on the chassis runs on that machine._

An attached workspace is **one name, one credential, one connected
machine**. The stack that owns the name makes a one-use code. Someone runs
`txco attach <code>` in a folder on their machine. That machine dials out
to the chassis, proves it holds the key it enrolled, and holds the name
until it disconnects. Nothing on the machine listens, and the chassis
never dials in.

It is not a sandbox. Commands sent to an attached machine run as the user
who ran `txco attach`, with that user's environment, starting in the
folder they attached from. Attaching a workspace is a decision to trust
the stack that owns it with a shell there.

## The four ops

All four act on a workspace of the **calling stack**, named by `WITH
workspace`, exactly as `workspace://` resolves a name. There is no way to
name another stack's workspace. Output lands under `into` (default
`_attach`); a failure is `<into>.error.{code,message}`.

| Op | WITH | Answers |
|---|---|---|
| `txco://attach/invite` | `workspace`, `label`?, `ttl`? | `{ok, code, expires_at}`. The code is answered **once**; only its hash is kept. `ttl` is a duration, an hour by default and a day at most. `label` is shown to the person before they accept. |
| `txco://attach/status` | `workspace` | `{enrolled, attached, machine, root, os, version, since}` |
| `txco://attach/revoke` | `workspace` | `{ok}`. No request reaches the machine from then on. |
| `txco://attach/stop` | `workspace`, `job` | `{ok}`. Asks the machine to end a detached job. |

```txcl
WHEN @web.req.url.path == "/attach/new"
  EXEC "txco://attach/invite"
    WITH workspace = "computer/rain", label = "Rain's computer", into = "invite"
```

`enrolled` and `attached` are two facts kept in two places. **Enrolled**
is durable: a machine holds this name, until the workspace is destroyed.
**Attached** is whether that machine is connected right now. A name that
is enrolled and not attached is _away_.

A name the workspace provider already holds cannot be invited:
`error.code = "name_in_use"`. Attaching never destroys anything. Give the
attached machine a name of its own beside the one the provider serves, or
destroy that workspace first.

## On the machine

```sh
cd ~/projects/onepony
txco attach granite-pillow-…-otter@attach.example.com
```

The command says what it is about to attach to and where, and asks. Then
it serves, in the foreground, until it is stopped.

- **The folder is taken once.** It is the directory `txco attach <code>`
  was run in, recorded as an absolute path. Every command starts there
  unless it names another directory, and a relative `cwd` is below it.
  An absolute `cwd` is used as given: the folder is where work starts,
  not a fence.
- **Running it again changes nothing.** `txco attach`, with no code and
  from any directory, serves every workspace the machine holds, each from
  the folder it was enrolled in. Moving a workspace to another folder or
  another machine is a new code, enrolled there; the old machine is told
  it was replaced and stops.
- **`txco attach list`** says what the machine holds, and where.
- **Commands get two variables,** set last: `TXCO_ATTACH_ROOT`, the folder,
  and `TXCO_ATTACH_STATE`, a directory outside it that a tool may keep its
  own files in.

Everything the command keeps is in one folder, `attach/` under the txco
home (`$TXCO_HOME`, else `~/.config/txco`):

```text
attach/
  credentials/<id>.json   the address, the folder and the key; mode 0600
  OPS/_attach/…           your own rules for the _attach stack
  state/<id>/             TXCO_ATTACH_STATE
  jobs/<job>/             detached jobs
  chassis/                the small chassis's own stores
  attach.log              one line a request, sizes only
```

### It is a chassis

`txco attach` is the txco binary run as a small chassis with a fixed
profile: a processor, one personality that dials out, and a workspace
provider that runs commands as you. Every `TXCO_*` setting in the
environment is ignored except `TXCO_HOME`.

A request from the other chassis is not run by the link. It is handed, as
data, to a stack on your machine named **`_attach`**, and that stack's
rule runs it. The built-in rule is short:

```txcl
WHEN @src == "attach" && @attach.req.shell == true
  WITH workspace = @attach.credential,
       command   = @attach.req.command,
       stdin     = @attach.req.stdin,
       cwd       = @attach.req.cwd,
       env       = @attach.req.env,
       timeout   = @attach.req.timeout_ms,
       detach    = @attach.req.detach,
       into      = "attach"
  EXEC "workspace://machine/exec"
```

It is yours to change. A file under `attach/OPS/_attach/exec/<scope>/`
with the same name as a built-in rule replaces it; any other name adds a
rule. So a log line, a command policy or an extra environment is a rule
you write, not a setting:

```txcl
# attach/OPS/_attach/exec/0/command.txcl: refuse anything that is not git.
WHEN @src == "attach" && @attach.req.shell == true && @attach.req.command =~ /^git /
  WITH workspace = @attach.credential, command = @attach.req.command,
       cwd = @attach.req.cwd, env = @attach.req.env,
       timeout = @attach.req.timeout_ms, into = "attach"
  EXEC "workspace://machine/exec"
```

A request no rule runs is answered `not_allowed`.

Two things are fixed. Every request enters `_attach`: nothing the other
chassis sends can choose another stack. And only `_attach` may run a
command on an attached machine: its workspace provider refuses every
other tenant. What goes back up the link is the run's `attach` object and
its `workspace.error`, and nothing else.

## What an exec can answer

An exec on an attached workspace answers as any exec does, with two more
`workspace.error.code` values:

| Code | Means | Safe to send again? |
|---|---|---|
| `away` | The machine is not connected, or was refused. **Nothing was delivered.** | Yes |
| `unknown` | The request may have reached the machine and was not seen to end. It may have run, in whole or in part. | Not without checking |

The chassis never sends a request twice. `unknown` is for the stack, and
often a person, to settle.

A command that simply runs out of time is `timeout`, as on any workspace:
the machine ends it and says so. `stream`, `tty`, `attach`, `connect`,
`checkpoint` and `sleep` answer `unsupported` on an attached workspace.

## Long commands: `mode = "async"`

A laptop sleeps, and a connection drops. A command that takes an hour
should not need either to hold.

```txcl
EXEC "workspace://computer/rain/exec"
  WITH command = "make all", mode = "async", max_duration = "1h", into = "build"
```

The run **suspends** at this op, durably, as any async op does. The
machine starts the command detached and answers at once. Nothing is held
open while it runs. When it ends, the machine sends the result up its
link, on whatever connection it then has, and the run resumes with the
result under `into`, shaped exactly like a held exec's.

- **`max_duration` is required.** It is the command's life and the run's
  expiry. The machine ends the command a little before it, so that a
  connected machine reports `timeout` before the run expires.
- **`timeout` bounds only the handoff** to the machine, 30 s by default.
- **The command outlives `txco attach`.** It runs under a wrapper that
  does not depend on it. Stop `txco attach`, even with `kill -9`, and the
  command runs on, its exit kept on disk; the next `txco attach` sends it.
- **A failure is data.** A command that fails, runs out of time
  (`timeout`) or could not be delivered (`away`) resumes the run with
  `workspace.error`, so the scopes after it decide what to do.
- **A handoff that was not confirmed is left open.** The command may be
  running. Its own report settles the op when it arrives, and the run's
  expiry does if it never does.
- **A machine that never comes back** never reports. The run expires at
  `max_duration`; a stack that must act on that keeps a deadline of its
  own.
- **Not with `secrets`, `grant`, `stream` or `tty`.** The result arrives
  on a later request, where a secret's value is no longer there to redact
  from it.

`mode = "async"` needs a workspace whose machine reports a command's end
itself, which today means an attached one. On any other workspace it
answers `unsupported`; use `mode = "continuable"` there. A stack that
serves both picks the mode with the workspace:

```txcl
WHEN .on_attached == true
  WITH workspace = .workspace, command = .command,
       mode = "async", max_duration = "1h", into = "build"
  EXEC "workspace://pony/exec"

WHEN .on_attached != true
  WITH workspace = .workspace, command = .command,
       mode = "continuable", timeout = "30m", into = "build"
  EXEC "workspace://pony/exec"
```

## Ending it

- **`txco://attach/revoke`** takes effect at the very next request: the
  credential is checked before each one is forwarded, not only when the
  machine connects. The name stays enrolled, so it reads `away` and is
  **not** handed back to the workspace provider.
- **`workspace://<name>/destroy`** on an attached name revokes the
  credential and gives the name back. Nothing on the machine is removed.
- **On the machine,** stop the command. Its credential stays; run `txco
  attach` to serve again.

## How the connection is kept honest

- **The key is proved twice.** The enrollment is signed by the key it
  enrolls, over the exact bytes sent, so a key cannot be enrolled by
  someone who does not hold it. Every connection then answers a challenge
  bound to that connection, that credential, the host the machine dialed
  and this protocol's version; an answer cannot be replayed anywhere else.
- **One connection holds a name.** Each authenticated connection gets a
  generation number that only goes up. A newer one replaces an older one,
  and an older one can never take the name back, in any order of events.
- **A machine cannot choose its workspace.** It names a credential; the
  workspace comes from that credential's record.

## Running the head

Off by default. Both must be set:

- `attach` in `--personalities`
- `--attach-listen-addrs`, for example `:8090`

`--attach-public-url` is the address machines dial, scheme and host. It is
appended to each code, as `<code>@<host>`, so the person pasting it does
not have to know it. Put the listener behind the same edge as your other
hosts; it speaks HTTP and upgrades to a WebSocket.

`txco dev --attach --allow-local-workspace` starts it on `:8090` for
trying this on one machine.

### More than one node

A machine's link lands on one node, and an op may run on any. Two things
make that work, and both are needed:

- a **shared `--kvstore`** (redis), where the claim that names a link's
  node lives;
- **`--attach-relay`**, naming a relay backend that carries a request to
  that node and its answer back.

Without a relay, a workspace is reachable only from the node that holds
its link, and reads `away` from every other. The bundled chassis defines
the relay's seam and ships no backend, as for `--websocket-relay`.
