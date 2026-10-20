<!-- nav: HTML read -->

# html-read — a public web page as Markdown, a part at a time

_`txco://html/read` fetches one public HTML page and returns its text as
Markdown: headings, paragraphs, lists, links, tables. A long page comes in
parts of a few thousand characters, so a flow reads it in turns. It is
[html-extract](./html-extract.md)'s sibling: the same fetch and the same
bounds, the page's words instead of values picked out of it._

Use `html/extract` when you know what you want from the page (a title, a
price, the links); use `html/read` when you want to read it.

## Read a page

```txcl
WITH url = ._link, into = "_page"
EXEC "txco://html/read"
```

The answer lands at `into` (default `_page`):

```json
{
  "ok": true,
  "url": "https://example.com/article",
  "final_url": "https://example.com/article",
  "status": 200,
  "content_type": "text/html; charset=utf-8",
  "title": "Example Article",
  "root": "main",
  "part": 1,
  "parts": 3,
  "chars": 2960,
  "total_chars": 8140,
  "text": "# Example Article\n\nThe first paragraph…"
}
```

- **`text`** is the part asked for, Markdown.
- **`part`**, **`parts`** say which part this is of how many; ask for the
  next with `WITH part = 2`, or the end with `part = "last"`.
- **`root`** says what was read: `main`, `article`, `body`, or the
  selector you named.
- **`title`** is the page's `<title>`, whitespace collapsed.
- **`chars`**, **`total_chars`** count characters in the part and in the
  whole text.
- **`truncated: true`** appears when the rendering stopped at a cap
  ([limits](#limits)); the parts are of what was rendered.
- A page with no words answers `ok: true` with `parts: 0`, `part: 0` and
  an empty `text`, whatever part was asked for.

A failure is an answer too, never a dropped op:

```json
{
  "ok": false,
  "url": "https://example.com/article",
  "parts": 3,
  "error": {"code": "txco_html_no_such_part", "message": "the page has 3 parts; there is no part 7"}
}
```

Branch on `._page.ok` in a later scope.

## What you can ask for

| `WITH` | Default | Meaning |
|---|---|---|
| `url` | required | The page, `http` or `https`. |
| `root` | — | A CSS selector naming the element to read: the first match in document order. Without one the op reads `<main>`, else the first `<article>`, else `<body>`. The selector grammar is [html-extract's](./html-extract.md#selectors). A selector that matches nothing is `root_not_found`. |
| `part` | `1` | Which part of the text, from 1; or `"last"`. |
| `part_chars` | `3000` | The most characters in one part, from 200 to 20000. The text is cut at line ends; a line longer than a part is cut where it must be. |
| `into` | `_page` | Where the answer lands. |

## What the Markdown keeps

| In the page | In the text |
|---|---|
| `h1`…`h6` | `#`…`######` |
| `p`, `div`, `section` and the other blocks | paragraphs, one blank line between |
| `ul`, `ol` (with `start`) | `- item`, `1. item`; nested lists indented two spaces |
| `a href` | `[words](address)`, the address resolved against the page's (its `<base href>` first); `http`, `https` and `mailto` only. An anchor (`#…`) or a `javascript:` link keeps its words alone. |
| `strong`, `b` · `em`, `i` | `**words**` · `_words_` |
| `code` · `pre` | `` `code` `` · a fenced block, its text as written |
| `blockquote` | lines starting `> ` |
| `table` | one row a line, `\| a \| b \|`, a `\|---\|` rule after the first row; a cell's content on one line |
| `dl` | the term on a line, its meaning after `: ` |
| `hr` · `br` | `---` · a line break |
| `img` | its `alt` text |

Left out: `script`, `style`, `template`, `noscript`, the `<head>`, SVG
and MathML, frames, embedded media, form controls, anything marked
`hidden` or `aria-hidden="true"`, and navigation wherever it sits: `nav`,
`aside` and elements with the `navigation` or `complementary` role (a
site's tool menus sit inside `<main>` as often as beside it). When the
page marks no main content (no `<main>`, no `<article>`) and the whole
`<body>` is read, its `header` and `footer` (and the `banner` and
`contentinfo` roles) are left out too, as page furniture; name a `root`
to read any of them.

Whitespace is collapsed as a browser collapses it. The text is what a
reader sees, so a page that builds itself in the browser has nothing to
read, as for `html/extract`.

## How the page is fetched

Exactly as [html-extract](./html-extract.md#how-the-page-is-fetched)
fetches it: one `GET` of an http(s) URL on port 80 or 443, no credentials,
no cookies, no headers of yours; every address checked by the egress
policy before it is dialed, redirects included (up to 3); at most 3 MiB
and 5 seconds; `text/html` only. The User-Agent is
`txco-read/<version> (+https://www.thanks.computer)`.

## Errors

Every code is `txco_html_<code>`. The fetch and document errors are
[html-extract's](./html-extract.md#errors); the op adds:

| Code | Meaning |
|---|---|
| `invalid_request` | `root` is not a string, `part` is not a whole number from 1 or `"last"`, or `part_chars` is outside 200–20000 |
| `invalid_selectors` | The `root` selector does not parse or uses something the grammar refuses (the message says `root`) |
| `root_not_found` | The `root` selector matched nothing |
| `no_such_part` | The part asked for is past the last; `parts` in the answer says how many there are |

## Limits

The document limits are html-extract's (tokens, nodes, nesting,
attributes, a second of CPU), plus:

| Limit | Value |
|---|---|
| Text rendered from one page | 1 MiB, then `truncated: true` |
| Nodes walked rendering it | 1,000,000, then `truncated: true` |
| One part | 200–20000 characters (`part_chars`) |

**Per node, across tenants:** the `--html-extract-*` flags bound this op
and `html/extract` together: one pool of fetch slots and parse slots, and
**one tenant allowance** of calls in flight and calls a minute shared by
both ops (see [flags](./flags.md)).

## Fuel

As html-extract: the `EXEC` (25), the fetch (50), 100 per started MiB
downloaded, and 10 per started millisecond of parsing and rendering. See
[fuel](./fuel.md).

## What comes back, and what doesn't

- **The text is the page's words,** not yours: treat it as untrusted. A
  page can say anything, including instructions; a flow that hands the
  text to a model should fence it as data.
- **The URL can carry data,** as with `EXEC "https://…"` and
  `html/extract`. Which URLs a stack may read is the stack's decision.
- **The trace records the answer,** the text included, under the usual
  redaction (`WITH redact`, see [trace](./trace.md)). The op's own
  timeline event, `html.read.completion`, records the host, a short hash
  of the URL, the status, sizes, times, the root and which part of how
  many — never the text.
