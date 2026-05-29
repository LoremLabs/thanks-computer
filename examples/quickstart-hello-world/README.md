# quickstart-hello-world

The example workspace from [docs/quickstart.md](../../docs/quickstart.md).

A multi-stage pipeline backed by a single Node.js service, and a tour of how the chassis routes a request to it.

## How routing works

Every request enters the chassis through one place: the **`_sys/boot` system pipeline** — a small, editable txcl stack the chassis ships embedded in the binary and (in `txco dev`) scaffolds into `OPS/_sys/` so you can see and change it. It lives in the same `OPS/` tree as your application stacks; the `_`-prefix is the only difference (`_`-prefixed = chassis-local/trusted, loaded locally; everything else = pushed via the admin API).

```
request → _sys/boot/0    EXEC "txco://detect-tenant"            # DECIDE: write _txc.route.* (no jump yet)
                         WHEN .../_txc/healthz → 200 "ok"        # parallel scope-0 op: health probe, halts
        → _sys/boot/100   WHEN @route.to != "" EXEC "txco://route"  # EXECUTE: re-tenant + goto
        → _sys/boot/1000  WHEN @tenant == "_sys" → 404            # nothing matched
```

The pipeline is split **decide → execute**, the opstack-native shape:

- **Scope 0 (`detect-tenant`) decides.** The hostname/listener/job → tenant resolver writes an *inert proposal* to `_txc.route.*` (tenant, stack, `to`). It does **not** jump or re-tenant — so any rule you add between 0 and 100 runs for **every** request and can read/modify the decision (rate-limit, deny, override the tenant).
- **Scope 100 (`route`) executes.** The visible `WHEN @route.to != ""` is the route-or-not decision (readable in the stack, not hidden in Go). When a route was proposed it promotes it to `_txc.goto`/`_txc.tenant` and the chassis re-tenants into that stack (one-way `_sys` → tenant). `txco://route` is pure mechanism — it's a Go op only because txcl `SET` can't copy one envelope path to another.
- **Scope 1000** is the unrouted 404 (no proposal → the gated route rule never fires) — rewrite it into a landing page, redirect, or onboarding stack.

Scope-window rule: **scopes 1–99 run for all traffic** (pre-execute); **101–999 run unrouted-only** (post-execute, before the 404). Scopes are sparse (0, 100, 1000) by convention, leaving wide gaps for hooks.

This replaces the old hardcoded-in-Go router *and* the old user-authored `boot/*` catch-all: routing is now an explicit, trace-visible, editable pipeline. The `boot/*` namespace is reserved for the system tenant (`_sys`); application stacks live under their own names (here, `hello-world`).

## Run it

Prerequisites: Node 18+ (uses `node:http` built-in — no `npm install`).

```sh
cp -r examples/quickstart-hello-world ~/my-workspace
cd ~/my-workspace
txco dev
```

On first run `txco dev` scaffolds `OPS/_sys/` from the binary (per-file no-clobber — it's already committed in this example, so it's left as-is; `--force-opstacks` overwrites). Edits to `OPS/_sys/` hot-reload in `dev` (no restart); `txco serve` is static after boot.

## Route a hostname to the stack

A fresh chassis has no hostname bindings, so every request is unrouted and gets the `_sys/boot` 404. Bind one:

```sh
# once, after the chassis is up:
txco auth tenant hostnames add localhost --stack hello-world

curl -i http://localhost:8080/anything
# HTTP/1.1 200 OK
# Content-Type: text/html; charset=utf-8
#
# <!doctype html><html>…<h1>hello world</h1>…</html>
```

That request flows: `_sys/boot/0` → `txco://detect-tenant` (resolves `localhost` → tenant `default`, stack `hello-world`) → one-way `_sys`→`default` re-tenant → `hello-world/100 → 200 → 1000` → HTML response.

An unbound host still 404s — by design, routing is explicit:

```sh
curl -i -H "Host: nope.example" http://localhost:8080/   # 404 from _sys/boot/1000
```

## See routing in action

Routing used to be invisible Go. Now it's a pipeline you can trace:

```sh
txco trace        # latest request's timeline shows:
#   stage.enter boot/0
#   op txco://detect-tenant
#   stage.enter boot/100
#   op txco://route
#   tenant.retenant  from=_sys to=default
#   stage.enter hello-world/100 …
```

## Edit the router

`OPS/_sys/boot/` is the system pipeline, editable like any stack:

