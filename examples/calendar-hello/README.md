# calendar-hello — a calendar a calendar app can open

A stack that provisions a calendar account, creates a calendar with a
daily hello event, and mints an ICS feed link — so Apple Calendar,
Thunderbird, or a Google Calendar subscription can show a calendar a rule
owns. The chassis's `calendar` personality serves it over CalDAV and as an
ICS feed; the stack decides what goes in it with `txco://calendar/put`.
Nothing lands there on its own.

```
OPS/calendar-demo/
  100/provision_parse.txcl   POST /calendar/provision → parse {"username", "password"?}
  110/account.txcl           txco://calendar/account (argon2id; password generated when omitted)
  110/missing.txcl           …400 without a username
  120/calendar.txcl          txco://calendar/calendar — "hello", policy put/delete = stack, feed = ensure
  130/hello.txcl             txco://calendar/put — a daily event from event{} (the chassis renders the VEVENT)
  200/*                      JSON responses (the password and feed token appear once)
  1000/notfound.txcl         anything else → 404 (never `200 {}`)
OPS/_calendar/0/
  observe.txcl               every committed client mutation, after the reply (@calendar.phase observe)
  answer_daily.txcl          a put that does not repeat daily is refused (@calendar.phase answer)
  answer_allow.txcl          …a daily one is accepted with a REWRITE (UTC-anchored, 30 minutes)
```

Run it (the calendar head is off unless asked for):

```
txco dev --calendar                               # from this directory
txco auth tenant hostnames add pony.local.thanks.computer --stack calendar-demo
curl -X POST http://localhost:8080/calendar/provision \
  -d '{"username":"paris@pony.local.thanks.computer"}'
# {"username":"paris@pony.local.thanks.computer","created":true,
#  "password":"xxxx-xxxx-xxxx-xxxx-xxxx-xxxx",
#  "calendar":{"name":"hello","path":"/dav/paris@pony.local.thanks.computer/calendars/hello/",
#              "feed_path":"/dav/feed/<token>.ics"},
#  "hello":{"uid":"hello.paris@pony.local.thanks.computer","etag":"…","noop":false},
#  "caldav":{"server":"pony.local.thanks.computer","port":8080,"tls":false,"path":"/dav/"}}
```

The password is returned exactly once — only its hash is stored. The
route is **open on loopback** (it is a demo); the guarantee that holds
everywhere is in the op: `txco://calendar/account` runs only inside this
tenant's rules and only for a domain the tenant owns. `*.local.thanks.computer`
resolves to loopback and is auto-verified by `txco dev`, which is why the
hostname bind above is enough.

### Open it in a calendar app

Calendar (macOS): **Add Account → Other CalDAV Account → Advanced**,
server `pony.local.thanks.computer`, port `8080`, SSL off, path `/dav/`,
the username and the password above. "Hello calendar" appears with a
daily 09:00 UTC event. Thunderbird: a network calendar at
`http://pony.local.thanks.computer:8080/dav/paris@pony.local.thanks.computer/calendars/hello/`.

Drag the hello event to 10:15 and stretch it to an hour. The calendar's
policy says `stack` for `put`, so the head asks `OPS/_calendar/0` before
committing: the event repeats daily, so `answer_allow.txcl` accepts it —
and rewrites it to a 30-minute event anchored in UTC, the shape this
stack runs. The client re-fetches and shows the rewritten event.
Change the repeat to weekly and `answer_daily.txcl` refuses with a
message the client shows; nothing is stored. `txco trace` shows one run
per edit and none for browsing.

### The feed

`feed_path` is an opaque bearer URL: paste
`http://pony.local.thanks.computer:8080/dav/feed/<token>.ics` into any
subscriber. On a deployed stack that is `https://<stack host>/dav/feed/…`,
which Google Calendar ("From URL") and Outlook accept. Provision again
with the same body and the calendar keeps its token (`ensure`); a
`feed = "rotate"` in `120/calendar.txcl` would retire it.

A client edit on a calendar without a `_calendar` stack: `observe` is
silent and `stack` answers 503 — the stack is the subscription.
