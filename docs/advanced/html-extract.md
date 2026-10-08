<!-- nav: HTML extract -->

# html-extract — values from a public web page

_`txco://html/extract` fetches one public HTML page and returns the values
CSS selectors pick out of it, as JSON. The page itself never enters the
document: a title, a description, an image address, a list of links, no
more._

A web page can't be merged into a flow: an `EXEC "https://…"` answer must
be JSON. This op does the reading for you, with the fetch and the parse both
bounded by the chassis.

## Read a page

```txcl
WITH url = ._link,
     selectors = &object(
       "title",       &object("selector", "title", "text", true),
       "description", &object("selector", "meta[name='description']", "attr", "content"),
       "image",       &object("selector", "meta[property='og:image']", "attr", "content"),
       "headings",    &object("selector", "h1, h2", "text", true, "all", true, "limit", 20)),
     into = "_page"
EXEC "txco://html/extract"
```

The answer lands at `into` (default `_html`):

```json
{
  "ok": true,
  "url": "https://example.com/article",
  "final_url": "https://example.com/article",
  "status": 200,
  "content_type": "text/html; charset=utf-8",
  "data": {
    "title": "Example Article",
    "description": "An interesting article",
    "image": "/images/cover.jpg",
    "headings": ["Introduction", "Background", "Conclusion"]
  }
}
```

- **`data`** uses your field names, in the order you named them.
- **`final_url`** is where any redirects ended.
- **`content_type`** is the header as the server sent it.
- **`truncated: true`** appears when a value was cut to 4 KiB.

A failure is an answer too, never a dropped op:

```json
{
  "ok": false,
  "url": "https://example.com/gone",
  "content_type": "text/html",
  "error": {"code": "txco_html_http_error", "message": "the server answered 404", "status": 404}
}
```

`content_type` is there whenever a response arrived. Branch on `._page.ok`
in a later scope.

## Selectors

`selectors` is an object of named fields. Each field is
`{selector, text: true | attr, all?, limit?}`:

| Key | Default | Meaning |
|---|---|---|
| `selector` | required | A CSS selector, at most 512 bytes. |
| `text` | `false` | The element's text, with whitespace collapsed. Script, style and template content is left out. |
| `attr` | — | One attribute's value, exactly as written in the page. |
| `all` | `false` | Every match, as an array in document order. Without it, the first match. |
| `limit` | `10` | The most matches `all` returns, from 1 to 50. |

Set exactly one of `text: true` and `attr`. You can name at most 20 fields.
A field name is letters, digits, `_` and `-`, starting with a letter or
`_`.

When nothing matches:
- a single field is `null`;
- an `all` field is `[]`.

An element that matches but lacks the `attr` is skipped, so the first
element that has it answers.

Attribute values are returned as written. A relative `href` or `src` stays
relative: resolve it yourself against `final_url`, or against the page's
`<base href>`, which you can also extract.

**What a selector may use:**
- type, `#id`, `.class` and attribute selectors (`[a]`, `[a=v]`, `~=`,
  `|=`, `^=`, `$=`, `*=`, `!=`, and the `i` flag);
- `*`;
- the `>` and `+` combinators;
- **one** descendant space per selector;
- `:not(…)`, `:first-child`, `:last-child` and `:root`;
- comma-separated alternatives.

**Refused** with `txco_html_invalid_selectors`:
- `:has()`, `:contains()` and `:matches()`;
- `:nth-*` and `:*-of-type`;
- the `~` combinator;
- a second descendant space (write `main > article h2`, not
  `body main article h2`);
- `[a#=regex]`.

Each of these costs more than a fixed amount per element on a hostile page,
and the chassis can't stop a match once it has started. An invalid selector
is an error, never an empty result.

## How the page is fetched

The fetch is narrow on purpose:
- **The request:** one `GET` of an `http` or `https` URL, on port 80 or
  443, with no credentials in it. It carries no cookies, no authentication,
  no request body and no headers of yours. The User-Agent is
  `txco-extract/<version> (+https://www.thanks.computer)`.
- **Destinations:** the host is resolved by the chassis, and every address
  is checked by the egress policy (`--egress-policy`; `private` on a fleet)
  before it is dialed. That rules out loopback, private networks,
  link-local and cloud-metadata addresses. The address checked is the
  address connected, so a DNS answer that changes between lookups can't
  slip a private address in.
- **Redirects:** up to 3, each checked the same way, scheme and port
  included.
- **Size and time:** at most 3 MiB of body (`--html-extract-max-bytes`) and
  5 seconds for the whole fetch (`--html-extract-timeout`; your op's
  deadline can only shorten it). A larger body fails; nothing is extracted
  from a partial page.
- **Type:** the response must be `2xx`, and its media type `text/html`. The
  op asks for an uncompressed body; a server that sends gzip anyway is
  decoded, within the same 3 MiB.
- **Encoding:** the charset comes from the header, else from the page's
  `<meta charset>`, else UTF-8. The values come back as UTF-8.

## Errors

Every code is `txco_html_<code>`:

