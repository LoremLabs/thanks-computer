<!-- nav: Calendar -->

# Calendar — CalDAV and ICS feeds as the UI

_The `calendar` personality serves a durable calendar store to any CalDAV
client — Apple Calendar, iOS, Thunderbird — and as an ICS feed anyone can
subscribe to (Google Calendar, Outlook). Stacks put events into it with an
op; the client reads them. The head is a generic calendar server: it knows
no product and never runs a stack to answer a read._

Nothing lands in a calendar on its own. A rule that wants an event to show
up in a client calls `txco://calendar/put` with the **event** it chose to
publish — a structured object the chassis renders to iCalendar, or the
iCalendar text itself — and the head serves it from the store. The wire
form is derived; the event is what is retained.

The other direction is a stack **hearing** what the client did: every
committed mutation (an event dragged to a new hour, a new event, a delete)
can reach the tenant's `_calendar` stack after the fact, and a calendar can
be set to ask the stack first — and to accept the stack's rewrite of the
client's event. That is how "drag the pony's schedule and the pony
reschedules" is written in txcl, not in Go.

## Turn it on

```
txco serve --personalities cron,web,admin,calendar
```

There is no listener of its own: the head mounts on the **web head** under
a reserved path prefix on every hostname it serves —
`--calendar-path-prefix` (default `/dav`) plus `/.well-known/caldav`, which
redirects into it. TLS is the web head's (a front proxy, or
`--web-tls-addr`). The index lives in its own SQLite file
(`--calendar-db-path`, default `./chassis/data/calendar.db`); objects are
small text and live in the index, never the blob CAS.

On a fleet, `--calendar-store` selects a backend an overlay registers (the
hosted build ships `postgres`, reading `TXCO_DB_AUTH_DSN`). A shared
backend is opened on **every** node, head or not, so `txco://calendar/*` on
any node project into the one index the head serves; a node whose open
failed at boot answers `txco_calendar_disabled` until restart.

For local development:

```
txco dev --calendar       # http://<dev host>:<web port>/dav/, Basic auth over plaintext
```

Add the account in Calendar (macOS) with **Other CalDAV Account →
Advanced**: server = the bound host, port = the web port, SSL off, path
`/dav/`; in Thunderbird, a network calendar at
`http://<host>:<port>/dav/<username>/calendars/<name>/`. Every Basic-auth
attempt logs one `calendar login` line with its outcome.

## Accounts: `txco://calendar/account`

```txcl
EXEC "txco://calendar/account"
  WITH username = "paris@pony.example.com",   # <local>@<domain the tenant owns>
       password = ._imapacct.password,        # the same password the IMAP account got
       into = "_calacct"
```

| WITH | Meaning |
|---|---|
| `username` (req) | `<local>@<domain>`. The domain must be a verified hostname binding or a delegated DNS zone of the tenant — the ownership rule `txco://sendmail` and `txco://imap/account` apply. Usernames are global: one tenant per address. |
| `password` | Omitted: unchanged on update, generated on create. `""`: generated. Otherwise stored (≥ 8 chars). Only an argon2id hash is kept. Pass the password another head minted and one credential opens both. |
| `rotate` | `true`: generate a new password for an existing account and return it once. |
| `password_style` / `password_words` | As [IMAP](./imap.md): `token` (default) or `words` (4–12 BIP-39 words, default 5). |
| `status` | `active` (default) or `disabled`. |
| `policy` | Account-default mutation policy (below), the fallback for every calendar. |

Result at `into` (default `_calendar`): `{username, created, principal,
password?, rotated?}` — `password` appears **only** when it was generated,
and only this once. `principal` is the account's CalDAV path.

## Calendars: `txco://calendar/calendar`

```txcl
EXEC "txco://calendar/calendar"
  WITH username     = "paris@pony.example.com",
       name         = "schedule",
       display_name = "Paris schedule",
       timezone     = "Europe/Paris",
       policy       = &object("put", "stack", "delete", "stack"),
       feed         = "ensure",
       into         = "_sched"
```

| WITH | Meaning |
|---|---|
| `username`, `name` (req) | The account and the calendar's path segment (`[A-Za-z0-9._~-]`, up to 128 chars). Creates when absent, updates otherwise; empty fields are left alone. |
| `display_name`, `description`, `color` (`#rrggbb`), `sort_order`, `timezone` (IANA) | What the client shows. |
| `policy` | Per-calendar mutation policy (below). |
| `feed` | `ensure` (mint a feed token if the calendar has none), `rotate` (mint a new one, retiring the old), `disable`. |
| `remove` | `true`: soft-delete the calendar and tombstone its objects. |

Result: `{id, name, path, display_name, description, color, timezone,
policy, feed, sync_token, created, feed_token?, feed_path?, removed?}`.
`feed_token` appears **only** when minted, and only this once; only its
hash is stored. `feed_path` is `<prefix>/feed/<token>.ics` — prepend the
stack's own `https://<host>` and that URL is the subscription.

