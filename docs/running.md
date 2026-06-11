# Running a chassis

_Thanks, Computer runs your rules on a **chassis** — a single binary
that listens on every channel and executes your stacks. This page is
the loop for running one yourself; for a throwaway playground, use
`txco demo` from the [quickstart](./quickstart.md) instead.
([Overview](./overview.md))_

One command boots the whole thing:

```sh
txco serve
```

When it prints `-ready-`, it's listening: `:8080` for web events,
`:5050` for TCP, `:8081` for the admin API, and a cron tick every 60s.
No database to provision, no containers — state lives in local files
next to the binary.

## The author–apply loop

Rules live in your workspace as plain files under `OPS/`, one folder
per stack:

```sh
mkdir ~/my-stack && cd ~/my-stack
txco init hello                  # scaffolds OPS/hello/100/resonator.txcl
# …edit the rule…
txco apply                       # parses OPS/ and pushes it to the chassis
```

`apply` is idempotent — it reports what changed and skips what didn't.
`txco dev` wraps the loop (serve + watch + apply on save) into one
command. Then bind a hostname to the stack so events route to it:

```sh
txco auth tenant hostnames add localhost --stack hello
curl -s http://localhost:8080/hello | jq
```

Your rules are just files, so they version like code: review them in
pull requests, apply them from CI against `:8081`.

## Locked by default, in one step

Production chassis auth is signed requests (RFC 9421) — every admin
call carries an ed25519 signature, with replay protection built in.
Enrolling is one command on a fresh chassis:

```sh
txco auth bootstrap-local    # one-time: enrol your signing key
txco auth login              # opens the admin UI, authenticated
```

The admin UI shows your stacks, tenants, and [traces](./trace.md)
against the live chassis.

## Secrets stay out of your rules

The chassis has a built-in encrypted secret store, so credentials never
appear in rule files:

```sh
txco auth tenant secrets set API_KEY
```

A rule references the name, and the chassis splices the value in at
dispatch:

```txcl
WITH secrets.headers.authorization.secret = "API_KEY"
EXEC "https://api.example.com/enrich"
```

There is no reveal command — to inspect a value, you rotate it.

## When you want to see everything

Add `--trace-mode=full` to record every flow's complete story — see
[Trace](./trace.md).
