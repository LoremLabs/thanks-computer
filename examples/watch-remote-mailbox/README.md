# watch-remote-mailbox — pull a mailbox you already own

Where [imap-hello](../imap-hello) *serves* a mailbox a client can open,
this example does the opposite: it **watches** a mailbox that already
exists somewhere else — a support address at Fastmail, a Gmail account,
a self-hosted Dovecot — and turns each new message into a run of a
stack. You declare the mailbox once (a `SOURCES/` pack), store its
password once (a secret), and the chassis polls it, remembers how far it
has read, fires `watch-mailbox/_source/0` per new message, and moves the
handled message aside. No MX or forwarding change at the provider.

To keep the example self-contained it plays both sides: `txco dev
--imap` serves a local mailbox on `127.0.0.1:1143` (that is the "remote"
mailbox), and `txco dev --source` runs the poller that dials into it.
The `POST /seed` route provisions that local account and drops mail in;
in a real deployment there is no `/seed` — you point the source at your
provider and skip straight to "watch".

```
OPS/watch-mailbox/
  SOURCES/mailboxes.jsonl    the declaration: WHAT to watch, and the NAME of the password secret
  _source/0/record.txcl      one run per pulled message → kv/cas into the `received` namespace (dedup on @source.key)
  080/seed_parse.txcl        POST /seed → parse {"password"}
  085/seed_missing.txcl      …400 if no password
  100/seed_account.txcl      txco://imap/account — provision the LOCAL account the source will log into
  110/seed_append.txcl       txco://imap/append — drop a fresh message (object_key = @rid) into its INBOX
  200/seed_respond.txcl      JSON: mailbox + appended uid
  080/received_list.txcl     GET /received → txco://kv/list the `received` namespace
  200/received_respond.txcl  JSON: {received: <count>, keys: [...]}
OPS/_sys/boot/…              the standard dev system opstack (detect/route/static/404); 75/auto-route → watch-mailbox
```

## Username or password — which goes where?

Both, in different places, and the split is the whole security point:

- **Username** is configuration, not a secret. It lives in the `SOURCES/`
  pack, in the `user` field, in plaintext — it is committed with your
  ops. Same for `host`, `port`, `tls`, `mailbox`.
