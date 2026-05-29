# Quickstart

Boot a chassis, author your first resonator, and watch an operation respond.
About 5 minutes.

## Prerequisites

- **Go** (see `go.mod` for the version) — to build from source
- **macOS or Linux** (Windows isn't supported yet)
- `curl` and a JSON viewer like `jq` or `python3 -m json.tool`

## 1. Install `txco`

```sh
brew tap loremlabs/txco && brew install txco
```

Or build from source:

```sh
git clone https://github.com/loremlabs/thanks-computer.git
cd thanks-computer
make install        # installs `txco` into $(go env GOPATH)/bin
```

`txco` is a single binary for everything — running the chassis (`txco serve`),
authoring resonators (`txco init` / `apply` / `diff`), and the all-in-one dev loop
(`txco dev`). Verify:

```sh
txco help
```

## 2. Run the chassis

```sh
txco serve
```

Output ends with `-ready-`. The chassis is now listening on:

- `:8080` — the **web inlet** (where events arrive)
- `:5050` — the **TCP inlet** (line-delimited JSON)
- `:8081` — the **admin API** (where `txco apply` pushes op stacks)
- a cron tick fires every 60s

Leave it running; open a second terminal for the rest.

## 3. Confirm it's alive

```sh
curl -s http://localhost:8080/ | jq
```

You'll see the **JSON envelope** the chassis wraps every event in — with no op
stacks defined yet, the event round-trips unchanged:

```json
{
  "_ts": "2026-05-11T11:52:13+02:00",
  "_txc": {
    "rid": "312TVU1pFqe9NZwzc",
    "src": "http",
    "web": { "req": { "url": { "path": "/" }, "method": "GET" } }
  }
}
```

Resonators read fields from `_txc.*` (and the payload) to decide what responds.

## 4. Write your first resonator

Create a workspace anywhere:

```sh
mkdir ~/my-stack && cd ~/my-stack
txco init hello                     # scaffolds OPS/hello/100/resonator.txcl
```

`hello` is an op stack — a namespace. Edit the scaffolded resonator so an
operation responds when the request path is `/hello`:

```sh
cat > OPS/hello/100/resonator.txcl <<'EOF'
WHEN @web.req.url.path == "/hello"
EMIT .greeting = "Hello from the chassis!"
EOF
```

| Clause                               | What it does                                                                                       |
| ------------------------------------ | -------------------------------------------------------------------------------------------------- |
| `WHEN @web.req.url.path == "/hello"` | The resonance condition — fire only when the path is `/hello`. `@` is shorthand for `._txc.`.      |
| `EMIT .greeting = "…"`               | Overlay this field onto the response. No `EXEC` needed — this resonator just shapes the flow.      |

That's a complete resonator: a condition and what to emit when it fires. To call
an external service or a built-in instead, add an `EXEC` — see
[txcl.md](./txcl.md) for the full language.

## 5. Push it

```sh
txco apply
# → applied 1, skipped 0 (unchanged)
```

`apply` walks `OPS/`, parses each `*.txcl`, and pushes them to the running chassis.

## 6. See it fire

Requests reach a stack once a hostname is bound to it. Bind `localhost → hello`
(once, while the chassis is up):

```sh
txco auth tenant hostnames add localhost --stack hello
curl -s http://localhost:8080/hello | python3 -m json.tool
```

```json
{
  "greeting": "Hello from the chassis!",
  "_ts": "2026-05-11T11:55:31+02:00",
  "_txc": { "rid": "...", "src": "http" }
}
```

`greeting` is the field your resonator added. A request to any other path passes
through unchanged — its resonator didn't fire.

## Explore in the browser

The chassis ships a web interface — author resonators, fire events, and inspect
the full trace of each flow without leaving the page.

```sh
txco demo
```

Zero config: `txco demo` boots a throwaway local chassis and opens the
**demo** in your browser. No workspace, no setup — it picks free ports, so
it runs happily alongside a `txco serve` you already have going. The fastest way
to learn TXCL.

```sh
txco auth bootstrap-local      # one-time: enrol your signing key
txco auth login                # opens the admin UI with an authenticated session
```

For a chassis you run yourself, `txco auth login` mints a browser session (signed
with your key) and opens the **admin UI** — browse stacks, tenants, and traces
against your live chassis.
