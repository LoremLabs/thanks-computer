# membership-and-file-literals — `in`, `exists`, `$[path.json]`

`GET /check?status=running&noun=run&tag=` classifies its query three
ways and answers with the verdicts plus two values read from JSON files
beside the ops.

```
OPS/membership/
  _data/statuses.json     ["pending", "running", "complete"]
  _data/config.json       {"enabled": true, "limits": {…}, "optional": null}
  100/status.txcl         … && @web.req.url.query.status.0 in $[../_data/statuses.json]
  100/noun.txcl           … && @web.req.url.query.noun.0 in ["seed", "run", "wake"]
  100/tag.txcl            … && exists @web.req.url.query.tag
  200/*.txcl              exists / !exists on the scope-100 flags → body fields
  300/respond.txcl        EMIT .statuses = $[…], .config = $[…], halt
```

Run it:

```
txco dev          # from this directory
curl 'http://localhost:8080/check?status=running&noun=run&tag='
```

```json
{"known":true,"noun":"known","tagged":true,"statuses":["pending","running","complete"],"config":{"enabled":true,"limits":{"max":10,"tags":["a","b"]},"optional":null}}
```

Three things to notice.

- **`in` is strict.** `?status=running` matches `"running"`; a status
  the list doesn't have, or a `"1"` against a list of numbers, does not.
  Before `in`, `noun.txcl` read `=~ /^(seed|run|wake)$/`.
- **`exists` is presence, not emptiness.** `?tag=` carries an empty
  value and still exists; `=~ /./` would say no. Scope 200 uses
  `!exists ._known` for "the flag was never set", which is what the
  older `!= true` idiom was reaching for.
- **`$[path.json]` keeps the JSON's type.** `statuses.json` becomes an
  array literal, `config.json` an object literal with its nested array
  and its `null`, both written in when `txco` reads `OPS/` and both
  validated then — a typo in either file fails `txco apply` with the
  file, line and column. `txco pull` shows the expanded literal; edit
  the file, not the op.

See [txcl — Conditions](../../docs/advanced/txcl/txcl.md#conditions) and
[File-backed literals](../../docs/advanced/txcl/txcl.md#file-backed-literals--pathjson).
