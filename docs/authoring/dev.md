# `txco dev` — the development loop

_One command boots your services and a throwaway chassis, watches your
files, and re-applies on save._

```sh
txco dev
```

What happens, in order:

1. **Apps boot.** Each `apps:` entry in `txco.yaml` starts in its
   directory and is health-checked (its `health` URL, up to 60s)
   before the next.
2. **A workspace chassis boots.** State lives under `.txco/dev/`
   (gitignored) — your real chassis data is untouched. Web inlet on
   `:8080`, admin on `:8081`.
3. **Watching begins** (on by default). Saving a `.txcl` or `.json`
   file pushes it to a *sticky draft* of its stack — visible in the
   admin UI, not yet live. Saving a colocated compute (`.js`/`.ts`)
   rebuilds the wasm, uploads, and **activates** it. Saves are
   debounced (500ms).
4. **Ctrl-C tears down** in reverse: chassis first (stop accepting
   events), then apps (SIGTERM, 5s grace).

Useful flags: `--apply` (apply the full bundle at boot), `--ui` (admin
UI with Vite hot reload on `:6161`), `--tcp` / `--dns` (extra heads),
`--no-chassis` (use an already-running chassis at `--chassis-addr`).

## `txco.yaml`

Optional — a compute-only workspace needs none. Full shape:

```yaml
target: dev                       # default target name

apps:                             # services txco dev boots for you
  api:
    path: ./APPS/api
    start: node server.js
    health: http://localhost:4100/health

operations:                       # where op:// names point when remote
  NOTIFY:
    url: http://localhost:4100/notify

targets:                          # environments for apply/push/diff
  dev:
    chassis: http://localhost:8081
  prod:
    chassis: https://chassis.example.com:8081
    mock: deny                    # strip mock_res on deploy — see mocks.md
    operations:
      NOTIFY:
        url: https://notify.internal.example.com
```

`--target <name>` (on `apply`, `push`, `diff`, …) selects an entry;
per-target `operations:` URLs let the same `EXEC "op://NOTIFY"` rule
point at localhost in dev and the real service in prod.
