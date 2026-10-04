# Running a chassis

You can self-host a [Thanks, Computer](https://www.thanks.computer) runtime ("the chassis"), or
use the hosted cloud service. This page is the self-hosted path: start a chassis, enroll your
key, and deploy to it.

## Start it

```sh
txco serve
```

When it prints `-ready-`, it's listening: `:8080` for web events,
`:5050` for TCP, `:8081` for the admin API, and a cron tick every 60s.
No database to provision, no containers — state lives in local files
under the directory you start it from ([what to back up](./advanced/serve.md#data-on-disk)).

Those are the default heads. Mail, IMAP, DNS, calendars, the drive and the rest are
**personalities** you add with `--personalities`; every flag also reads from a `TXCO_*`
environment variable ([the runtime reference](./advanced/serve.md)).

## Enroll your key

Production chassis auth uses signed requests. Every admin call carries an ed25519 signature, with replay protection built in.

Enrolling is one command on a fresh chassis:

:::note
On first boot the chassis logs a one-time secret of eight words, with the exact
command to use it. Enrolling spends it; after that, admin access is by key only.
:::

```sh
txco auth bootstrap-local    # one-time: enroll your signing key (prompts for the secret)
txco auth login              # opens the admin UI, authenticated
```

More people join with invitations, not the secret
([identity & access](./advanced/cli.md#identity--access)).

## Deploy to it

Point your workspace at the chassis in `txco.yaml`:

```yaml
target: prod
targets:
  prod:
    chassis: https://chassis.example.com:8081
```

Then the loop is the one you use against `txco dev`:

```sh
txco apply          # deploy every changed stack in OPS/
txco status         # what differs between OPS/ and the chassis
txco trace last     # what the last request did, step by step
```

Every apply makes a new version of each stack it changes; `txco versions <stack>` lists
them and `txco activate <stack> --version N` rolls back to one. If someone else deployed since your last sync,
`apply` refuses rather than overwrite them
([the fast-forward rule](./advanced/cli.md#fast-forward-rule)).

## Where to go next

- [The runtime reference](./advanced/serve.md) — personalities, flags, data on disk, limits, network policy.
- [The admin API](./advanced/admin-api.md) — keys, enrollment and invitations in detail.
- [Routing](./routing.md) — binding hostnames to stacks.
- [Tenants](./tenants.md) — one chassis, many isolated worlds.
