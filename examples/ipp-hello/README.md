# ipp-hello — a stack you can print to

A tenant presented to your computer as a **printer**. Print from any
application and the page arrives at the `_ipp` stack, which here simply
files it in the tenant's blob store under the printer's name. The chassis's
`ipp` personality is the printer; the stack decides what printing means.
Full reference: [docs/advanced/protocols/ipp.md](../../docs/advanced/protocols/ipp.md).

```
OPS/_ipp/0/
  keep.txcl                  every print job → txco://blob/put name = <printer>/<job id>, from_sha = the document
OPS/ipp-demo/
  090–095/provision_*.txcl   POST /ipp/provision: who may call it (open on loopback; Basic auth once IPP_PROVISION_PASSWORD is set)
  100/provision_parse.txcl   …its JSON body {"printer", "email", "name"}
  110/user.txcl              txco://user/create        → who may print (the email is the username)
  120/printer.txcl           txco://ipp/printer        → the printer: label, principal, display name
  120/credential.txcl        txco://credential/create  → the password, once, scopes ["ipp:*:*"]
  200/provisioned.txcl       …the answer: username, password, the printer's URI
  100/printed.txcl           GET /ipp/printed?printer=research → what that printer has received
  200/respond.txcl           …as JSON
  1000/notfound.txcl         anything else → 404 (including /p/… on this ordinary hostname)
```

Two things make a printer. The `_ipp` stack is what makes the tenant
printable at all; a **printer row** (`txco://ipp/printer`) is what makes
`/p/research` exist, and says whose it is. The label reaches the rule as
`@ipp.printer` and the person who printed as `@principal.id`. Register
`research` and `expenses` and the same rule files them under two names.

## Run it

```
txco dev --ipp                                                   # from this directory

curl -X POST localhost:8080/ipp/provision \
  -d '{"printer":"research","email":"you@example.com","name":"Research Pony"}'
# {"created":true,"display_name":"Research Pony","page":"https://ipp.localhost:8443/p/research",
#  "password":"k7m2-river-galaxy-bamboo-orbit-velvet","printer":"research",
#  "uri":"ipps://ipp.localhost:8443/p/research","username":"you@example.com"}
```

Until that row exists the printer does not (`404`). The password is shown
once: a printer holds none, and the chassis keeps only a hash. Provision a
second printer for the same email and no new password is issued — the one
you have opens every printer that person is granted (`ipp:*:*`).

`txco dev --ipp` serves HTTPS on `127.0.0.1:8443` with a self-signed
certificate kept in `.txco/dev/`, and `ipp.localhost` belongs to this tenant
because `localhost` is bound to the `ipp-demo` stack. Open
`https://ipp.localhost:8443/p/research` in a browser for the printer's page:
its name, an **Add printer** link, and the fields for adding it by hand.

### With CUPS's test client (no printer is added to your Mac)

```
U='ipps://you%40example.com:<password>@ipp.localhost:8443/p/research'          # the @ in the username is %40
T=/usr/share/cups/ipptool
ipptool -t $U get-printer-attributes.test                                    # printer-dns-sd-name = Research Pony
ipptool -t -f $T/document-letter.pdf $U validate-job.test
ipptool -t -f $T/document-letter.pdf $U print-job.test
ipptool -t -f $T/document-letter.pdf $U create-job.test                      # Create-Job + Send-Document
ipptool -t $U get-completed-jobs.test

curl "http://localhost:8080/ipp/printed?printer=research"
# {"count":2,"printed":[{"content_type":"application/pdf","name":"research/ipj_…","sha256":"13e3…","size":488245,…}],"printer":"research"}
```

Things to try: print `$T/color.jpg` (refused — only PDF is advertised);
copy a `.ps` file to `fake.pdf` and print that (refused — the bytes are not
a PDF); use a wrong password (`401`, and after 30 a minute, `429`); provision
a second printer for another email and print to it with the first password
(`401` — a printer is its principal's alone).

### As a real printer

```
lpadmin -U you@example.com -p pony -E -v ipps://ipp.localhost:8443/p/research -m everywhere
lp -d pony some.pdf            # or File → Print → pony, from anything
lpstat -W completed -o pony
lpadmin -x pony                # remove it again
```

CUPS accepts a self-signed certificate on first use. If the Printers &
Scanners dialog refuses it, trust `.txco/dev/web-selfsigned.crt` once —
`txco dev --ipp` prints the two `security add-trusted-cert` commands
(`cupsd` runs as root, so the System keychain as well as your own).

The chassis log shows an `ipp wire` line per request: the operation and
the *names* of the attributes the client sent, never their values. That log
is how you find out what a print client really does.

## What the rule sees

`@ipp.printer`, `@ipp.job_id`, `@ipp.job_number`, `@ipp.job_name`,
`@ipp.requesting_user` (whatever the client claimed — untrusted),
`@ipp.submitted_at` and `@ipp.document.{sha256,size,format,name}` — and
`@principal.{id,kind,credential}`: who signed in to print, pinned by the
chassis, which no client or rule can set. No bytes
and no `copies`: the document is already in the blob store, and this
printer has no paper. The run starts **after** the client was told the job
completed, so nothing the rule does — or how long it takes — changes what
the print queue shows.