- **`0/detect.txcl`** — `EXEC "txco://detect-tenant"` (DECIDE). Rarely changed.
- **`0/healthz.txcl`** — default `GET /_txc/healthz` → `200 ok`. A parallel scope-0 op gated on the path; halts before routing, so it answers even on a fresh chassis. Override or delete to change/remove the health endpoint.
- **`100/route.txcl`** — `WHEN @route.to != "" EXEC "txco://route"` (EXECUTE). The route-or-not decision is this visible WHEN.
- **`1000/notfound.txcl`** — the unrouted 404. Swap it for a branded page or a redirect.
- **Add a scope 1–99 rule** — runs for **every** request (before the route executes), gating on the `_txc.route.*` decision. Example — reject routes to an unverified hostname before they execute:

  ```txcl
  WHEN @route.to != "" && @route.hostname_verified == false
    EMIT @web.res.status = 403,
         @web.res.body = b64"forbidden\n",
         @halt = true
  ```

  Save it under `OPS/_sys/boot/50/policy.txcl` while `txco dev` is running; it hot-reloads. A scope 101–999 rule instead runs only on the *unrouted* path (after execute, before the 404). `@` is sugar for `._txc.`; `b64"…"` base64-encodes a body. (txcl `SET` takes literals — gate on `_txc.route.*`, set literals.)

## Layout

```
quickstart-hello-world/
├── txco.yaml                       # one app, four operations
├── APPS/
│   └── api/server.js               # Node http server, 4 routes
└── OPS/
    ├── _sys/                       # chassis-local: loaded locally, NOT pushed via apply
    │   └── boot/
    │       ├── 0/detect.txcl       # DECIDE  — EXEC "txco://detect-tenant"
    │       ├── 0/healthz.txcl      # parallel scope-0 op — GET /_txc/healthz → 200 "ok"
    │       ├── 100/route.txcl      # EXECUTE — WHEN @route.to != "" EXEC "txco://route"
    │       └── 1000/notfound.txcl  # unrouted 404 (editable)
    └── hello-world/                # application stack (pushed via the admin API)
        ├── 100/{hello,world}.txcl  # EXEC "op://HELLO" / "op://WORLD"  (parallel)
        ├── 200/sort.txcl           # EXEC "op://SORT"
        └── 1000/render.txcl        # EXEC "op://RENDER"
```

One `OPS/` tree, two load paths split by the `_` prefix: `OPS/_sys/…` is chassis-local — the chassis loads it directly from disk and `txco apply`/`txco dev` never push it to the admin API. `OPS/hello-world/…` is an application stack, pushed and tenant-scoped as usual. `OPS/_sys` is committed here so the router is visible without running anything; in your own workspace `txco dev` scaffolds it for you (per-file no-clobber).

## The service pipeline

Once a request re-tenants into `hello-world/100`:

| Stage | Rules | Envelope gains |
|---|---|---|
| 100 | `hello.txcl` + `world.txcl` (parallel) | `words: ['hello','world']` |
| 200 | `sort.txcl` | `sorted_words: ['hello','world']` |
| 1000 | `render.txcl` | `_txc.web.res.body` (base64 HTML) + content-type |

Scope numbers are sparse (`100, 200, 1000`); the chassis advances scope-by-one and uses scope-window semantics to find the next non-empty stage. The render op writes `_txc.web.res.*`; the web inlet returns those bytes (base64-decoded) with the requested Content-Type — curl sees HTML, not JSON. The `hello-world/*` rules need no `WHEN` of their own: they only run once routing has dispatched into the stack. One gate, one place.

## Scheduled work (per-tenant cron)

Author a stack literally named `_cron` to opt this tenant into a recurring
tick. Every `--cron-period` the chassis fans out one event into
`<tenant>/_cron/0`, pinned to this tenant, carrying precomputed wall-clock
fields under `_txc.cron.*` (`minute, hour, dom, dow, month, year, tick`, and
`mod5/mod10/mod15/mod30`). No `_cron` stack → no ticks (off by default); the
schedule itself is a visible `WHEN`, not Go config.

See `OPS/hello-world/_cron/100/heartbeat.txcl`:

```
WHEN @src == "cron" && @cron.mod10 == 1
  EMIT @cron.heartbeat = true
```

That wakes once every 10 minutes (`:01, :11, :21, …`). Trace it with a short
period: `txco dev --cron-period=5` and watch `_cron/0` ticks in the trace
list. Each tick is one normal request — it emits a single `usage` log line
(`src=cron`, `tenant=<this tenant>`), so scheduled work is metered like any
other traffic. Swap the `EMIT` for an `EXEC "op://…"` to do real work.

## Try changing it

- Add a third parallel word at scope 100 (`OPS/hello-world/100/cherry.txcl` → `EXEC "op://CHERRY"` plus a `CHERRY` op in `txco.yaml`). Sort and render pick it up automatically.
- Replace `OPS/_sys/boot/1000/notfound.txcl` with a branded "not found" page or a 302 to your marketing site.
- Add `OPS/_sys/boot/50/ratelimit.txcl` to shape **all** traffic before the route executes (scopes 1–99 are pre-execute).

Ctrl-C the `txco dev` terminal to tear down the chassis and the Node service cleanly.
