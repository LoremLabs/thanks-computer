<!-- nav: Web feed -->

# web-feed — read a site's feed

_`txco://web/feed` fetches one public feed, RSS, Atom or JSON Feed, and
returns its newest items as JSON: a title, an address, a date and a few
lines each. The document itself never enters the envelope._

A feed is what a site publishes about itself. Reading it answers "what has
this site put out" without fetching the site's pages, which many refuse to
a program and which [html-read](./html-read.md) reads one at a time. A
news watch, a check on a blog, a release list, a podcast's episodes: one
call, one document, the items in it.

## Read

```txcl
WITH url   = "https://example.org/feed.xml",
     count = 10,
     since = @scheduled.payload.last_run,
     into  = "_news"
EXEC "txco://web/feed"
```

The answer lands at `into` (default `_feed`):

```json
{
  "ok": true,
  "url": "https://example.org/feed.xml",
  "final_url": "https://example.org/feed.xml",
  "status": 200,
  "format": "rss",
  "feed": {
    "title": "Example",
    "link": "https://example.org/",
    "description": "News from Example",
    "updated": "2026-10-20T09:30:00Z"
  },
  "items": [
    {
      "id": "https://example.org/posts/first",
      "title": "First post",
      "url": "https://example.org/posts/first",
      "date": "2026-10-19T13:00:00Z",
      "summary": "A few lines from the post, as plain text.",
      "author": "Ada",
      "source": "example.org"
    }
  ],
  "count": 1,
  "total": 24,
  "truncated": false
}
```

- **`format`** is `rss` (RSS 2.0, 0.9x and RSS 1.0), `atom` or `json`
  (JSON Feed).
- **`feed`** is what the document says about itself. `updated` is its own
  date, RFC 3339 in UTC, or `null`.
- **`items`** are the newest first when every item is dated; otherwise
  they stand in the document's order. An item with neither a title nor an
  address is left out.
- **`title`**, **`summary`** and **`author`** are plain text: HTML read for
  its words, tags gone, entities decoded, whitespace collapsed. `summary`
  is the item's summary or description, else its content, cut at 4 KiB.
- **`url`** is the item's page, resolved against the feed's address,
  `http` or `https`, no credentials; `""` when the item has none.
  **`source`** is its host, lower case.
- **`date`** is the item's published date, else its updated date, RFC 3339
  in UTC, or `null` when the feed writes one this op doesn't read.
- **`id`** is the feed's own identifier for the item (`guid`, Atom `id`,
  JSON Feed `id`), `""` when it has none. Use it to tell a new item from
  one seen before.
- **`count`** is how many came back; **`total`** how many the document
  holds (at most 500 are read), before `since` and `count`.
- **`truncated: true`** says something was cut or left out for size: a
  field at its cap, an item dropped from the end, a document with more
  than 500 items, or one that broke off after some items.

No item is a success with `items: []`. A failure is an answer too, never a
dropped op:

```json
{
  "ok": false,
  "url": "https://example.org/",
  "final_url": "https://example.org/",
  "content_type": "text/html; charset=utf-8",
  "error": {
    "code": "txco_feed_not_a_feed",
    "message": "the address is an HTML page, not a feed; the page advertises 1 feed (`feeds`): read that address instead",
    "feeds": ["https://example.org/feed.xml"]
  }
}
```

Branch on `._news.ok` in a later scope. A page that is not a feed usually
says where its feed is: `error.feeds` lists the addresses it advertises,
first the one it names first, so a second call can read it.

## The request

