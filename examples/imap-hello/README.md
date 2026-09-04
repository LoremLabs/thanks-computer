# imap-hello — an account a mail client can open

A stack that provisions an IMAP account and drops a hello message into
its INBOX, so Apple Mail or Thunderbird can open a mailbox a rule owns.
The chassis's `imap` personality serves the mailbox; the stack decides
what goes in it with `txco://imap/append`. Nothing lands there on its own.

```
OPS/imap-demo/
  080/autoconfig_host.txcl   GET /.well-known/autoconfig/mail/config-v1.1.xml → Thunderbird autoconfig (host from the request)
  085/autoconfig_respond.txcl  …the config-v1.1.xml: this host, 993, SSL, %EMAILADDRESS%
  090/provision_auth.txcl    POST /imap/provision → txco://basic-auth-verify (open until IMAP_PROVISION_PASSWORD is set)
  095/provision_reject.txcl  …401 unless the verdict is an explicit true
  100/provision_parse.txcl   POST /imap/provision → parse {"username", "password"?}
  100/folders_*.txcl         POST /imap/folders (create a role-tagged folder), GET /imap/folders
  110/account.txcl           txco://imap/account (argon2id; password generated when omitted)
  120/hello.txcl             txco://imap/append — a RECORD, rendered to RFC 5322 on FETCH
  130/folder.txcl            txco://imap/mailbox — role + per-verb policy
  140/*, 200/*               JSON responses
  1000/notfound.txcl         anything else → 404 (never `200 {}`)
OPS/_imap/0/
  observe.txcl               every committed client mutation, after the reply (@imap.phase observe)
  answer_readonly.txcl       a folder with role "readonly" refuses appends (@imap.phase answer)
  answer_allow.txcl          …everything else a stack-policy folder asks about is ok
```

Run it (the IMAP head is off unless asked for):

```
txco dev --imap                                   # from this directory
txco auth tenant hostnames add pony.local.thanks.computer --stack imap-demo
curl -X POST http://localhost:8080/imap/provision \
  -d '{"username":"paris@pony.local.thanks.computer"}'
# {"username":"paris@pony.local.thanks.computer","created":true,
#  "password":"xxxx-xxxx-xxxx-xxxx-xxxx-xxxx","hello_uid":1,"hello_noop":false,
#  "imap":{"host":"127.0.0.1","port":1993,"tls":true}}
```

The password is returned exactly once — only its hash is stored. The
routes in this example are **open on loopback** (it is a demo); the
guarantee that holds everywhere is in the op: `txco://imap/account` runs
only inside this tenant's rules and only for a domain the tenant owns. A
product decides who may call it in the route's WHEN clause. The
username's domain must be one the tenant owns (a verified hostname
binding or a delegated zone); `*.local.thanks.computer` resolves to
loopback and is auto-verified by `txco dev`, which is why the hostname
bind above is enough.

### Thunderbird finds the server by itself

Thunderbird's account setup fetches
`https://<domain>/.well-known/autoconfig/mail/config-v1.1.xml` for the
address's domain before it starts guessing hostnames. `080`/`085` answer
it with the request's host as the IMAP server, port 993, SSL — the
fleet's front door — so on a deployed stack a Thunderbird user types
only `paris@<stack host>` and the password. Thunderbird discards a config
without an `<outgoingServer>`, and there is no submission head, so the
file carries a placeholder SMTP entry (this host, 465) flagged
`useGlobalPreferredServer`: Thunderbird sends through the user's existing
default SMTP server instead. Apple Mail ignores autoconfig and needs the
server typed in either way. (Thunderbird does not consult `_imaps._tcp`
SRV records; the `dns` personality can publish them for clients that do.)

### Closing the provision route

`POST /imap/provision` closes the moment a password is set on the
tenant running the stack (`OPS/imap-demo/090` + `095`):

```
txco auth tenant secrets set IMAP_PROVISION_PASSWORD --tenant <slug>   # hidden prompt
curl -u provision:<that password> -X POST https://<stack host>/imap/provision \
  -d '{"username":"paris@<stack host>"}'
```

Without the header the route answers `401` with `WWW-Authenticate: Basic
realm=imap-provision`. `txco://basic-auth-verify` checks the header inside
the op, constant-time, and emits only a verdict — the password never
touches the envelope or a trace. The rule declares the secret
`optional` and says `allow_unconfigured = true`, which is the whole
policy in one visible line: **unset ⇒ open** (the demo), **set ⇒ Basic
auth**. Two things follow from that choice: a misspelled secret name
reads as "unset", so after deploying with a password, prove the gate
with one POST that carries no credentials and expect `401`; and on a
chassis without this op the verdict is absent and the reject rule still
fires, so the route fails closed there rather than open. The folders
routes stay open — gate them the same way if a deployment needs it.

The dev head serves a self-signed certificate kept at
`.txco/dev/imap-selfsigned.crt`. Trust it once so the mail client connects
without a warning (macOS; enter your login password when asked):

```
security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db .txco/dev/imap-selfsigned.crt
```

Then add the account in a mail client:

| Setting | Value |
|---|---|
| Account type | IMAP |
| Server / port | `pony.local.thanks.computer` / `1993`, SSL on (port `1143` speaks STARTTLS too) |
| Username | `paris@pony.local.thanks.computer` |
| Password | the one the route returned |
| Outgoing server | Apple Mail verifies one while adding the account and there is no submission head yet: run Mailpit (`brew install mailpit; mailpit --smtp-auth-accept-any --smtp-auth-allow-insecure`) and use `localhost:1025`, or a real SMTP account |

INBOX shows "Hello from your stack". Re-running the provision call with
the same username rotates nothing unless a password is given, and the
hello is a no-op (same `object_key`, same content). A wrong password is
`NO [AUTHENTICATIONFAILED]`; more than ten attempts a minute from one
address is `NO [LIMIT]`.

Or with a plain client:

```
$ openssl s_client -quiet -connect 127.0.0.1:1993     # or: nc 127.0.0.1 1143 (plaintext, dev only)
* OK [CAPABILITY IMAP4rev1 SASL-IR LITERAL- AUTH=PLAIN] IMAP server ready
a LOGIN paris@pony.local.thanks.computer xxxx-xxxx-xxxx-xxxx-xxxx-xxxx
b SELECT INBOX
c FETCH 1 (ENVELOPE BODY[TEXT])
```

Folders and the two lanes:

```
curl -X POST http://localhost:8080/imap/folders \
  -d '{"username":"paris@pony.local.thanks.computer","name":"Reference","role":"readonly","policy":{"append":"stack"}}'
curl 'http://localhost:8080/imap/folders?username=paris@pony.local.thanks.computer'
```

Now drag a message into `Reference` in the mail client: the head asks
the `_imap` stack first (`@imap.phase == "answer"`), the stack answers
`ok: false, code: "cannot"`, and the client shows "This folder is
read-only". Drag it into any other folder (or create one in the client)
and the stack hears about it afterwards (`@imap.phase == "observe"`) —
watch the trace for `._observed`.

See `docs/advanced/protocols/imap.md`.