## Objects: `txco://calendar/put`

```txcl
EXEC "txco://calendar/put"
  WITH username = "paris@pony.example.com",
       calendar = "schedule",
       name     = "daily-digest.ics",         # the resource name on create
       event    = &object(
         "summary",     "Daily digest",
         "description", .question,
         "start",       "2026-01-01T09:00:00Z",
         "duration",    "PT30M",
         "rrule",       "FREQ=DAILY"),
       into     = "_put"
```

| WITH | Meaning |
|---|---|
| `username`, `calendar` | The account and the calendar (its `name`). |
| `event{}` **or** `ical` | The structured event (the chassis renders a canonical VEVENT) or iCalendar text (the UID is the bytes' own). Not both. |
| `uid` | The object's identity. Optional with `event{}`: derived from `name` as `<name>.<local>@<domain>`, stable across re-materializations. |
| `name` | Resource name on create; on update the object keeps the name it has, whatever the client chose. |

`event{}` is generic iCalendar vocabulary: `uid`, `summary`, `description`,
`location`, `status` (CONFIRMED / TENTATIVE / CANCELLED), `url`, `start`,
`end` or `duration` (default PT1H), `tzid`, `rrule` (an RFC 5545 RECUR
value), `exdate[]`. `start`/`end` are `YYYY-MM-DD` (all-day), RFC3339 (with
`Z` or an offset, stored as UTC), or a local `YYYY-MM-DDTHH:MM:SS` **with**
`tzid` — a VTIMEZONE is emitted from the chassis's own zone data, so
"08:00 Paris every weekday" survives daylight saving. Floating times are
refused.

Result: `{name, path, uid, etag, created, noop, sequence, modseq}`. The
same content again (DTSTAMP and SEQUENCE aside) is a `noop` — an hourly
re-materialization never changes an etag; changed content gets a higher
SEQUENCE. The op charges [fuel](../fuel.md) per MiB like `blob/put`.
Errors land as `<into>.error.{code, message}` with the run continuing:
`txco_calendar_disabled`, `txco_calendar_no_account`,
`txco_calendar_no_calendar`, `txco_calendar_no_object`,
`txco_calendar_domain_not_owned`, `txco_calendar_username_taken`,
`txco_calendar_invalid_arg`, `txco_calendar_too_large`,
`txco_calendar_conflict` (the UID names another resource).

## The rest of the op family

| Op | WITH | Returns |
|---|---|---|
| `txco://calendar/get` | `username`, `calendar`, `uid` or `name` | `{name, path, uid, etag, component, size, summary, start_utc, end_utc, recurs, sequence, modseq, updated_at, ical, event{}}` — `event` is the parse: every input field plus `all_day`, `start_utc`, `end_utc`, `recur{freq, interval, byday[], …}`, `sequence`, `dtstamp` |
| `txco://calendar/list` | `username` | `{calendars:[{…, objects}], count, home}` |
| `txco://calendar/list` | `username`, `calendar`, `after` (modseq cursor), `limit` (≤ 1000) | `{items:[{name, path, uid, etag, summary, start_utc, recurs, modseq, …}], count, next, sync_token}` |
| `txco://calendar/delete` | `username`, `calendar`, `uid` or `name` | `{deleted, name, uid}` |

## What a client gets

Discovery through `/.well-known/caldav` → `<prefix>/` (current-user-
principal) → `<prefix>/<username>/` (calendar-home-set) →
`<prefix>/<username>/calendars/` (the calendars) — so a client needs the
server, the username and the password. Then `PROPFIND`, `REPORT`
(`calendar-query` with time ranges — recurrences are expanded — and
`calendar-multiget`), `GET`, `PUT` (with `If-Match` / `If-None-Match`),
`DELETE`, `MKCALENDAR`, `PROPPATCH` (display name, description, colour,
order). Objects keep stable URLs, UIDs and ETags across
re-materialization. Not yet: `sync-collection` / `getctag` (clients fall
back to an etag diff per refresh, which works), `RECURRENCE-ID` overrides
in time-range matching, VTODO in feeds, invitations, free/busy.

Basic auth on every request: the head verifies argon2id once and caches
the verified triple for five minutes, so a client's dozens of requests per
refresh cost one hash. A request must arrive over TLS — the web head's own
listener, or `X-Forwarded-Proto: https` from the front proxy — unless
`--calendar-insecure-auth` (dev). Throttles count cache misses only
(`--calendar-login-rate`, per client IP and per username). A `disabled`
account and a suspended tenant are refused; an account on another tenant's
hostname is refused exactly like a wrong password.

The **ICS feed** `GET <prefix>/feed/<token>.ics` needs no auth — the token
is the secret (24 random bytes, hashed at rest, shown once by the op). It
renders every live object with an ETag and `Cache-Control: private,
max-age=<--calendar-feed-max-age>`; `If-None-Match` answers 304. Feeds are
off until a stack asks for one.

## Policy: what a stack hears, and when

Per calendar, five verbs — `put`, `delete`, `mkcalendar` (a client creates
a calendar), `remove` (a client deletes one), `proppatch` — each one of:

| Mode | Effect |
|---|---|
| `deny` | `403` at the protocol layer, no round trip (the default for `mkcalendar` and `remove`) |
| `local` | protocol state only, no event (the default for `proppatch`) |
| `observe` | commit, then tell the `_calendar` stack, fire-and-forget (the default for `put` and `delete`) |
| `stack` | ask the `_calendar` stack **first**; `403` unless it answers `@calendar.res.ok = true` |

Resolution: the calendar's `policy`, then the account's `policy`, then the
chassis default. The `_calendar` stack is the subscription — while it
exists, events flow; without it `observe` is silent and `stack` answers
`503`.

### The envelope

```
@src                "calendar"                          @client.ip
@calendar.tenant    (slug)         @calendar.account    (username)
@calendar.phase     observe | answer                    @calendar.op   put | delete | mkcalendar | remove | proppatch
@calendar.calendar  {id, name, display_name, timezone}
@calendar.object    {name, uid, etag, prior_etag, component, size, exists}   (put / delete)
@calendar.ical      the client's object, canonical iCalendar text            (put)
@calendar.event     {…the parse of it: uid, summary, start, tzid, start_utc, recur{…}, …}
@calendar.prior     {event: {…the stored object's parse…}}                   (put on an existing object, delete)
@calendar.props     {displayname, description, color, order, timezone}      (mkcalendar / proppatch)
```

The object's bytes ride the envelope (they are small); a rule may `EMIT
@delete = &array("@calendar.ical", "@calendar.event", "@calendar.prior")`
once it has consumed them.

### Answering (`@calendar.phase == "answer"`)

```txcl
WHEN @calendar.phase == "answer" && @calendar.event.recur.freq != "DAILY"
  EMIT @calendar.res.ok = false, @calendar.res.code = "cannot",
       @calendar.res.msg = "this schedule runs daily; weekly rules come later"

WHEN @calendar.phase == "answer" && @calendar.op == "put"
  EMIT @calendar.res.ok = true,
       @calendar.res.event = &object("summary", @calendar.event.summary,
                                     "start", "2026-01-01T09:00:00Z",
                                     "duration", "PT30M", "rrule", "FREQ=DAILY")
```

`ok` absent or false is a `403`; `code` is `cannot` (403), `limit` (507) or
`unavailable` (503); `msg` is shown to the client. On `ok`, an optional
`@calendar.res.event` (or `.ical`) is the object the head **commits
instead of the client's bytes** — the client's UID is kept, so its
resource keeps its identity — which is how a stack normalizes an edit to
what it can actually run. The head waits `--calendar-resp-timeout` (30 s);
past it the client gets `503` and a late answer is discarded.

Observe-lane runs are sampled and bounded (`--calendar-observe-sample`,
`--calendar-observe-max-inflight`); a full queue drops the observation
rather than delay a client. Both lanes meter fuel like any run.

## Client settings

The head never routes by name: it serves every hostname the web head
does, so **the server a calendar client should use is the domain of the
address** — `paris@<stack>.stacks.example` connects to
`https://<stack>.stacks.example/dav/`, and `/.well-known/caldav` on that
host does the rest. For clients that discover a server from an address by
DNS (RFC 6764), the `dns` personality can publish `_caldavs._tcp` SRV +
TXT records (`--dns-caldavs-port`, see [dns](./dns.md)).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--calendar-path-prefix` | `/dav` | The reserved prefix on every hostname (plus `/.well-known/caldav`) |
| `--calendar-store` / `--calendar-db-path` | `sqlite` / `./chassis/data/calendar.db` | The index; a non-sqlite backend is shared and opened on every node |
| `--calendar-insecure-auth` | `false` | Accept Basic auth without TLS (`txco dev --calendar` sets it) |
| `--calendar-login-rate` | `30` | Verifications per minute, per IP and per username, on cache misses only |
| `--calendar-object-max-bytes` | 1 MiB | Size cap for an object (ops and client PUT; advertised as `max-resource-size`) |
| `--calendar-feed-max-age` | `300` | `Cache-Control` max-age on feeds |
| `--calendar-resp-timeout` | `30s` | Answer-lane deadline |
| `--calendar-observe-sample` / `--calendar-observe-max-inflight` | `1` / `8` | Observe-lane sampling and concurrency |

Env: `TXCO_CALENDAR_*`. Example: `examples/calendar-hello`.