| Code | Meaning |
|---|---|
| `invalid_url` | Not http(s), credentials in it, a port other than 80/443, no host; or a redirect to such a URL |
| `invalid_selectors` | A field is malformed, or its selector does not parse or uses something refused (the message names the field) |
| `destination_denied` | The host resolves only to addresses the egress policy refuses |
| `dns_failed` | The host does not resolve |
| `connection_failed` | The connection or TLS handshake failed |
| `timeout` | The fetch or the extraction ran out of time |
| `redirect_limit` | More than 3 redirects |
| `http_error` | A non-2xx answer; `error.status` holds it |
| `unsupported_content_type` | Not `text/html`, or no Content-Type |
| `unsupported_encoding` | A Content-Encoding other than gzip |
| `response_too_large` | The body is over the byte cap |
| `document_too_complex` | The page exceeds a document limit below |
| `parse_failed` | The page could not be decoded or parsed |
| `output_limit_exceeded` | The values together are over 64 KiB |
| `rate_limited` · `busy` | This tenant's limits on this node, below |
| `no_tenant` · `internal_error` | The op ran outside a request, or failed unexpectedly |

Messages never contain an address or a connection detail.

## Limits

| Limit | Value |
|---|---|
| Fields | 20 |
| Selector length | 512 bytes |
| Matches per `all` field | 50 |
| One value | 4 KiB, cut at a character boundary, with `truncated: true` |
| All values together | 64 KiB, else `output_limit_exceeded` |
| Page body | 3 MiB |
| Tokens in the page | 600,000 |
| Nodes in the parsed page | 400,000 |
| Nesting | 512 |
| Attributes on one element | 256 |
| Parsing and matching | 1 second of CPU |

The document limits fit a dense 3 MiB page of ordinary markup: about 530k
tokens and 380k nodes, about 150 ms and 70 MB on a laptop. A page built to
be expensive is refused before it costs that much.

One check applies before the page is parsed. The HTML parsing algorithm
recreates every unclosed `<b>`, `<i>` and `<font>` in each new block, so a
few kilobytes can grow into millions of nodes. The op estimates the nodes
the parser will create, and refuses a page whose estimate is too high.

**Per node, across tenants:**

| Flag | Default | Limit |
|---|---|---|
| `--html-extract-concurrency` | 16 | Fetches in flight. A call waits for a slot until its deadline. |
| `--html-extract-parse-concurrency` | 2 | Parses at once. Each can hold about 70 MB and a second of CPU. |
| `--html-extract-tenant-concurrency` | 4 | Calls one tenant may have in flight. One more is `busy`. |
| `--html-extract-rate-per-min` | 60 | Calls one tenant may start a minute. More are `rate_limited`. |

## Fuel

Each call pays:
- **the `EXEC`:** 25;
- **the fetch:** 50 more;
- **bytes downloaded:** 100 per started MiB;
- **parsing and matching:** 10 per started millisecond, the nano-op compute
  rate.

A 100 KB page that parses in 5 ms costs about 235; a dense 3 MiB page,
about 2,000. See [fuel](./fuel.md).

## What comes back, and what doesn't

- **The values are the page's words,** not yours: treat them as untrusted
  text. The op writes only to `into`, never to `_txc`.
- **The URL can carry data, as with `EXEC "https://…"`.** A rule that
  builds the URL from what it has read sends that to whoever runs the
  host. Which URLs a stack may read is the stack's decision, as it is for
  any outbound call.
- **The trace records the answer,** including `url` and `final_url`. If a
  URL can hold something private, mask it with `WITH redact` (see
  [trace](./trace.md)). The op's own timeline event, `html.completion`,
  records only the host, a short hash of the URL, the status, sizes and
  times.

## Known limits

- **No JavaScript.** A page that builds itself in the browser has nothing
  to read in its HTML.
- **No pretending to be a crawler.** Some sites only send their
  `og:`/`twitter:` tags to the big crawlers' user agents; for those, the
  op sees what any browser without JavaScript sees.
- **The fetch leaves from the node's own address.** A site that blocks
  hosting providers' addresses blocks it.

## A link preview

Ask for every source of a title, description and image, and pick in a
later step: `og:` first, then `twitter:`, then the plain tags. Resolve the
image against `base` or `final_url`.

```txcl
WITH url = ._link,
     selectors = &object(
       "title",          &object("selector", "title", "text", true),
       "description",    &object("selector", "meta[name='description']", "attr", "content"),
       "og_title",       &object("selector", "meta[property='og:title']", "attr", "content"),
       "og_description", &object("selector", "meta[property='og:description']", "attr", "content"),
       "og_image",       &object("selector", "meta[property='og:image']", "attr", "content"),
       "og_site_name",   &object("selector", "meta[property='og:site_name']", "attr", "content"),
       "tw_title",       &object("selector", "meta[name='twitter:title']", "attr", "content"),
       "tw_image",       &object("selector", "meta[name='twitter:image']", "attr", "content"),
       "canonical",      &object("selector", "link[rel='canonical']", "attr", "href"),
       "icon",           &object("selector", "link[rel~='icon']", "attr", "href"),
       "base",           &object("selector", "base[href]", "attr", "href")),
     into = "_page"
EXEC "txco://html/extract"
```

The order of preference and the URL resolution are a few lines in a
[nano-op](../authoring/nano-ops.md). Cache the result in
[kv](./kv.md) with a TTL if the same links come up often.
