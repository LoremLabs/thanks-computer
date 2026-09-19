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

Two things make a tenant a printer, and it needs **both**:

1. an active **`_ipp` stack** (`OPS/_ipp/0/…`) — the rules that receive
   print jobs; and
2. the **`IPP_PASSWORD` secret** — the credential a print client logs in
   with:

   ```
   txco auth tenant secrets set IPP_PASSWORD --tenant acme <target>
   ```

   The username is `print`; set an `IPP_USERNAME` secret to change it.
   Scope either secret to the `_ipp` stack (`--stack _ipp`) or leave it
   tenant-wide. Rotating the secret takes effect on the next request.

A tenant missing either one has no printers, and says so exactly the way
a hostname nobody owns does: `404`, before the request body is read. There
is one answer for every reason a printer is not there.

> **v1 credential.** One static credential per tenant. Printer-scoped
> accounts (a password that opens only `/p/research`) arrive with the
> reworked account subsystem; this page will change when they do.

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
that tenant's `_ipp` stack, that tenant's `IPP_PASSWORD`, the same one 404
for a handle that names nothing. The stack still sees only the printer
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
lpadmin -U print -p research -E -v ipps://ipp.acme.example:443/p/research -m everywhere \
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
queue that asks for credentials from its first print.

**`lpadmin` needs `-U <username>`.** `-U` names the printer's user.
Without it `lpadmin` offers your login name, retries 16 times and fails
with "Unable to query printer". It then asks once, in the terminal, for the
printer's password. Without `-o auth-info-required=username,password`,
the first job waits "on hold for authentication" until you authenticate
from the print queue window; after that, printing asks no more.

`<printer>` is any label you like (`a-z 0-9 . _ -`, lowercase). The chassis
keeps **no list of printers**: the label reaches the stack as
`@ipp.printer`, and one tenant can hand out `…/p/research`,
`…/p/summarize` and `…/p/expenses` as three printers that mean three
different things.

## What the stack receives

The `_ipp` stack runs once per job, at `_ipp/0`, **after** the client has
been told the job completed.

| Path | |
|---|---|
| `@ipp.printer` | the label after `/p/` — the operation selector |
| `@ipp.job_id` | opaque, durable, unique — key idempotence on this |
| `@ipp.job_number` | the integer job id the print client saw |
| `@ipp.job_name` | the title the application gave the job (may be empty) |
| `@ipp.requesting_user` | who the client **claims** printed it — untrusted |
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
answered without the password. A request over plaintext is refused (`403`)
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
document is challenged before its body is read. Verifications are throttled per
client IP and per tenant (`--ipp-auth-rate`), counting only logins that are
not already verified, so a client that re-authenticates on every request
costs nothing while a guesser is capped — correct guess included.

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
| `--ipp-auth-rate` | 30/min | verifications per IP and per tenant, cache misses only |
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

Logs carry tenant, printer, job id, format, size, duration and outcome.
They never carry a job name, a document name or a user name: a print
queue's titles are exactly what people do not want in a log.

## Try it locally

```
cd examples/ipp-hello
txco dev --ipp
txco auth tenant secrets set IPP_PASSWORD --tenant default dev
```

`txco dev --ipp` adds an HTTPS listener on `127.0.0.1:8443` with a
self-signed certificate kept under `.txco/dev`, and logs what every client
sends. `ipp.localhost` resolves to loopback and belongs to the tenant
`localhost` is bound to. CUPS's own test client speaks to it directly
(it trusts a self-signed certificate on first use):

```
U=ipps://print:<password>@ipp.localhost:8443/p/research
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