- **Password** is a secret. It never appears in the pack, on the command
  line as a stored value, in an envelope, a trace, or a log. The pack
  references it **by name** (`"secret": "PONY_MAILBOX"`); you store the
  value once with the CLI:

  ```
  txco auth tenant secrets set PONY_MAILBOX --tenant default    # hidden prompt
  ```

  The value is AES-256-GCM encrypted at rest and materialized only for
  the moment of the IMAP LOGIN, then zeroed. (These `txco auth …` and
  `curl` commands target the running dev chassis; if your active profile
  points at a cloud target, add `--url http://localhost:8081` so you
  don't write to prod.)

So: set the username in the file, set the password with `secrets set`.
For THIS example there is a twist — because the mailbox is one you also
provision locally, the password has to match on both sides: the value
you store as `PONY_MAILBOX` is the same one you hand to `/seed`, which is
what the local account's login is set to. For a real remote mailbox you
only do the `secrets set` half; the provider already owns the password
(use a per-mailbox **app password**, not your main one).

## Run the full loop

From this directory, in one terminal:

```
TXCO_SOURCE_PERIOD=5 txco dev --imap --source
```

`--imap` serves the local mailbox, `--source` runs the poller.
`TXCO_SOURCE_PERIOD=5` just makes it poll every 5s instead of the 30s
default so the demo is snappy. In a second terminal:

```
# 1. store the mailbox password (use this exact value below too)
txco auth tenant secrets set PONY_MAILBOX --tenant default          # enter: correct-horse-battery

# 2. own the domain so the local account can be provisioned under it
#    (*.local.thanks.computer resolves to loopback and auto-verifies in dev)
txco auth tenant hostnames add pony.local.thanks.computer --stack watch-mailbox --tenant default

# 3. provision the LOCAL watched mailbox + drop a message in it
curl -sS -X POST http://localhost:8080/seed \
  -H 'Host: pony.local.thanks.computer' \
  -d '{"password":"correct-horse-battery"}'
# {"seeded":true,"mailbox":"desk@pony.local.thanks.computer","appended_uid":1,"next":"…"}

# 4. within ~5s the source polls, fires a _source run, and moves the message.
#    See what it pulled:
curl -sS http://localhost:8080/received -H 'Host: pony.local.thanks.computer'
# {"received":1,"keys":["<uidvalidity>:1"],"next_cursor":""}

# run /seed again to add another → /received climbs to 2, and so on.
```

Watch the source's own state at any time:

```
txco source status --tenant default
# id       kind  cursor           claimed_by  next_poll   last_error
# watched  imap  {uidvalidity,1}  <node>      +5s         (none)
```

## What to look for

- **`/received` climbs by one per `/seed`.** Each key is `@source.key`,
  `"<uidvalidity>:<uid>"` — the message's stable identity at the source.
- **The message moves.** Open the mailbox in a client (server
  `127.0.0.1`, port `1993`, SSL, trust the self-signed cert; user
  `desk@pony.local.thanks.computer`, the password above) and you'll find
  handled messages in **Processed**, not INBOX — the source `MOVE`d them
  after the run succeeded. INBOX only holds mail not yet processed.
- **It's idempotent.** Re-running with the same message (a crash-replay,
  or a `Processed` folder the source can't reach) never double-records:
  the `kv/cas` on `@source.key` is a write-if-absent.

## Making it real

Point the `SOURCES/` pack at your provider and drop `/seed` entirely:

```jsonl
{"id":"support","kind":"imap","host":"imap.fastmail.com","port":993,"tls":"implicit","user":"desk@acme.com","secret":"ACME_DESK_MAILBOX","mailbox":"INBOX","every":"5m","on_processed":"move:Processed"}
```

Then `txco auth tenant secrets set ACME_DESK_MAILBOX` with the app
password, `txco apply`, and run a node with `source` in its
personalities. Your `_source/0` stack does the real work — file a
ticket, answer, hand off to an LLM — instead of just counting.

## Notes

- **Read + move only.** A source FETCHes and, on success, MOVEs or marks
  `\Seen`. It never deletes or expunges. If the server doesn't advertise
  `MOVE`, a `move` degrades to `\Seen` (logged) rather than emulating a
  move that could lose mail.
- **At-least-once.** A crash between a successful run and the move
  re-delivers next poll; `@source.key` is the dedup handle (here, the
  `kv/cas`).
- **Egress is guarded.** A source obeys `--egress-policy`. `txco dev`
  runs it `open`, which is why pointing at loopback (`127.0.0.1`) works
  here; production defaults to `private` and blocks RFC1918/loopback, so
  a self-hosted mailbox needs its range in `--egress-allow-cidrs`.
- **`/seed`'s password is in-flight cleartext — the source's is not.**
  Provisioning a local account inherently needs the password in the
  envelope, so `POST /seed` puts it there and (under `--trace-mode=full`)
  it appears in that request's trace. That is the demo route, not the
  feature: the source's own mailbox password lives ONLY in the secret
  store, is materialized inside the poller and zeroed after LOGIN, and
  never reaches an envelope, trace, or log — `grep`-ing a full trace for
  it after a poll returns nothing. A real deployment has no `/seed`.
- **`tls":"none"` is a dev convenience** for the loopback plaintext
  listener (the dev IMAP cert is self-signed, so the source can't verify
  `implicit`/`starttls` against it). A real source uses `implicit`
  (993) or `starttls` (143).

See [docs/advanced/protocols/source.md](../../docs/advanced/protocols/source.md)
for the full field reference and operational notes.
