<!-- nav: Read file -->

# read-file — load FILES/ assets into the document

_`txco://read-file` reads a stack's `FILES/` assets into the document as DATA —
so a rule can template them, hash them, return them, or hand them to another op.
It's the read-into-the-tree counterpart to [`txco://static`](./static-files.md),
which serves a file straight back as an HTTP response._

The headline use is mail — read `FILES/_mail/welcome.html` and pass it to
[`txco://sendmail`](./protocols/sendmail.md) — but it's general: config,
fixtures, prerendered fragments, anything you ship under `FILES/`.

## Read a file

```txcl
WITH files = &array(&object("path", "_mail/welcome.html", "as", "welcome"))
EXEC "txco://read-file"
```

The bytes are now at `._files.welcome.content`. A later op can use them — e.g.
reply with the template (sendmail reads the `_sendmail` contract):

```txcl
WHEN ._files.welcome.found == true
  SET ._sendmail.to      = @lmtp.mail.from,
      ._sendmail.from    = @lmtp.rcpt.0,
      ._sendmail.subject = "Welcome",
      ._sendmail.body    = ._files.welcome.content
  EXEC "txco://sendmail"
```

(Read in one scope, send in a later one — same-scope ops run in parallel, so the
file must be loaded before the op that consumes it.)

## `files` is a list of `{ path, as }`

```txcl
WITH files = &array(
  &object("path", "_mail/welcome.html", "as", "welcome"),
  &object("path", "config/limits.json", "as", "limits"))
EXEC "txco://read-file"
```

- **path** — a `FILES/`-relative path within the routed stack. `_`-prefixed
  paths work (that's how `_mail/` templates are read — they're indexed but never
  served over HTTP).
- **as** — the key the result lands under. Required, unique, no `.` or `/`.
  Results are keyed by your **alias**, not the file path, so downstream
  addressing stays clean and decoupled from the on-disk layout.

## The result shape

Each file lands under `into` (default `_files`), keyed by `as`:

```json
{
  "_files": {
    "welcome": {
      "found":    true,
      "content":  "<!doctype html>…",
      "encoding": "utf8",
      "ctype":    "text/html; charset=utf-8",
      "size":     1234,
      "path":     "_mail/welcome.html"
    }
  }
}
```

`_files` is `_`-prefixed, so it's dropped from the default web response — scratch
the client never sees. A missing file is `{ "found": false, "path": … }`, not an
error (unless `strict`, below). The result is an **object keyed by alias** (never
an array), so re-running the op overwrites cleanly instead of appending.

## Dynamic paths

`files` is resolved before the op runs, so paths can be computed from the
document — request fields, a manifest, the recipient:

```txcl
WITH files = &array(&object(
  "path", &concat("docs/", @web.req.url.query.name.0, ".md"),
  "as",   "doc"))
EXEC "txco://read-file"
```

A prior op can also assemble the whole array; then you just pass
`files = .the_array`.

## Options

| WITH | Default | Does |
|---|---|---|
| `files` | — (required) | `&array` of `{ path, as }`. |
| `into` | `_files` | Where the result object lands. |
| `encode` | `auto` | `auto` = UTF-8 text as a string, binary as base64; force with `utf8` / `base64`. |
| `strict` | `false` | A missing or over-cap file fails the op instead of `found:false`. |
| `max_bytes` | `--read-file-max-bytes` (1 MiB) | Per-file cap; over-cap is truncated (or errors under `strict`). |

Each entry's `encoding` tells a consumer how to read `content`, so binary assets
round-trip as base64 without corrupting the document.

## Scope & safety

- Reads only the **routed tenant + stack**'s `FILES/` (then the workspace-wide
  and embedded layers) — never another tenant's, never an arbitrary host path.
- Path-cleaned: `..` traversal is rejected.
- Pure in-memory — it reads the same content-addressed index `txco://static`
  uses, so it never touches the filesystem on the request path.
- Pays normal [fuel](./fuel.md) and shows up in [traces](./trace.md).
