# Fleet — many chassis, one control plane

_One chassis needs none of this. When several serve the same tenants, each
change made on one (a deploy, a new hostname, a secret) has to reach the
others, and a new node has to start from where the fleet is. The chassis does
both with two pieces: a **control feed** that carries every change in order,
and **snapshots** a new node starts from._

## The control feed

A mutation on the admin API — `txco apply`, an activation, a tenant, hostname
or secret change — is written to an outbox in the same transaction as the
change. A pump publishes each one to the **feed sink** as a small event: what
changed, and a content-addressed reference to the artifact that holds it (a
stack version, for instance). The events themselves stay small.

Every node reads the **feed source** after its own cursor and applies events in
order. Each event has an id, and a node records the ones it has applied, so
a repeat delivery changes nothing.

| Flag | Default | |
|---|---|---|
| `--feed-sink` | `nop` | Where this node publishes its changes: `nop` (keep them local) or `file` |
| `--feed-source` | `nop` | Where this node reads the fleet's changes from: `nop` (the feed is off) or `file` |
| `--feed-source-file-dir` | `./chassis/data/feed` | The directory the `file` backend uses |
| `--artifact-store` | `file` | Where the artifacts the events point at live |

Open core ships the `nop` and `file` backends: `file` is a directory every node
can reach, enough for nodes on one host or a shared volume. A build can
register another backend under its own name; the hosted service uses a message
broker.

If a node missed something, `txco admin resync --tenant <slug>` re-emits that
tenant's whole state (its row, hostnames, active stack versions and secrets) as
fresh events. It only upserts, so it is safe to run again.

## Starting a node from a snapshot

A snapshot is the runtime database as a checksummed, versioned artifact:

```sh
txco snapshot export --out ./snapshot.snap        # to a file (+ snapshot.snap.manifest.json)
txco snapshot import ./snapshot.snap              # restore into an empty runtime DB
txco snapshot publish --alias snapshots/latest    # export + upload to the artifact store
```

`publish` prints the artifact's ref on its last line. A new node started with
`--snapshot-bootstrap-ref snapshots/latest` restores it before serving, **if its
runtime database is fresh**, then catches up on anything newer from the feed.
`import` refuses a populated database unless `--force`. An artifact from a
slightly older binary restores safely: the chassis applies newer migrations on
the next boot.

Publishing on a schedule (from cron, say) keeps the snapshot a new node starts
from close to the head of the feed.

## See also

- [The runtime reference](./serve.md) and [every flag](./flags.md)
- [The admin API](./admin-api.md)