| Field | Required | Default | Constraints |
|---|---|---|---|
| `url` | yes | | `http` or `https`, on port 80 or 443, no credentials, at most 2 KiB. Checked the way [html-extract](./html-extract.md#the-url) checks its URL |
| `count` | no | 20 | 1 to 50, a whole number or one written as a string: the most items to return |
| `since` | no | | An RFC 3339 instant, or a date (`YYYY-MM-DD`, midnight UTC). Items dated before it are left out; undated items stay, since nothing says they are old |

- **Any other key is refused** with `txco_feed_invalid_request`, so a typo
  such as `sinse` fails instead of being ignored. The chassis's own WITH
  keys (`into`, `timeout`, `mode`, `redact`, `omit`, `secrets`, …) are not
  the op's and pass.
- **An optional field set to `null` is one not given,** so a rule can
  write `since = @cap.input.since` whether or not the caller named one.

## How the feed is fetched and read

- **The fetch** is the same safe fetcher as html-extract: one `GET`, the
  host resolved here and every address checked by the egress guard before
  it is dialed, up to three redirects checked the same way, at most
  `--web-feed-max-bytes` (3 MiB) and `--web-feed-timeout` (5 seconds; your
  op's deadline can only shorten it). No cookies, no credentials, no
  proxy. The `User-Agent` is `txco-feed/<version>`.
- **Any media type is accepted.** Feeds are served as
  `application/rss+xml`, `atom+xml`, `xml`, `text/xml`, `application/json`,
  `feed+json`, and often `text/html` or `text/plain` by mistake, so the
  op reads the bytes for what they are: XML with an `rss`, `feed` or
  `rdf:RDF` root, or a JSON object with `items`. An HTML page is
  `not_a_feed` with the feeds it advertises in its `<head>`; anything
  else is `not_a_feed` too.
- **The read is lenient** where feeds are sloppy: unescaped ampersands,
  HTML entities in XML, a declared or undeclared Latin-1 encoding, HTML in
  titles, dates in a dozen shapes. A document that breaks off after some
  items answers with those items and `truncated: true`; one that breaks
  before any is `invalid_feed`.
- **The read is bounded** in elements, nesting and text per element, and
  reads at most 500 items; a document past those bounds is cut, not
  refused.

## Errors

Every code is `txco_feed_<code>`:

| Code | Meaning |
|---|---|
| `invalid_request` | A field is missing, out of range or unknown. The message names it |
| `invalid_url` | The URL is not `http(s)` on 80/443 with a host and no credentials |
| `disabled` | The operator turned the op off (`--web-feed=false`) |
| `rate_limited` | This tenant's per-minute limit on this node |
| `busy` | This tenant's in-flight limit, or no node slot before the deadline |
| `timeout` | The deadline passed |
| `destination_denied` · `dns_failed` · `connection_failed` · `redirect_limit` | The fetch could not complete, as for [html-extract](./html-extract.md#errors) |
| `http_error` | The server answered outside 2xx (`status`) |
| `response_too_large` | Over `--web-feed-max-bytes` |
| `not_a_feed` | The document is an HTML page (`feeds` lists the feeds it advertises), or neither a feed's XML nor a JSON Feed |
| `invalid_feed` | XML or JSON that does not parse, broken before any item could be read, or nested too deeply |
| `no_tenant` · `internal_error` | The op ran outside a request, or failed unexpectedly |

Messages never contain the document, a header or an address.

## Limits

| Limit | Value |
|---|---|
| Items returned | 50 |
| Items read from one document | 500 |
| One title | 1 KiB, cut at a character boundary |
| One summary | 4 KiB, cut at a character boundary |
| One author | 256 bytes |
| One id | 1 KiB; a longer one is dropped from its item |
| One URL | 2 KiB; a longer one is dropped from its item, since a cut URL points somewhere else |
| The answer | 256 KiB; past that, items drop from the end |
| The document | `--web-feed-max-bytes`, 3 MiB |

**Per node, across tenants:**

| Flag | Default | Limit |
|---|---|---|
| `--web-feed-concurrency` | 16 | Fetches in flight. A call waits for a slot until its deadline. |
| `--web-feed-tenant-concurrency` | 4 | Calls one tenant may have in flight. One more is `busy`. |
| `--web-feed-rate-per-min` | 60 | Calls one tenant may start a minute. More are `rate_limited`. |

## Fuel

As html-extract: the `EXEC` (25), the fetch (50), 100 per started MiB
downloaded, and 10 per started millisecond of the read. A typical feed of
a hundred KiB that reads in a few milliseconds is about 200. See
[fuel](./fuel.md).

## What comes back, and what you keep

- **Items are the site's words,** not yours: titles and summaries are
  external text and a known place for prompt injection. Label them
  untrusted wherever a model reads them. The op writes only to `into`,
  never to `_txc`.
- **The trace records the step** like any other: its input, so the URL,
  and its output, the items, for as long as your trace settings keep them.
  Mask the items with `WITH redact = "_feed.items"` (see
  [trace](./trace.md)). The timeline event, `web.feed.completion`, carries
  the host, a short hash of the URL, the status, counts and times, never
  the path, the query or an item.
- **The chassis keeps no copy.** Each call fetches the document again;
  conditional requests (`ETag`, `If-Modified-Since`) are not sent. A watch
  that runs often should keep the last `id` or `date` it saw and pass
  `since`.

## Known limits

- **One document, as published.** Paged feeds (RFC 5005), feeds that need
  a login, and podcasts' enclosures (the audio file's address) are not
  read; an enclosure-only entry comes back with `url: ""`.
- **No discovery fetch.** A page that advertises a feed is reported, not
  followed: a second call reads the feed.
- **Dates the reader doesn't know** come back `null` and such items sort
  with the undated.

## A watch

Read a feed from a schedule, keep what is new, and let a later step
summarize it:

```txcl
WITH url    = "https://example.org/feed.xml",
     since  = @scheduled.payload.last_run,
     count  = 20,
     into   = "_news",
     redact = "_news.items"
EXEC "txco://web/feed"
```

Pass `._news.items` to an `ai://chat` step as sources to cite, not as text
to repeat: a `url` and a `title` are the reader's way to the page, while a
`summary` is a few lines the site chose.
