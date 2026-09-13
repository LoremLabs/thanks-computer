# workspace-browser

A human sees and drives the workspace's **own** Chromium, in their browser,
over one WebSocket — no browser state is copied anywhere. This is the second
attached transport (`workspace://<name>/connect`, see
[`workspace-connect`](../workspace-connect)) carrying a VNC stream, and it is
the transport behind "take over my pony's browser"
(`thanks-computer-service/docs/todo-workspace-browser-takeover.md`).

> **Requires a real workspace provider (Sprites).** The workspace installs
> and runs Chromium + Xvfb + x11vnc; the local provider cannot, and on a Mac
> the setup script refuses rather than `apt-get` on your own machine. This
> example is therefore **not part of `scripts/examples-smoke.sh`** — run it
> against a chassis whose `--workspace-provider` is a real one, the way the
> hosted fleet runs.

## The shape

```
GET /setup   → an idempotent exec that PROVISIONS (installs the runtime
               packages once, keyed by a version marker) and COLD-STARTS
               (launches Xvfb + Chromium + x11vnc if not already up)
GET /        → this page; calls /setup, then opens the socket
WS  /screen  → accept; the first message runs connect WITH service="browser"
               (the built-in 127.0.0.1:5900 in the chassis service table);
               from then on the socket is the VNC stream noVNC renders
```

| Piece | What it does |
|---|---|
| `browser/100/setup.txcl` | provisions + cold-starts; runtime version and package list are its `env` (data), the script is fixed mechanism |
| `browser/200/setup_ok.txcl` / `setup_err.txcl` | return the script's JSON verdict, or a 503 on a transport failure |
| `browser/100/upgrade.txcl` | accepts the WebSocket on `/screen` |
| `browser/_websocket/200/connect.txcl` | `workspace://tools/connect WITH service = "browser"` |
| `browser/FILES/index.html` | calls `/setup`, opens the socket, hands it to noVNC after the `{"type":"attached"}` ack |

## Provisioning vs cold start vs warm

`/setup` does three different amounts of work, and telling them apart is the
point:

- **Provision** — the `/var/lib/workspace-runtime/version` marker is behind
  `REQ`: `apt-get install` the packages, write the marker. Tens of seconds,
  and only when the runtime version bumps. Because the version and package
  list are data in `setup.txcl`, the workspace *evolves* — bump `REQ` and the
  next `/setup` re-provisions, no new image.
- **Cold start** — packages are present but the browser is not running: launch
  Xvfb + Chromium + x11vnc. A second or two.
- **Warm** — everything is up: a sub-second no-op (guarded by `pgrep`).

## Run it (against a real provider)

Point a chassis at a real workspace provider (see
`thanks-computer-service/docs/runbook-workspaces-prod.md`), apply this stack,
open its hostname, click **open the browser**. First click provisions; give it
a minute. After that it is a cold start, then the live browser.

## Caveats to verify on the first real run

Verified on a real sprite (2026-09-12): `apt-get install chromium` gives a
working `/usr/bin/chromium-browser` (a real deb, not a snap) and x11vnc binds
5900. Two things to know:

1. **Daemons must be detached from the exec's stdio.** Xvfb/Chromium/x11vnc are
   launched `setsid … </dev/null >log 2>&1` so they do not hold the exec's
   stdout open — otherwise the exec hangs until its timeout even though the
   work finished. Only the final JSON reaches stdout (the rest is a `{ } 1>&2`
   group).
2. **noVNC with a pre-opened socket.** The page hands the WebSocket to
   `RFB(target, ws, …)` after the `attached` ack — supported in noVNC ≥ 1.3 (pinned 1.7.0);
   if the pinned build behaves differently the handshake stalls at "connecting
   to the display…". Check the browser console. This is the one thing not yet
   proven end to end. (If a future image lacks a working `chromium` deb, the
   fallback is Google Chrome's `.deb` — bump `REQ`, change `PKGS`.)

## Restricting access

**This stack fails CLOSED**: because it hands over a logged-in browser, every
route returns 401 until you set a password. Enable access with a tenant secret:

```sh
txco auth tenant secrets set EXAMPLE_BROWSER_PW --tenant <this stack's tenant>
```

Until then, `/` and every action answer 401 (only `/healthz` stays open). Once
set, every route requires HTTP Basic auth: user `human`, password = that secret. The browser
prompts once on the page and reuses the credentials for the WebSocket and the
`fetch` calls. `/healthz` stays open for load balancers. The gate
(`browser/090`, `browser/095`) uses `txco://basic-auth-verify`, which consumes
the secret inside the op and compares it constant-time — the password never
reaches the envelope — and **fails closed** three ways: no secret set, wrong password, or a chassis
without the op all get 401. The `allow_unconfigured=false` decision is written in
`090/auth.txcl` where you can see it — flip it to `true` only if you truly want
the open-until-configured behavior.

The page is served from `browser/100/home.txcl` (base64-embedded), NOT a static
`FILES/index.html` — static files bypass the txcl pipeline, so a static page
would be world-readable and would never trigger the auth prompt. The editable
source is `PAGE/index.html`; if you change it, regenerate the base64 in
`home.txcl`.

## What this is not (yet)

This is the human **view** — Phase 2 of the takeover plan. It has no control
model: the pony's browser automation is not started here, and there is no
"who has the controls" handoff. That is Phase 3
(`todo-workspace-browser-takeover.md` §6), where entering human control stops
the pony harness and the lease is the single source of truth.

x11vnc runs `-nopw` on `127.0.0.1:5900`: no VNC password, because the port is
loopback-only and reachable solely through the `connect` binding the chassis
authorized. `--no-sandbox` is the pragmatic choice for a single-tenant VM.
