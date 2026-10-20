<!-- nav: Web search -->

# web-search — ask a public search engine

_`txco://web/search` asks a public search engine one question, under your
own key, and returns titles, addresses and snippets as JSON. The engine's
raw answer never enters the document._

A flow that needs to know what the web says, a news watch, a check on a
claim, a lookup before a draft, can't search from txcl on its own. This op
does the asking, with the request built by the chassis and bounded by it.
Brave Search answers it, on the tenant's Brave subscription.

## Set the key

The op uses the tenant's own Brave Search API key, stored as the secret
`BRAVE_SEARCH_KEY`, the way `ai://chat` uses `OPENROUTER_KEY`:

```sh
txco auth tenant secrets set BRAVE_SEARCH_KEY
```

A stack-scoped value (`--stack news`) beats the tenant-wide one. Without a
key the op answers `txco_web_missing_secret`; nothing is sent. On a chassis
with `--ai-chat-env-fallback` on (the bundled default, off on a shared
fleet), an environment variable of the same name answers when the store
has nothing. See [the secret store](./runbook-secret-store.md).

Brave bills the tenant's subscription per successful request; the chassis
adds no price of its own, only [fuel](#fuel).

## Search

```txcl
WITH query     = &concat("French employment law changes ", @scheduled.payload.month),
     count     = 8,
     country   = "FR",
     language  = "fr",
     freshness = "month",
     into      = "_found"
EXEC "txco://web/search"
```

The answer lands at `into` (default `_websearch`):

```json
{
  "ok": true,
  "query": "French employment law changes October",
  "kind": "web",
  "provider": "brave",
  "results": [
    {
      "title": "Ce qui change au 1er octobre",
      "url": "https://example.fr/actualites/octobre",
      "description": "Les nouvelles règles sur…",
      "page_date": "2026-10-01T00:00:00Z",
      "source": "example.fr"
    }
  ],
  "count": 1,
  "truncated": false
}
```

- **`results`** are in the engine's order; the chassis ranks nothing. No
  result is a success with `results: []`.
- **`title`** and **`description`** are plain text: tags stripped,
  entities decoded, whitespace collapsed.
- **`url`** is as the engine gave it, checked: `http` or `https`, no
  credentials in it.
- **`page_date`** is the page's date as the engine reports it, RFC 3339 in
  UTC, or `null`. Brave calls it "published or last modified": treat it as
  the page's date, not the article's.
- **`source`** is the URL's host, lower case.
- **`truncated: true`** says a title or description was cut, or a result
  dropped for size.

A failure is an answer too, never a dropped op:

```json
{
  "ok": false,
  "query": "French employment law changes October",
  "provider": "brave",
  "error": {"code": "txco_web_rate_limited", "message": "the search provider's rate limit was reached", "status": 429, "retry_after": 1}
}
```

Branch on `._found.ok` in a later scope.

## News

`kind = "news"` asks the engine's news index instead of its web index: recent
articles, each a headline, its address and a short description. It answers
"what has this outlet published today" without fetching the outlet's pages,
which many news sites refuse to a program:

```txcl
WITH query     = "site:nytimes.com",
     kind      = "news",
     freshness = "day",
     count     = 10,
     into      = "_news"
EXEC "txco://web/search"
```

The answer has the same shape, with `"kind": "news"`. A result's
`page_date` is the article's date as the engine reports it. The other
fields work as for a web search, and the price is the same.

## The request

| Field | Required | Default | Constraints |
|---|---|---|---|
| `query` | yes | | 1 to 500 characters after trimming, at most 75 words |
| `kind` | no | `web` | `web` (pages) or `news` (articles from the news index) |
| `count` | no | 10 | 1 to 20, a whole number or one written as a string |
| `country` | no | the engine's (Brave: `US`) | two letters, or `ALL` |
| `language` | no | the engine's (Brave: `en`) | a two-letter ISO 639-1 code |
| `freshness` | no | `any` | `any`, `day`, `week`, `month` (31 days), `year` |
| `safesearch` | no | `moderate` | `off`, `moderate`, `strict` |

- **Any other key is refused** with `txco_web_invalid_request`, so a typo
  such as `freshnes` fails instead of being ignored. The chassis's own WITH
  keys (`into`, `timeout`, `mode`, `redact`, `omit`, `secrets`, …) are not
  the op's and pass.
- **An optional field set to `null` is one not given,** so a rule can
  write `count = @cap.input.count` whether or not the caller named one.
- **No engine parameters, headers, endpoint or key** can be set from a
  rule.
- **Which countries and languages exist is the engine's list.** The
  chassis maps the spellings Brave uses where they differ from ISO 639-1
  (`ja`, `zh`, `pt`); a country or language Brave doesn't serve comes back
  as `txco_web_invalid_request` with `status: 422`.
- **The defaults are American English.** A stack serving anyone else
  should pass `country` and `language`.

## How the engine is called

- **The request:** one `GET` to Brave's Web Search API (`kind` web) or News
  Search API (`kind` news) with the key in the header Brave reads it from,
  and nothing else of the tenant's: no tenant id, no address, no context.
  The query is the rule's; what a rule puts in it is the rule's choice.
- **Destinations:** the API root is the operator's (`--web-search-brave-url`),
  and the fetch goes through the same egress guard as every op dial, with
  no redirects at all: a redirect from the engine is an error, and the key
  never travels past the first hop.
- **Size and time:** at most 1 MiB of answer and 5 seconds
  (`--web-search-timeout`; your op's deadline can only shorten it). No
  retries.
- **Type:** the answer must be `2xx` and `application/json`.

## Errors

Every code is `txco_web_<code>`:

| Code | Meaning |
|---|---|
| `invalid_request` | A field is missing, out of range or unknown; or the engine refused a parameter (`status` 400 or 422). The message names it |
| `missing_secret` | `BRAVE_SEARCH_KEY` isn't set for this tenant or stack |
| `disabled` | The operator turned the op off (`--web-search=false`) |
| `rate_limited` | This tenant's per-minute limit on this node, or the engine's own limit (`status: 429`, `retry_after` in seconds) |
| `busy` | This tenant's in-flight limit, or no node slot before the deadline |
| `timeout` | The deadline passed |
| `destination_denied` · `dns_failed` · `connection_failed` | The endpoint could not be reached, as for [html-extract](./html-extract.md) |
| `provider_auth` | The engine refused the key (401 or 403) |
| `provider_error` | Any other non-2xx answer (`status`), or a redirect |
| `response_too_large` | Over 1 MiB |
| `invalid_response` | Not JSON, or not the shape the op reads |
| `no_tenant` · `internal_error` | The op ran outside a request, or failed unexpectedly |

Messages never contain the engine's body, a header, an address or the key.

## Limits

| Limit | Value |
|---|---|
| Query | 500 characters, 75 words |
| Results per call | 20 |
| One title | 1 KiB, cut at a character boundary |
| One description | 4 KiB, cut at a character boundary |
| One URL | 2 KiB; a longer one drops its result, since a cut URL points somewhere else |
| The answer | 128 KiB; past that, results drop from the end |
| Engine answer | 1 MiB |

**Per node, across tenants:**

| Flag | Default | Limit |
|---|---|---|
| `--web-search-concurrency` | 16 | Requests in flight. A call waits for a slot until its deadline. |
| `--web-search-tenant-concurrency` | 4 | Calls one tenant may have in flight. One more is `busy`. |
| `--web-search-rate-per-min` | 60 | Calls one tenant may start a minute. More are `rate_limited`. |

Brave's own limit is the tenant's plan (50 requests a second on the Search
plan). A 429 from Brave is `rate_limited` with `retry_after` read from
Brave's reset headers.

## Fuel

Each call pays:
- **the `EXEC`:** 25;
- **reading the key:** 100, as for any materialized secret;
- **the request:** 50 more.

A search is 175, whether or not the engine answers well: the chassis did
the work. Brave's own price is the tenant's subscription, not fuel. See
[fuel](./fuel.md).

## What comes back, and what you keep

- **Results are the web's words,** not yours: titles, snippets and
  addresses are external text, and snippets are a known place for prompt
  injection. Label them untrusted wherever a model reads them. The op
  writes only to `into`, never to `_txc`.
- **Brave's terms bind the tenant.** Results may not be stored "in whole
  or in part" beyond the transient storage running the application needs,
  nor used to train or evaluate AI models. The chassis keeps no cache,
  writes nothing to kv, and its own timeline event, `web.search.completion`,
  records a short hash of the query, the status, counts and times, never
  the query or a result.
- **The trace records the step** like any other: its input, so the query,
  and its output, the results, for as long as your trace settings keep
  them. Mask the results with `WITH redact = "_websearch.results"` (see
  [trace](./trace.md)). A suspended stage stores its whole envelope and
  isn't redacted, so a stack that must not keep results past the run uses
  them before it suspends, not after.

## Known limits

- **One engine.** Brave, through the tenant's own subscription. Other
  engines, image and video search, paging past the first results and date
  ranges are not offered.
- **No spelling correction is reported.** Brave may search for a corrected
  query; the op returns the results and your query as you sent it.
- **The request leaves from the node's own address.** Brave sees the
  chassis, not the tenant.

## A news watch

Search monthly, keep the sources, and let a later step read them:

```txcl
WITH query     = &concat("French employment law changes ", @scheduled.payload.month),
     count     = 8,
     country   = "FR",
     language  = "fr",
     freshness = "month",
     into      = "_found",
     redact    = "_found.results"
EXEC "txco://web/search"
```

Pass `._found.results` to an `ai://chat` step as sources to cite, not as
text to repeat: a `url` and a `title` are the reader's way to the page,
while a `description` is a sentence the engine chose.
