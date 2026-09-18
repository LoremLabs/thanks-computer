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
  100/printed.txcl           GET /ipp/printed?printer=research → what that printer has received
  200/respond.txcl           …as JSON
  1000/notfound.txcl         anything else → 404 (including /p/… on this ordinary hostname)
```

The `_ipp` stack is what makes the tenant a printer. There is no list of
printers anywhere: `/p/<anything>` is a printer, and the label reaches the
rule as `@ipp.printer`. Print to `/p/research` and to `/p/expenses` and the
same rule files them under two names.

## Run it

```
txco dev --ipp                                                   # from this directory
txco auth tenant secrets set IPP_PASSWORD --tenant default dev   # type a password; username is `print`
```

Until that secret exists the printer does not exist (`404`). `txco dev
--ipp` serves HTTPS on `127.0.0.1:8443` with a self-signed certificate kept
in `.txco/dev/`, and `ipp.localhost` belongs to this tenant because
`localhost` is bound to the `ipp-demo` stack.

### With CUPS's test client (no printer is added to your Mac)

```
U=ipps://print:<password>@ipp.localhost:8443/p/research
T=/usr/share/cups/ipptool
ipptool -t ipps://ipp.localhost:8443/p/research get-printer-attributes.test   # anonymous, as Add Printer does
ipptool -t -f $T/document-letter.pdf $U validate-job.test
ipptool -t -f $T/document-letter.pdf $U print-job.test
ipptool -t -f $T/document-letter.pdf $U create-job.test                      # Create-Job + Send-Document
ipptool -t $U get-completed-jobs.test

curl "http://localhost:8080/ipp/printed?printer=research"
# {"count":2,"printed":[{"content_type":"application/pdf","name":"research/ipj_…","sha256":"13e3…","size":488245,…}],"printer":"research"}
```

Things to try: print `$T/color.jpg` (refused — only PDF is advertised);
copy a `.ps` file to `fake.pdf` and print that (refused — the bytes are not
a PDF); use a wrong password (`401`, and after 30 a minute, `429`).

### As a real printer

```
lpadmin -p pony -E -v ipps://ipp.localhost:8443/p/research -m everywhere
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
`@ipp.submitted_at` and `@ipp.document.{sha256,size,format,name}`. No bytes
and no `copies`: the document is already in the blob store, and this
printer has no paper. The run starts **after** the client was told the job
completed, so nothing the rule does — or how long it takes — changes what
the print queue shows.
