# notebook-log — an append-only record via `txco://notebook/*`

A stack that journals what happened to a task and reads it back. The KV
store holds a stack's current state; a **notebook** holds its history —
ordered, append-only, read by cursor / time window / tail (always oldest
first) and exported as newline-delimited JSON.

```
OPS/notebook-demo/
  100/parse.txcl        POST /log/append — decode the JSON body
  110/append.txcl       notebook/append WITH type, data, object_key (idempotent)
  100/read.txcl         GET /log/read?nb=…[&tail|&after|&since|&limit]
  100/export.txcl       GET /log.ndjson?nb=…  (the op writes the body + halts)
  100/list.txcl         GET /log/list[?prefix=…]
  100/delete.txcl       POST /log/delete?nb=…
  200/*.txcl            responders; any notebook error → 400 with its code
```

Run it:

```
txco dev          # from this directory
curl -X POST 'http://localhost:8080/log/append?nb=task/1' -d '{"type":"task.created","data":{"who":"me"},"object_key":"created"}'
curl -X POST 'http://localhost:8080/log/append?nb=task/1' -d '{"type":"task.note_added","data":{"note":"hello"}}'
curl -X POST 'http://localhost:8080/log/append?nb=task/1' -d '{"type":"task.completed","data":{"exit":0},"object_key":"done"}'
curl -X POST 'http://localhost:8080/log/append?nb=task/1' -d '{"type":"task.completed","data":{"exit":0},"object_key":"done"}'   # existed:true, same seq
curl 'http://localhost:8080/log/read?nb=task/1&tail=2'      # newest two, oldest first
curl 'http://localhost:8080/log/read?nb=task/1&limit=2'     # next + cursor: page with &after=<next>
curl 'http://localhost:8080/log.ndjson?nb=task/1'           # application/x-ndjson
curl 'http://localhost:8080/log/list?prefix=task/'
txco notebook tail --url http://localhost:8081 --tenant default notebook-demo task/1
```

Cursors (`next`, `cursor`, `?after=`) are opaque strings; a bare number is
refused, and a cursor taken before a `/log/delete` answers
`txco_notebook_stale_cursor` once the notebook is written again.

See `docs/advanced/notebooks.md`.
