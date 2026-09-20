<!-- nav: IPP (printing) -->

# IPP — a printer as the UI

_The `ipp` personality presents a tenant to a computer as a **printer**.
File → Print → "research" in any application is one run of the tenant's
`_ipp` stack, with the printed document delivered by reference. The chassis
implements the printer; the stack decides what printing means._

```
ipps://ipp.<zone>:443/p/<printer>                          a tenant with a zone of its own
ipps://ipp.<structured suffix>:443/p/<handle>/<printer>    any tenant (the shared front door)
```

Print is already in every desktop application. Nobody has to find the
file, open a site and upload it: they print, and the page they were
looking at is in the stack's hands. What arrives is a **print
representation** — usually a PDF of what the application rendered — not
the original file. For "put this original file there", use
[WebDAV](./webdav.md); the two are different verbs.

IPP is HTTP (a `POST` of `application/ipp`: a binary attribute block, then
the document), so the head binds no listener: the **web head** hands it
every request whose hostname is `ipp.<something>`, and no other. A path
like `/p/…` on an ordinary hostname stays the stack's own.

## Turn it on

```
txco serve --personalities cron,web,admin,dns,ipp --dns-ipp
```

Two things make a printer, and it needs **both**:

1. an active **`_ipp` stack** (`OPS/_ipp/0/…`) — the rules that receive
   the tenant's print jobs; and
2. a **printer**, registered by a rule — its label, who may print to it,
   and what a computer should call it:

   ```txcl
   EXEC "txco://ipp/printer"
     WITH printer      = "research",          # …/p/research
          principal    = ._user.principal,    # who may print here
          display_name = "Research Pony"
   ```

