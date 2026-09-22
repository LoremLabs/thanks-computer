# state-hello — durable records with atomic transitions via `txco://state/*`

A stack that keeps a task's state in a **state record** — a state and a
version, changed only by compare-and-swap — and journals every transition
the chassis presents into `_state`.

```
OPS/state-demo/
  100/create.txcl       POST /task/create?id=…            state/create (working@1)
  100/move.txcl         POST /task/move?id=…&from=…&to=…&v=…   state/transition (CAS)
  100/get.txcl          GET  /task?id=…                     state/get
  100/log.txcl          GET  /task/log?id=…                 the notebook _state/0 wrote
  200/*.txcl            responders; a state error → 409 with its code + reason
OPS/_state/
  0/log.txcl            every presented transition → notebook/append (idempotent on event_id)
  10/slow.txcl          a transition into "slow" runs long, to show `done` is at acceptance
```

Run it:

```
txco dev --state      # from this directory; --state starts the dispatcher
curl -X POST 'http://localhost:8080/task/create?id=t1&owner=me'
curl -X POST 'http://localhost:8080/task/move?id=t1&from=working&to=waiting&v=1'
curl -X POST 'http://localhost:8080/task/move?id=t1&from=working&to=waiting&v=1'   # 409: version conflict, current = waiting@2
curl -X POST 'http://localhost:8080/task/move?id=t1&from=working&to=ready&v=2'     # 409: state conflict
curl -X POST 'http://localhost:8080/task/move?id=t1&from=waiting&to=ready&v=2'     # ready@3
curl 'http://localhost:8080/task?id=t1'
curl 'http://localhost:8080/task/log?id=t1'    # two entries: working→waiting@2, waiting→ready@3, each with the HTTP request's rid as trace
```

The slow run (optional): start `python3 -c 'import http.server,time
class H(http.server.BaseHTTPRequestHandler):
  def do_GET(s): time.sleep(5); s.send_response(200); s.end_headers(); s.wfile.write(b"{}")
http.server.HTTPServer(("127.0.0.1",8377),H).serve_forever()'`, then move a
task into `slow` and read `.txco/dev/state.db` (`sqlite3 … "select status,
delivered_rid from state_events"`) before the five seconds are up: the
event is `done` while `_state/10` is still waiting.

See `docs/advanced/protocols/state.md`.
