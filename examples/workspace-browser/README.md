# workspace-browser

A human sees and drives the workspace's **own** Chrome, in their browser, over
one WebSocket — no browser state is copied anywhere. This is the second
attached transport (`workspace://<name>/connect`, see
[`workspace-connect`](../workspace-connect)) carrying a VNC stream, and it is
the transport behind "take over my pony's browser"
(`thanks-computer-service/docs/todo-workspace-browser-takeover.md`). It also
carries that plan's Phase 3 demonstrator: a pony-side harness that drives the
same Chrome over CDP, and a control handoff that stops it while a human has
the controls.

> **Requires a real workspace provider (Sprites).** The workspace installs
> and runs Google Chrome + Xvfb + x11vnc; the local provider cannot, and on a
> Mac the setup script refuses rather than `apt-get` on your own machine. This
> example is therefore **not part of `scripts/examples-smoke.sh`** — run it
> against a chassis whose `--workspace-provider` is a real one, the way the
> hosted fleet runs. Proven end to end on a real sprite (2026-09-12/13).

## The shape

```
GET /setup    → fast every call: reports readiness; if the workspace is not
                ready it kicks PROVISIONING + LAUNCH off DETACHED and returns
                at once; the page polls it until {"ready":true}
GET /         → the page (served from a gated op, not a static file); calls
                /setup until ready, then opens the socket
WS  /screen   → accept; the first message runs connect WITH service="browser"
                (the built-in 127.0.0.1:5900 in the chassis service table);
                from then on the socket is the VNC stream noVNC renders
GET /take     → the human takes control: the pony harness is killed by pidfile
GET /release  → control goes back: the harness is restarted if its pid is gone
GET /pony/act → the pony side: it can act only while the harness pid is alive
GET /logs     → tails the workspace's harness/x11vnc/Xvfb/openbox/chrome logs
```

| Piece | What it does |
|---|---|
| `browser/090/auth.txcl` / `095/auth_reject.txcl` | the HTTP Basic gate over every route but `/healthz` (fails closed, see below) |
| `browser/100/setup.txcl` + `setup.sh` | readiness check + detached provision/launch; the runtime version is its `env` (`REQ`, data), the script (`setup.sh`, pulled in with `&include`) is fixed mechanism, fed on `stdin` to `bash -s` |
| `browser/200/setup_ok.txcl` / `setup_err.txcl` / `setup_fail.txcl` | the script's JSON verdict, or a 503 on a transport failure |
| `browser/100/home.txcl` | serves the page, base64-embedded; the editable source is `PAGE/index.html` (regenerate the base64 after editing) |
| `browser/100/upgrade.txcl` | accepts the WebSocket on `/screen` |
| `browser/_websocket/200/connect.txcl` | `workspace://tools/connect WITH service = "browser"` |
| `browser/100/take.txcl` / `release.txcl` / `pony_act.txcl` / `logs.txcl` | the control handoff and diagnostics, one exec each |

The pony harness (`ws-harness.py`: a stdlib-only WebSocket client that finds
Chrome's CDP page target on `:9222` and navigates it every few seconds, so a
watcher sees the pony working) is written into the workspace by `setup.sh`
from a base64 literal. There is no separate source file for it yet; to change
it, decode that literal, edit, re-encode.

## Provisioning vs cold start vs warm

`/setup` does three different amounts of work, and telling them apart is the
point:

- **Provision** — the `/var/lib/workspace-runtime/version` marker is behind
  `REQ`: install the packages, write the marker. About a minute, and only
  when the runtime version bumps. On amd64 the browser is **Google Chrome's
  `.deb`** from Google's apt repo (key and source list installed by the
  script) — Ubuntu's `chromium` apt package is a snap stub that never
  launches in the VM — plus `xvfb`, `x11vnc`, `openbox` (Chrome under Xvfb
  needs a window manager or the screen is black) and `fonts-liberation`; on
  other architectures, `chromium`. Because the version is data in
  `setup.txcl`, the workspace *evolves* — bump `REQ` and the next `/setup`
  re-provisions, no new image; a bump also retires the running daemons so
  they relaunch with new flags.
- **Cold start** — packages are present but the browser is not running:
  launch Xvfb `:99`, openbox, Chrome (`--remote-debugging-port=9222`, a
  persistent `--user-data-dir=$HOME/browser-profile`, `--no-sandbox`),
  x11vnc on 5900, then the harness. A second or two.
- **Warm** — everything is up: a sub-second no-op (guarded by `pgrep` and a
  listener check on 5900).

Provisioning runs **out of band** (`setsid bash /tmp/ws-provision.sh`, logging
to `/tmp/ws-setup.log`): the hosted edge allows 20 s for response headers and
an `apt-get` takes longer, so `/setup` returns at once and reports the latest
`PHASE` line while the script runs. Each `/setup` is a real exec, which also
wakes the sprite (it idle-sleeps ~30 s after the last exec) — the page
keep-alive-polls it while VNC is open so the display does not freeze.

## Run it (against a real provider)

Point a chassis at a real workspace provider (see
`thanks-computer-service/docs/runbook-workspaces-prod.md`), apply this stack,
set the password (below), open its hostname, click **open the browser**. First
click provisions; give it a minute. After that it is a cold start, then the
live browser.

## Verified on a real sprite (2026-09-12/13)

1. **`chromium` from apt is a snap stub** ("requires the chromium snap") that
   never launches in the VM; Google Chrome's `.deb` works. The script picks
   by architecture.
2. **Daemons must be detached from the exec's stdio.** Xvfb, openbox, Chrome
   and x11vnc are launched `setsid … </dev/null >log 2>&1` so they do not
   hold the exec's stdout open — otherwise the exec hangs until its timeout
   even though the work finished.
3. **x11vnc must not be `-localhost`.** The `connect` tunnel arrives from the
   sprite's gateway address, not loopback, so `-localhost` rejects it ("does
   not match 127.0.0.1"). The script allows `127.0.0.1` plus the default
   gateway (from `ip route`); 5900 is still reachable only through the
   authorized chassis tunnel.
4. **Xvfb needs `/tmp/.X11-unix`** (mode 1777) to exist.
5. **Chrome under Xvfb needs a window manager** (openbox) or the display is
   black.
6. **noVNC with a pre-opened socket works:** the page hands the WebSocket to
   `RFB(target, ws, …)` after the `{"type":"attached"}` ack (noVNC ≥ 1.3,
   pinned 1.7.0 — 1.5/1.6 do not exist on jsdelivr). The full path —
   connect → attached → x11vnc's `RFB 003.008` greeting → noVNC's reply — was
   traced end to end.
7. **Kill the harness by pidfile, never `pkill -f ws-harness.py`:** that
   pattern matches the shell running the kill itself.
8. When the handshake stalls at "connecting to the display…", `/logs`
   (x11vnc's log above all) says why.

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

## The control handoff (Phase 3 demonstrator)

`/take` kills the harness by its pidfile and logs the handoff to
`~/takeovers.log`; while it is dead the pony cannot act (`/pony/act` says so);
`/release` restarts it. Killing the process, not asking it to pause, is the
enforcement. What this example does NOT have is the product control model —
the lease as the single source of truth, the pony's own agent loop wired to
the harness, control handed back and forth from the pony's mail loop — which
is the takeover plan's Phase 4.

x11vnc runs `-nopw`: no VNC password, because 5900 is reachable solely through
the `connect` binding the chassis authorized. `--no-sandbox` is the pragmatic
choice for a single-tenant VM.