A printer holds no password. The person (or thing) it is granted to signs
in with their own username and a password
[`txco://credential/create`](../users.md#credentials) issued them, whose
scopes cover `ipp:<printer>:print` — `["ipp:*:*"]` opens every printer they
are granted, and the same credential can open mail and the drive too. See
[The printer op](#the-printer-op) and [Users and credentials](../users.md).

A label with no active printer, on a tenant with no `_ipp` stack or a
hostname nobody owns: all one `404`, before the request body is read.

> **Upgrading.** The `IPP_PASSWORD` and `IPP_USERNAME` secrets and
> `--ipp-auth-rate` are gone, and `/p/<anything>` is no longer a printer.
> Register each printer with `txco://ipp/printer`, issue its principal a
> credential with an `ipp` scope, and give the client the new username and
> password. A queue that signs in as `print` stops working. The old secrets
> can be deleted once nothing runs the previous release.

### The printer op

`txco://ipp/printer` is tenant-scoped and answers at `into` (default
`_printer`).

| WITH | |
|---|---|
| `printer` | the label: `a-z 0-9 . _ -`, lowercase, starting and ending with a letter or digit. The last segment of the printer's URL |
| `principal` | who may print to it: `user:usr_…` from `user/create`, or your own `<kind>:<name>` |
| `display_name?` | what a client calls the printer (its `printer-dns-sd-name`), at most 63 bytes. Left out, an existing name is kept |
| `status?` | `active` (default) or `disabled` — a disabled printer is a `404` |
| `delete?` | `true` removes the printer (`principal` not needed). Jobs already accepted are still delivered |

The result is `{printer, principal, display_name, status, created}`, or
`{printer, deleted}`. It is idempotent: run it on every sign-up, or every
password reset, and it changes only what you pass.

**One principal per printer.** Only that principal's credential prints
there; anyone else's valid password is refused exactly like a wrong one.
Re-point a printer by running the op with another principal.

**The [creator-stack rule](../users.md#which-stack-may-manage-a-principal)
covers printers.** The calling stack must manage the `principal`, and the
principal must already exist — have a user record, a bound username or a
credential — so a printer cannot be pointed at a name another stack could
later claim. Changing or deleting a printer needs the principal it has now
as well.

Errors land at `<into>.error.{code, message}` as `txco_ipp_…`:
`invalid_arg`, `not_found` (the principal), `not_owner`, `no_tenant`,
`no_stack`, `store`, and `disabled` — the node has no printer store, which
is opened only where the `ipp` personality runs. Register printers from a
rule that runs there (a web request on the web tier does).

**What a registered label gives away.** A registered printer answers `401`
(and serves [its page](#add-the-printer)) where an unregistered label
answers `404`, so anyone can test whether a label exists; and with
`--ipp-anonymous-attributes` its display name is in the anonymous answer.
A label is not a secret — it is in the URL its owner hands out — so choose
display names you would put on a door.

### Which hostname

`ipp.<X>` belongs to the tenant that owns `X`, where `X` is — exactly, never
a subdomain of —

- the **origin of a delegated [DNS](./dns.md) zone** (`ipp.acme.example` for
  the zone `acme.example`). With `--dns-ipp` the chassis publishes
  `ipp.<zone>` A/AAAA at the edge IPs for every pattern zone, including
  zones that existed before the flag did, and the zone's wildcard
  certificate already covers the name; or
- a **verified hostname** bound to one of the tenant's stacks. This is what
  makes `ipp.localhost` work under `txco dev`, and what a custom domain that
  is not a delegated zone uses (that domain's owner adds the `ipp.` record).

Behind a front proxy that issues certificates on demand, the chassis's
`tls-ask` endpoint authorizes `ipp.<X>` under the same two rules.

### The shared front door

A tenant with no zone of its own lives under the platform's structured-host
suffix (`--structured-host-suffix`), as `<handle>.<suffix>` — say
`core-hmhzx2isby.stacks.example`. That zone belongs to nobody, so
`ipp.<suffix>` cannot take its tenant from the zone; it takes it from the
**path**, as the handle of any hostname the tenant has under the suffix:

```
ipps://ipp.stacks.example:443/p/core-hmhzx2isby/research
                               └─ handle ─────┘ └ printer
```

The handle is resolved through that hostname's own verified binding —
exactly as `ipp.<hostname>` would be — and then everything is the same:
that tenant's `_ipp` stack, that tenant's printers, the same one 404 for a
handle that names nothing. The stack still sees only the printer
label; the handle is addressing, like the hostname it stands in for.

It exists because of certificates. `ipp.<suffix>` is ONE label under the
suffix, so the suffix's wildcard certificate and wildcard DNS already cover
it: every tenant gets a printer URL with no zone, no record and no
certificate of its own. The obvious alternative, `ipp.<handle>.<suffix>`,
is two labels deep, where no wildcard certificate reaches (and `tls-ask`
never issues under the suffix). The handle form exists only on
`ipp.<suffix>`; on a tenant's own zone a second path segment is a 404.
The handle must be a single DNS label, so a path cannot name a host at
another depth. Under `txco dev` the suffix is `localhost`, which makes
`ipp.localhost` both this door (`/p/<minted handle>/<printer>`) and the
ordinary front door of the bound hostname `localhost` (`/p/<printer>`);
the two are told apart by the shape of the path.

A stack whose name sanitizes to the label `ipp` cannot have `ipp.<zone>` as
its hostname — that name is the front door — and gets a structured host
instead.

## Add the printer

macOS — Printers & Scanners → Add → **IP**:

| Field | Value |
|---|---|
| Address | `ipp.acme.example:443` |
| Protocol | Internet Printing Protocol – IPP |
| Queue | `p/research` |

or from a terminal:

```
lpadmin -U alice@acme.example -p research -E -v ipps://ipp.acme.example:443/p/research -m everywhere \
  -o printer-is-shared=false -o auth-info-required=username,password
```

Give the **port**. IPP's default is 631, which this head does not listen
on; CUPS treats port 443 as always-TLS, so `:443` is IPPS.

**The Add Printer dialog needs `--ipp-anonymous-attributes`.** Printers &
Scanners › Add, and an `ipps://…` link, which opens the same dialog, both
query the printer's capabilities before they have a password to offer.
Challenged, the dialog says "Unable to communicate with the printer" and
never asks for one (macOS 15.8). With the flag on, the dialog asks for the
username and password once, offers the Keychain, and builds an AirPrint
queue that asks for credentials from its first print. The queue is named
after the printer's `display_name`; a printer without one is named after
the host, which every printer on that host shares.

**`lpadmin` needs `-U <username>`.** `-U` names the printer's user: the
address its principal signs in with.
Without it `lpadmin` offers your login name, retries 16 times and fails
with "Unable to query printer". It then asks once, in the terminal, for the
printer's password. Without `-o auth-info-required=username,password`,
the first job waits "on hold for authentication" until you authenticate
from the print queue window; after that, printing asks no more.

**The printer's page.** The printer's address over https, for example
`https://ipp.acme.example/p/research`, is a web page for that printer. It
shows the printer's name (its registered `display_name`, else its label),
an **Add printer** button (the `ipps://` link,
which opens macOS's Add Printer),
and the three fields for setting it up by hand: Address with the port
spelled out, Protocol, and Queue. Hand people this link rather than the
fields.

- **Where it appears:** only for a registered, active printer. Everything
  else gets the same 404 as ever.
- **What it shows:** what its URL already says, and the display name. It
  carries no credential and no username.
- **Where it comes from:** the chassis builds it from `printer-ui/`
  (Svelte, one self-contained file embedded like the continuation page).
  A product that wants more, such as a setup profile or telling each
  person their username, builds that itself and links here.

**A printer is a row, and what it means is the stack's.** The label
reaches the stack as `@ipp.printer`, and one tenant can register
`…/p/research`, `…/p/summarize` and `…/p/expenses` as three printers that
mean three different things — for one person, or one each.

## What the stack receives

The `_ipp` stack runs once per job, at `_ipp/0`, **after** the client has
been told the job completed.

| Path | |
|---|---|
| `@ipp.printer` | the label after `/p/` — the operation selector |
| `@ipp.job_id` | opaque, durable, unique — key idempotence on this |
| `@ipp.job_number` | the integer job id the print client saw |
| `@ipp.job_name` | the title the application gave the job (may be empty) |
| `@principal.id`, `.kind`, `.credential` | who **signed in** to print it: the printer's principal and the credential it used. Pinned by the chassis; no client or rule can set it ([Who is acting](../users.md#signing-in)) |
| `@ipp.requesting_user` | who the client **claims** printed it — untrusted; use `@principal` |
| `@ipp.document.sha256` | the document, as a blob reference |
| `@ipp.document.size`, `.format`, `.name` | bytes, MIME type, file name if sent |
| `@ipp.host`, `@ipp.printer_uri` | the host and URI the job arrived on (through the shared front door the URI keeps its `/p/<handle>/…`) |
| `@ipp.submitted_at`, `@ipp.attempt`, `@ipp.node` | when, which delivery attempt, which node |
| `@client.ip` | the submitter's address |

All read-only. There are **no bytes in the envelope** — a 400 KB page and a
400 MB report make the same size envelope. The document is already in the
tenant's [blob](../blobs.md) store; adopt it under a name without copying
it, or read it:

```txcl
WHEN @src == "ipp"
  EXEC "txco://blob/put"
    WITH name = &concat(@ipp.printer, "/", @ipp.job_id),
         from_sha = @ipp.document.sha256,
         content_type = @ipp.document.format,
         into = "_kept"
```

```txcl
WHEN @src == "ipp" && @ipp.printer == "summarize"
  EXEC "txco://blob/get"
    WITH sha256 = @ipp.document.sha256,
         grants = &array("blob:cas:read"),
         into = "_doc"
```

There is no `@ipp.res`. By the time the stack runs there is nobody left to
answer.

### There is no paper

`@ipp.copies`, media, sides, quality: none of them exist. They are
properties of paper, and "17 copies, duplex, A4" means nothing to a stack
receiving a document. The printer advertises exactly **one** of everything
a print client insists a printer has (1 copy, one sheet size, one quality)
and never a choice, because a capability advertised is a promise. A client
that asks for the one value the printer has gets a plain OK; one that asks
for anything else still has its job accepted, and is told — in IPP's own
terms, `successful-ok-ignored-or-substituted-attributes` — which settings
meant nothing.

## A job's life

```
receiving   the document is streaming in
committed   it is in the blob store, the tenant owns it, the job is durable
delivered   the envelope was accepted onto the execution path   ← IPP "completed"
canceled    the client canceled before delivery
failed      the chassis could not receive or deliver it
```

**A print job ends at `delivered`.** The stack may then take a second or
three days, create a task, wait for a human, fail, or throw the document
away; none of that is a print job, and the printer neither waits for it nor
reports on it. A job is never shown as aborted because a stack was still
thinking about a document that was handed over perfectly well.

The document streams straight into the content-addressed store — its hash
is only known at the last byte, so it is spooled to a temporary location
and promoted at EOF (disk: temp file → link; S3: multipart → server-side
copy). The chassis never holds a document in memory. A job row that is
`committed` survives a restart and is delivered by whichever node gets to
it; a crash between the bus accepting an envelope and the row being marked
can deliver a job twice, which is why `@ipp.job_id` exists.

Jobs are kept for `--ipp-retention` and then forgotten. **Documents are
not deleted with them** — there is no blob garbage collection yet, so a
printed document stays in the tenant's store until something removes the
names pointing at it.

## What is accepted

`--ipp-formats` (default `application/pdf` only) is both what the printer
advertises and what it takes. A job must declare a listed format **and**
its first bytes must look like it; a PDF that is really PostScript is
refused (`client-error-document-format-error`) and nothing is stored.
`application/postscript` is never accepted however the list is configured:
PostScript is a program, not a page. Treat what arrives as hostile anyway
— the check is shallow by design, and parsing belongs to the stack.

| Refusal | IPP status |
|---|---|
| format not listed | `client-error-document-format-not-supported` |
| bytes are not the declared format, or empty | `client-error-document-format-error` |
| over `--ipp-max-job-bytes` | `client-error-request-entity-too-large` |
| compression other than `none` | `client-error-compression-not-supported` |
| `--ipp-max-inflight` uploads already running for the tenant | `server-error-busy` (clients retry) |
| a second document for one job | `server-error-multiple-document-jobs-not-supported` |
| cancel after delivery | `client-error-not-possible` |
| storage unavailable | `server-error-temporary-error` |
| node draining (new documents only) | HTTP `503` + `Retry-After` |

Operations: Get-Printer-Attributes, Validate-Job, Print-Job, Create-Job,
Send-Document, Get-Job-Attributes, Get-Jobs, Cancel-Job. Everything else
is `server-error-operation-not-supported`.

## Authentication, precisely

Basic over TLS, for **every** operation: nothing on an `ipp.` host is
answered without a password. The username is the address the printer's
principal signs in with, in full (`alice@acme.example`); the password is a
credential issued to that principal. A login passes four checks, in order:

```text
/p/<label> ──▶ printer row ──▶ username ─binding─▶ principal ─id in the password─▶ credential
                   │                                                                    │
                   │                                    its scopes cover ipp:<label>:print
                   └────────── the printer is granted to THAT principal ────────────────┘
```

Any of them failing is the same `401`. The chassis log tells them apart
(`ipp auth` with `outcome` = `failed`, `scope`, `grant`, `disabled`,
`throttled`, `denied`). **Revocation is immediate:** a verified password is
remembered for a few minutes so a client's stream of requests costs one
hash, but every request reads the credential, so a revoked one is refused
on its next use. The run a job starts is pinned to the principal that
created the job, and only that principal may send a job its document. A request over plaintext is refused (`403`)
before a credential is read, unless `--ipp-insecure-auth` (dev). A bare
request is challenged (`401`) and the client authenticates and asks again —
CUPS does this on its own. `--ipp-anonymous-attributes` (default off) is the
one escape hatch. It lets Get-Printer-Attributes alone through without
credentials, for a print client that queries a printer's capabilities while
*adding* it, before it has a password to offer. macOS's Add Printer dialog
is such a client (see [Add the printer](#add-the-printer)), so a
deployment that wants that dialog to work turns the flag on. The answer
holds nothing about the tenant: the fixed capability set, the label from
the URL, and an idle queue. It does confirm that a printer exists at that
hostname, which a `401` rather than a `404` already tells. Printing is never
anonymous. Every other operation is challenged, and a request carrying a
document is challenged before its body is read. Password checks are limited
per client IP and per principal by `--login-rate` — one budget shared with
the IMAP, CalDAV, CardDAV and WebDAV heads, so a guess here spends the same
budget as a guess there. It counts only logins that are not already
verified, so a client that re-authenticates on every request costs nothing
while a guesser is capped — correct guess included. Over it the answer is
`429`.

**Who, before what.** The head decides everything it can from the request
*headers*, before reading one byte of the body: a credential is verified,
or a request that is evidently carrying a document without one is
challenged, first. This is not tidiness. The IPP operation lives in the
body, and the first read of the body is what makes an HTTP server send
`100 Continue` — after which a print client starts streaming its document.
A `401` sent then lands mid-upload, the connection resets, the client never
sees the challenge, and CUPS retries the whole request, without
credentials, indefinitely (measured during development: one `ipptool`
produced ~50,000 Print-Job requests in two minutes). For the same reason a
refusal that can only be made after the header is read drains the document
before answering.

## Flags

| Flag | Default | |
|---|---|---|
| `--ipp-formats` | `application/pdf` | advertised and accepted formats, first is the default |
| `--ipp-max-job-bytes` | 256 MiB | per document |
| `--ipp-max-inflight` | 4 | concurrent uploads per tenant |
| `--login-rate` | 30/min | password checks per IP and per principal, shared with every head that signs in; cache misses only |
| `--ipp-anonymous-attributes` | false | let Get-Printer-Attributes (only) through without credentials |
| `--ipp-insecure-auth` | false | Basic over plaintext (dev) |
| `--ipp-store`, `--ipp-db-path` | `sqlite`, `./chassis/data/ipp.db` | the job store (its own file, never the runtime DB) |
| `--ipp-poll-interval` | 1 s | dispatcher retry cadence (a new job also wakes it) |
| `--ipp-dispatch-timeout` | 10 s | how long the bus may take to **accept** a job — never how long the run may take |
| `--ipp-lease-stale-after` | 600 s | crash recovery for a node that died mid-handoff |
| `--ipp-max-attempts` | 20 | deliveries before an undeliverable job fails |
| `--ipp-receive-timeout` | 600 s | a Create-Job waits this long for its document |
| `--ipp-retention` | 7 d | finished job rows |
| `--ipp-wire-debug` | false | log each request's operation and attribute **names** |
| `--dns-ipp` | false | publish `ipp.<zone>` for every delegated pattern zone |
| `--web-tls-self-signed` | false | dev: serve `--web-tls-addr` with a kept self-signed certificate |

Job lines carry tenant, printer, job id, format, size, duration and
outcome. They never carry a job name, a document name or the name the
client claims (`requesting-user-name`): a print queue's titles are exactly
what people do not want in a log. The sign-in line (`ipp auth`) carries the
login address and, once known, the principal and credential id, as the
other heads' do.

## Try it locally

```
cd examples/ipp-hello
txco dev --ipp
curl -X POST localhost:8080/ipp/provision \
  -d '{"printer":"research","email":"you@example.com","name":"Research Pony"}'
# → {"username":"you@example.com","password":"k7m2-…","uri":"ipps://ipp.localhost:8443/p/research",…}
```

The example's provision route runs the three ops: `user/create`,
`ipp/printer`, `credential/create`. The password is shown once.

`txco dev --ipp` adds an HTTPS listener on `127.0.0.1:8443` with a
self-signed certificate kept under `.txco/dev`, and logs what every client
sends. `ipp.localhost` resolves to loopback and belongs to the tenant
`localhost` is bound to. CUPS's own test client speaks to it directly
(it trusts a self-signed certificate on first use):

```
U='ipps://you%40example.com:<password>@ipp.localhost:8443/p/research'   # the @ in the username is %40
T=/usr/share/cups/ipptool
ipptool -t $U get-printer-attributes.test
ipptool -t -f $T/document-letter.pdf $U validate-job.test
ipptool -t -f $T/document-letter.pdf $U print-job.test
ipptool -t -f $T/document-letter.pdf $U create-job.test
ipptool -t $U get-jobs.test
ipptool -t $U get-completed-jobs.test
curl "http://localhost:8080/ipp/printed?printer=research"     # what the _ipp stack filed
```

All six pass against CUPS 2.3.4 (macOS 15). Printing the JPEG fixture is
refused as not supported; a PostScript file renamed `.pdf` is refused as a
format error.

## What a print client actually sends

Recorded with `--ipp-wire-debug`. CUPS 2.3.4 `ipptool`:

- speaks IPP **1.1** or 2.0, and always sends `Expect: 100-continue`;
- sends a request with no document with a `Content-Length`, and a document
  **chunked** (no length);
- does **not** authenticate pre-emptively: it sends the first request bare
  and answers the `401`;
- Get-Printer-Attributes asks for `all,media-col-database`, and its
  conformance test expects `media-col-default`, `printer-location` and
  `printer-more-info` in addition to the RFC 8011 basics;
- rejects a response whose unsupported-attributes group comes after the job
  group (RFC 8011 §4.1.3 order: operation, unsupported, then the object);
- sends `copies=1` in its job ticket.

macOS 15.8 Add Printer and print queues (recorded 2026-09-19, onepony
docs/0028):

- **Adding a printer.** Add Printer sends Get-Printer-Attributes bare, with
  no credential, and never answers a `401` with a password prompt. After
  three `401`s it gives up with "Unable to communicate with the printer". With
  `--ipp-anonymous-attributes` it adds the printer.
- **The queue it builds.** The printer gets an AirPrint PPD built from the
  attributes, with PDF only; `image/urf` is not needed. The queue is
  `auth-info-required=username,password` from the start, taken from
  `uri-authentication-supported=basic`.
- **The printer's name.** Add Printer also asks for `printer-dns-sd-name`,
  which this head does not send. So the queue is named after the host
  (`ipp.acme.example`), not the printer.
- **Printing.** A queue sends Validate-Job, then Create-Job + Send-Document
  (chunked), after one bare request that is challenged.
- **The Keychain.** It keeps one credential per host (`print` @ host, no
  port, no path), so every printer on a host shares it.
- **Configuration profiles** (`com.apple.mcxprinting`) install a queue
  without ever querying the printer. They use Apple's Generic Printer PPD,
  which passes PDF through.

Still to record: what Safari sends, and macOS 26.
