# contacts-hello — an address book a contacts app can open

A stack that provisions a contacts account, creates an address book with a
hello card — so Apple Contacts, Thunderbird or DAVx⁵ can show an address
book a rule owns. The chassis's `contacts` personality serves it over
CardDAV; the stack decides what goes in it with `txco://contacts/put`.
Nothing lands there on its own.

```
OPS/contacts-demo/
  100/provision_parse.txcl   POST /contacts/provision → parse {"username", "password"?}
  110/account.txcl           txco://contacts/account (argon2id; password generated when omitted)
  110/missing.txcl           …400 without a username
  120/addressbook.txcl       txco://contacts/addressbook — "hello", policy put/delete = stack
  130/hello.txcl             txco://contacts/put — a card from card{} (the chassis renders the vCard)
  200/*                      JSON responses (the password appears once)
  1000/notfound.txcl         anything else → 404 (never `200 {}`)
OPS/_contacts/0/
  observe.txcl               every committed client mutation, after the reply (@contacts.phase observe)
  answer_email.txcl          a card without an email address is refused (@contacts.phase answer)
  answer_allow.txcl          …one with an address is accepted as written
  answer_other.txcl          groups, deletes, renames: allowed
```

Run it (the contacts head is off unless asked for):

```
txco dev --contacts                               # from this directory
txco auth tenant hostnames add pony.local.thanks.computer --stack contacts-demo
curl -X POST http://localhost:8080/contacts/provision \
  -d '{"username":"paris@pony.local.thanks.computer"}'
# {"username":"paris@pony.local.thanks.computer","created":true,
#  "password":"xxxx-xxxx-xxxx-xxxx-xxxx-xxxx",
#  "addressbook":{"name":"hello","path":"/carddav/paris@pony.local.thanks.computer/addressbooks/hello/"},
#  "hello":{"uid":"hello.paris@pony.local.thanks.computer","etag":"…","noop":false},
#  "carddav":{"server":"pony.local.thanks.computer","port":8080,"tls":false,"path":"/carddav/"}}
```

The password is returned exactly once — only its hash is stored. The
route is **open on loopback** (it is a demo); the guarantee that holds
everywhere is in the op: `txco://contacts/account` runs only inside this
tenant's rules and only for a domain the tenant owns. `*.local.thanks.computer`
resolves to loopback and is auto-verified by `txco dev`, which is why the
hostname bind above is enough.

### Open it in a contacts app

Contacts (macOS): **Add Account → Other Contacts Account → CardDAV →
Advanced**, server `pony.local.thanks.computer`, port `8080`, SSL off, path
`/carddav/`, the username and the password above. "Hello contacts" appears
with one card. Thunderbird: **Address Book → New CardDAV Address Book**,
the same server and username.

Add a card with an email address. The book's policy says `stack` for
`put`, so the head asks `OPS/_contacts/0` before committing:
`answer_allow.txcl` accepts it as written — what you typed, photo and
labels included, is what every client reads back. Add a card without an
address and `answer_email.txcl` refuses with a message the client shows;
nothing is stored. Make a group: Apple sends it as a card too, and
`answer_other.txcl` lets it through. `txco trace` shows one run per edit
and none for browsing.

A client edit on an address book without a `_contacts` stack: `observe` is
silent and `stack` answers 503 — the stack is the subscription.
