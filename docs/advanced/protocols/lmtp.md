# LMTP — accepting mail as events

*For operators building mail-driven op stacks (e.g. a support mailbox). Requires a colocated Postfix.*

The `lmtp` personality is a chassis head that speaks [LMTP](https://datatracker.ietf.org/doc/rfc2033/) (RFC 2033) to a colocated Postfix (or any LMTP client). Inbound mail becomes a normal event envelope (`_txc.src=lmtp`, `_txc.lmtp.*`) so resonators can SELECT on recipient, subject, headers, body, attachments, and emit per-recipient verdicts back to Postfix.

The chassis is a **destination MTA**, not a public-internet MX: Postfix owns TLS termination, SPF/DKIM/DMARC verification, greylisting, queueing, retries, and bounces. The chassis sits behind it and answers LMTP over a Unix socket (or a private TCP listener).

## Quick reference

| Flag / config key | Default | Meaning |
|---|---|---|
| `--personalities` | `cron,tcp,web,admin` | Add `lmtp` to enable the head. **Not default.** |
| `--lmtp-listen-addrs` | `:2424` | Comma list of `unix:/path` or `:port` / `host:port`. Default mirrors the chassis convention (high-port mirror of LMTP's well-known port 24, which needs root). Set empty to explicitly disable the head while keeping `lmtp` in `--personalities`. |
| `--lmtp-max-msg-bytes` | `26214400` | 25 MiB. Postfix sees `552 5.3.4 message too large` on overflow. |
| `--lmtp-max-recipients` | `50` | Per LMTP transaction. |
| `--lmtp-read-timeout` | `30s` | Per-command idle. |
| `--lmtp-data-timeout` | `60s` | DATA phase. |
| `--lmtp-resp-timeout` | `30s` | Pipeline response (envelope dispatch → rule verdict). |
| `--lmtp-hostname` | `os.Hostname()` | Greeting hostname. |
| `--lmtp-default-hosts` | _(empty)_ | Comma list. Enables [Strategy A](#strategy-a--operator-hosts-mail-for-many-tenants): rcpts `tenant.stack@<this host>` parse to `<tenant>/<stack>`. |

**Enabled by `--personalities`** — the personality string is the only gate. Once `lmtp` is in the list, the head binds `:2424` by default. 

To run alongside Postfix on a different port (or a Unix socket), override `--lmtp-listen-addrs`. Common production layouts:

```sh
# Co-located with Postfix on the same host (recommended).
txco serve --personalities=cron,tcp,web,admin,lmtp \
           --lmtp-listen-addrs=unix:/var/run/txco/lmtp.sock

# Cross-host LMTP over the well-known port (needs root or
# CAP_NET_BIND_SERVICE on the chassis binary).
txco serve --personalities=cron,tcp,web,admin,lmtp \
           --lmtp-listen-addrs=:24
```

## Postfix recipe

The canonical layout is **Postfix on port 25 / 587, chassis on a local Unix socket**. Postfix terminates TLS, runs SPF/DKIM/DMARC, and hands clean mail to the chassis over LMTP.

### `/etc/postfix/master.cf`

Postfix's default `lmtp` service entry is already a client (it speaks LMTP outbound). Nothing to add there — the work is in `main.cf`.

### `/etc/postfix/main.cf`

```cf
# Recipient domains the chassis should handle.
mydestination = localhost

# Hand every accepted message to the chassis over LMTP.
# `unix:` tells Postfix to dial AF_UNIX; the path is relative to
# $queue_directory unless it starts with `/`.
mailbox_transport = lmtp:unix:/var/run/txco/lmtp.sock

# Optional: enforce SPF/DKIM verification before delivering, so
# the chassis can read Authentication-Results headers and trust
# them (rules wouldn't trust unverified headers from the open
# internet, but Postfix's own stamps over the local LMTP are
# trustworthy).
smtpd_recipient_restrictions =
    permit_mynetworks,
    reject_unauth_destination,
    check_policy_service unix:private/policyd-spf
```

Reload Postfix (`postfix reload`); deliveries to `mydestination` now flow into the chassis.

### Socket permissions

The chassis's listening user must own the directory; Postfix's user must be able to write to the socket. On a typical Debian box:

```sh
useradd -r -s /usr/sbin/nologin txco
mkdir -p /var/run/txco && chown txco:postfix /var/run/txco && chmod 0750 /var/run/txco
```

The chassis creates the socket on bind; it picks up the permissions of the parent directory. Postfix runs as `postfix:postfix`; placing it in `txco`'s group lets it connect without weakening the directory.

Run the chassis with the Unix-socket listener (override the default `:2424`):

```sh
txco serve \
    --personalities=cron,tcp,web,admin,lmtp \
    --lmtp-listen-addrs=unix:/var/run/txco/lmtp.sock
```

### Cross-host (TCP) LMTP

If Postfix and the chassis live on different boxes, point Postfix at the chassis's TCP listener:

```cf
# Postfix main.cf
mailbox_transport = lmtp:inet:lmtp.chassis.internal:2424
```

```sh
# chassis — :2424 is the default, no flag needed
txco serve --personalities=cron,tcp,web,admin,lmtp
```

If you want to use LMTP's well-known port 24 instead, set `--lmtp-listen-addrs=:24` and grant the chassis binary the bind capability (`setcap cap_net_bind_service+ep /usr/local/bin/txco` on Linux) or run as root.

## Security Notice

**TCP LMTP has no built-in authentication** You must restrict access at the network layer (security group, WireGuard tunnel, firewall, etc.) — `--lmtp-listen-addrs=10.0.0.5:2424` plus an ingress allowlist. 

## Routing inbound mail

Each `RCPT TO` resolves **independently**. Recipients that land in the same `(tenant, stack)` get batched into one envelope; cross-tenant deliveries fan out into one envelope per tenant. Unrouted recipients short-circuit to 550 without invoking any pipeline.

The chassis offers two opinionated default strategies on top of operator-configured overrides, so most deployments don't need to enumerate every routable address.

> What follows is the operator-facing summary of the routing model.

### Resolution order (per RCPT TO)

Each recipient walks this list, first match wins:

| # | Rule | Source |
|---|---|---|
| 1 | `recipients[<exact addr>]` | YAML — operator exact override |
| 2 | `recipients["@" + domain]` | YAML — operator domain wildcard |
| 3 | **Strategy A** — `tenant.stack[+modifier]@<chassis-host>` parse | `--lmtp-default-hosts` config |
| 4 | **Strategy B** — `<verified domain>` → `<tenant>/_mail` | `verified_domains:` YAML stand-in **or** the chassis's `tenant_hostnames` DB (same table HTTP routing uses) |
| 5 | `listeners[<listener>]` | YAML — listener catch-all |
| 6 | _no match_ → `550 5.1.1` per recipient | default-deny |

Operator overrides (1, 2) always beat the default strategies; Strategy A beats Strategy B (more specific local-part shape); both beat the listener catch-all (tenant-specific routing wins over the operator's last-resort drop).

### Strategy A — operator hosts mail for many tenants

Set `--lmtp-default-hosts=chassis.example` (or supply a comma list). The chassis then parses any rcpt of the form `tenant.stack[+modifier]@chassis.example`:

| Address | Routes to |
|---|---|
| `acme.support@chassis.example` | `acme/support` |
| `acme.support+monday@chassis.example` | `acme/support` (modifier ignored for routing — it's just data on the rcpt string) |
| `acme.support+anything@chassis.example` | `acme/support` (same; rules read modifier from `_txc.lmtp.rcpt[i]` if they care) |
| `notatenant.fake@chassis.example` | unrouted → 550 (no chassis-side check that the tenant exists; if no rule fires in `notatenant/fake/0`, default-deny carries through) |

Both halves must be valid slugs (`[a-z][a-z0-9-]*`). The `+modifier` is RFC 5233 subaddress — parsed off the local-part for routing, then **not** propagated as a separate envelope field. Rules that want it split `_txc.lmtp.rcpt[i]` on `+` themselves.

```sh
txco serve \
    --personalities=cron,tcp,web,admin,lmtp \
    --lmtp-default-hosts=chassis.example
```

### Strategy B — tenant brings their own domain

When a tenant has verified a hostname (the same `tenant_hostnames` rows that authorize HTTP routing), mail to `anything@<that domain>` routes to `<tenant>/_mail`. The tenant authors `OPS/<tenant>/_mail/` to opt in — no `_mail` stack = no rule fires = default-deny 550.

Operationally the tenant still has to point MX records at the chassis (or its front-end Postfix). The chassis trusts whatever DNS-ownership proof `tenant_hostnames` carries (HTTP-01 challenge, etc.); pointing MX is a separate operational step the tenant takes — not a separate verification flow.

**Subdomain matching is exact.** Verifying `app.acme.example` routes mail to `*@app.acme.example` but NOT `*@acme.example`. Tenants who want all subdomains verify the apex.

Two ways to populate Strategy B:

```yaml
# Static stand-in for tenant_hostnames — useful for embedders
# running without the chassis DB, or for static deployments. Routes
# anyone@acme.example → acme/_mail (the convention). Override the
# stack with an explicit `stack:` field if your tenant uses a
# different name.
ingress:
  lmtp:
    verified_domains:
      acme.example:
        tenant: acme
      beta.example:
        tenant: beta
        stack: beta/inbound       # optional; default <tenant>/_mail
```

```sh
# DB-backed — the chassis queries tenant_hostnames at routing time.
# Same rows that authorize HTTP routing also authorize mail; no new
# table, no new verification flow. Strict mode rejects unverified
# rows.
txco serve \
    --personalities=cron,tcp,web,admin,lmtp \
    --require-hostname-verification    # optional but recommended for production
```

### Operator overrides

Need to send `vip@acme.example` somewhere other than `acme/_mail`? Add a `recipients:` entry — they beat both default strategies:

```yaml
ingress:
  lmtp:
    recipients:
      "vip@acme.example":
        tenant: acme
        stack: acme/exec_inbox
      "@partner.example":
        tenant: acme
        stack: acme/partners
    listeners:
      default:
        tenant: system
        stack: system/mail_drop    # last-resort catch-all
```

### Per-recipient envelope shape

When a delivery's RCPTs span multiple tenants, the chassis dispatches one envelope per tenant. Each envelope's `_txc.lmtp.rcpt` is the **group sublist**; `_txc.lmtp.transaction_rcpt` carries the full original RCPT TO list for rules that want to see who else was on the delivery.

```jsonc
// Example: one DATA to (alice@acme.example, bob@beta.example, carol@acme.example).
// Two envelopes dispatched. The acme envelope sees:
{
  "_txc": {
    "src": "lmtp",
    "route": {
      "tenant": "acme",
      "stack":  "acme/_mail",
      "ingress": "domain:acme.example",
      "to": "acme/_mail/0"
    },
    "lmtp": {
      "listener": "default",
      "rcpt": ["alice@acme.example", "carol@acme.example"],          // group sublist
      "transaction_rcpt": [                                            // full delivery
        "alice@acme.example", "bob@beta.example", "carol@acme.example"
      ],
      // … mail/msg/etc.
    }
  }
}
```

Per-recipient verdicts (next section) are indexed within the group's sublist; the inlet stitches them back to the original RCPT TO order on the wire.

## Writing rules

Once an envelope is routed into your stack, the LMTP fields are stamped under `_txc.lmtp.*`. Rule authors read them like any other envelope path.

### Envelope shape

```jsonc
{
  "_txc": {
    "src": "lmtp",
    "rid": "01HXX…",
    "route": {
      "tenant": "acme",
      "stack":  "acme/_mail",
      "ingress": "domain:acme.example",
      "to": "acme/_mail/0"
    },
    "lmtp": {
      "listener": "default",
      "client":   { "ip": "10.0.0.7", "helo": "mail.postfix.example" },
      "mail":     { "from": "alice@example.com", "size": 18342 },
      "rcpt":     ["support@your.tenant", "bcc@your.tenant"],
      "transaction_rcpt": [
        "support@your.tenant", "bcc@your.tenant"
      ],
      "msg": {
        "id":          "<CA+….@mail.gmail.com>",
        "date":        "2026-05-25T14:00:00Z",
        "from":        [{"name":"Alice","addr":"alice@example.com"}],
        "to":          [{"name":"","addr":"support@your.tenant"}],
        "cc":          [],
        "subject":     "wifi keeps dropping",
        "text":        "Hi support,\n\nMy wifi…",
        "html":        "<p>Hi support…</p>",
        "headers":     { "received": ["…","…"], "authentication-results": ["…"] },
        "attachments": [{"name":"…","type":"…","size":…,"sha256":"…","content":"b64:…"}],
        "raw":         "b64:…"
      }
    }
  }
}
```

- `_txc.route.*` is pre-stamped by the LMTP inlet (chassis-owned; not from client input). The boot pipeline honors the prior decision; rules read but don't write it.
- `_txc.lmtp.rcpt` is the **group sublist** — only the RCPTs that resolved to this envelope's `(tenant, stack)`. Use `_txc.lmtp.transaction_rcpt` to see every recipient on the original delivery.

`_txc.lmtp.msg.raw` is the full RFC 5322 bytes (b64), always present — the safe escape hatch for rules that want to re-deliver or archive the unmodified message. The parsed fields are best-effort and built from `raw`; a parse failure logs and falls through.

Header keys are lowercased and sorted for stable rule selectors and deterministic envelope hashes. Multi-valued headers (`Received`, `DKIM-Signature`) preserve order.

### Per-recipient verdicts

LMTP's whole reason for existing is the per-recipient status line: after `DATA`, the server returns **one status code per recipient**, in `RCPT TO` order. Rules express this via `_txc.lmtp.res.recipients[]`:

```txcl
WHEN @lmtp.rcpt.0 =~ /support@/
  SET @lmtp.res.recipients.0.code = 250
  SET @lmtp.res.recipients.0.msg  = "queued"
```

For the "treat all the same" case, a broadcast verdict applies to every recipient:

```txcl
WHEN @lmtp.msg.subject =~ /unsubscribe/
  SET @lmtp.res.code = 250
  SET @lmtp.res.msg  = "noted"
```

Resolution hierarchy at the inlet (default-deny throughout):

| Pipeline writes | Per-recipient outcome |
|---|---|
| `_txc.lmtp.res.recipients[i].{code,msg}` present | That slot's verdict |
| `_txc.lmtp.res.recipients[]` shorter than `rcpt[]` | **Missing slots → 550** (NOT inherited from the previous entry) |
| `_txc.lmtp.res.recipients[]` longer than `rcpt[]` | Extras logged + ignored |
| No array; `_txc.lmtp.res.{code,msg}` set | Broadcast to all recipients |
| Neither set | **Every recipient → 550** |

The short-array rule is load-bearing: **explicit accept is required per recipient**. A rule that writes `recipients.0.code = 250` and forgets `recipients.1` does NOT accept rcpt[1] — it 550s it. This prevents an "accept the first" rule from silently accepting later recipients it never considered.

### Status code idioms

Use the SMTP codes operators already know:

| Verdict | Code | When |
|---|---|---|
| Accept | `250` | Rule handled the delivery (queued, processed, intentionally dropped). |
| Tempfail | `4xx` (typically `451`) | Rule errored or upstream unavailable; Postfix queues and retries. |
| Reject | `5xx` (typically `550`) | Mailbox doesn't exist / policy violation. Postfix bounces immediately. |
| Mailbox full | `452` | Tenant over quota. |

The chassis emits sensible enhanced status codes (`5.1.1`, `4.3.0`, etc.) automatically based on the basic code — rules don't need to specify them.

### Gating on SPF / DKIM / DMARC

Postfix stamps an `Authentication-Results` header before handing the message to the chassis. Read it through the parsed headers:

```txcl
WHEN @lmtp.msg.headers.authentication-results.0 !=~ /spf=pass/
  SET @lmtp.res.code = 550
  SET @lmtp.res.msg  = "SPF failed"
```

The chassis does **not** verify SPF/DKIM/DMARC itself — that's Postfix's job (via `opendkim`, `opendmarc`, `policyd-spf`, etc.). If Postfix didn't stamp the header, rules should treat the message as unauthenticated.

## Verification

Once the chassis is up with the LMTP head (default `:2424`):

```sh
# Round-trip a delivery directly to the chassis.
swaks --protocol LMTP \
      --server localhost:2424 \
      --from   alice@example.com \
      --to     support@your.tenant \
      --header "Subject: wifi keeps dropping" \
      --body   "test"

# Expect: 250 OK (or 550 with your default-deny message if no
# rule accepted the recipient).

# Inspect the trace:
txco trace      # the latest envelope; src=lmtp, the rule fires, the verdict.
```

End-to-end through Postfix:

```sh
swaks --to support@your.tenant --server <postfix-host>
tail /var/log/mail.log
# expect: status=sent (250 2.0.0 queued)
```

## Default-deny: what unrouted mail sees

When LMTP is enabled but **no opstack resonates** with a delivery (no listener match, no recipient match, OR a route matched but no rule wrote `_txc.lmtp.res.*`), the chassis returns **`550 5.1.1 no rule accepted this recipient`** for every recipient. Postfix bounces back to the sender with a DSN.

This is deliberate. The alternatives:

- **`250 OK` by default** would silently blackhole mail — sender thinks the message was delivered, no bounce, message lost. Worst possible mail-server behavior.
- **`4xx` by default** would make Postfix queue and retry for days before bouncing — slow and noisy.

`550` produces a proper DSN immediately so the sender knows their message didn't land. An opstack must affirmatively SET an accept code (`250`) to deliver.

If you want a chassis-wide "always accept, drop the body, archive raw" catch-all (the explicit blackhole), author a `system/mail_catchall` stack:

```txcl
# OPS/system/mail_catchall/0/accept.txcl
SET @lmtp.res.code = 250
SET @lmtp.res.msg  = "accepted (archived)"
EXEC "op://ARCHIVE"
```

…and wire it under `ingress.lmtp.listeners.default`.

## Keeping sensitive mail out of trace logs

The chassis's trace subsystem captures the inbound envelope, every per-step input/output, and the final response under `<trace_dir>/requests/<rid>/`. For mail that pattern picks up cleartext bodies, attachment payloads, and any headers the rules touched — fine for development, less fine for production retention. Two reserved WITH keys on any rule in the routed stack scrub the trace artifacts at write time, without touching runtime data:

```txcl
WHEN @lmtp.rcpt.0 =~ /^support@/
WITH omit   = "_txc.lmtp.msg.attachments, _txc.lmtp.msg.raw"
WITH redact = "_txc.lmtp.msg.headers.authorization"
EXEC "op://CLASSIFY"
```

- `omit` deletes the path from the trace JSON entirely — best for big payloads (`_txc.lmtp.msg.raw`, `_txc.lmtp.msg.attachments`).
- `redact` replaces the value with `"[REDACTED]"` — best for header values where "something was here" is a useful signal.

The rule still SELECTs / EXECs against the full envelope; only what hits disk is masked. Scope is per-`(tenant, stack)` and hints take effect at the next dbcache reload. See [`docs/txcl.md` § WITH](../txcl/txcl.md#with--per-call-modifiers) for the full semantic.

## See also

- [`docs/ingress.md`](routing.md) — the ingress router this builds on.
- [`examples/inbound-support-mailbox/`](../../../examples/inbound-support-mailbox/) — worked example: route `support@…` to a classification + ticketing pipeline.
