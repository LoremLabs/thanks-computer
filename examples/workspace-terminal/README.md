# workspace-terminal

A real interactive terminal onto a workspace: `xterm.js` in the browser, a
pseudo-terminal in the machine, one WebSocket between them.

The stack accepts an upgrade on `/term`, and the page's first message runs
`workspace://tools/attach`, which binds a PTY (running `tmux new -A -s main`)
to that session. After that the socket carries the terminal's bytes straight
to and from the process — **the stack does not run per keystroke** — until
the tab closes, the process exits, or the lease's `max_duration` passes.

This is the interactive counterpart to
[`workspace-hello`](../workspace-hello), which runs one-shot commands, and it
builds on the [`websocket-counter`](../websocket-counter) personality.

## Run it

The terminal needs the local workspace provider **and** a real PTY, so:

```sh
cd examples/workspace-terminal
txco dev --allow-local-workspace
```

Open the URL `txco dev` prints, click **connect**, then click into the
terminal and type. `vi`, `top` and `htop` work at full speed with colour.
Resize the window and the terminal reflows. Close the tab and the process
dies; reconnect and `tmux` resumes the same screen.

## How it works

| Piece | What it does |
|---|---|
| `term/100/upgrade.txcl` | accepts the WebSocket on `/term` (an attachment is a capability the stack grants here, once) |
| `term/_websocket/100/parse.txcl` | decodes the first message `{"type":"attach","cols","rows"}` |
| `term/_websocket/200/attach.txcl` | `workspace://tools/attach` binds the PTY and returns the lease |
| `term/FILES/index.html` | `xterm.js` + the fit addon; input → binary frames, output → `term.write` |

### The wire protocol

- **Binary frames** are terminal bytes: client → stdin, process → client. No
  base64, no UTF-8 boundary problems.
- **Text frames** are a typed control envelope:
  - client → server: `{"type":"resize","cols":C,"rows":R}`,
    `{"type":"signal","signal":"INT"}`, `{"type":"detach"}`
  - server → client: `{"type":"exit","code":N}`, `{"type":"expired"}`,
    `{"type":"error","error":"…"}`

### Persistence is tmux's job

`tmux new -A -s main` gives reattach, scrollback and survival across
disconnects for free: the socket's death kills the tmux *client*, while the
server and the `main` session keep running in the workspace. The chassis
never learns about scrollback or screen state. (The workspace image must
have tmux; the Sprites image does.)

### Metering

An attachment is a metered lease, not a long request: it heartbeats every
60 s, priced at the ordinary workspace rate (1 fuel per 30 s), charged to
the tenant. `--workspace-attach-max-duration` (default 8h) caps how long one
attachment may live.
