# The Context — A shared context that gets passed between operations

In [Thanks, Computer](https://www.thanks.computer) operations are glued together by passing a JSON `context`.

> Read JSON. Write JSON. Merge JSON.

We coordinate across operations via the context, a serialized state of the event flow at a point in time.
Because every [operation](../resonators.md) of the same step runs in parallel, they all receive the same input as JSON.

When operations finish, they emit JSON as their "answer", which gets merged into the context. Because of this, each operation
needs to be careful about the namespace it uses, as **two operations running at the same step could clobber each other's responses if they write to the same part of the context tree**. You'll find yourself creating merge operations that merge together previous steps responses, occasionally, and that's totally ok--it's the trade-off we're making.


## Private Context

By convention, anything starting with a `_` is considered private and won't be returned in the final result under normal production paths. Subsequent operations will be able to see the entire context, even those branches that start with a `_`, so if you want to prevent this, be sure to use the `SELECT` [txcl](./txcl/txcl.md) in your operation's resonator to restrict its input.

## System Context

The `_txc` branch is for system-related data, such as identity and routing information. As syntactic sugar you can use `@` which 
is a shorthand for `_txc.` Note that this shorthand is for chassis `txcl`, operations must still return as `_txc` in their response. 

> `@src` and `_txc.src` are equivalents.

### Identity and routing data:

| Field | Meaning |
|---|---|
| `@src` | Head: `http`, `lmtp`, `cron`, `tcp` |
| `@rid` | Request id (trace correlation) |
| `@tenant` / `@stack` | Resolved by [ingress](../routing.md); pinned per request |
| `@ingress` / `@hostname_verified` | Matched ingress key / ownership-verification bit |
| `@op` / `@step` | The firing op's identity and scope (stamped on dispatched envelopes) |

### Per-head request data:

| Head | Namespace highlights |
|---|---|
| web | `@web.req.method`, `@web.req.url.{path,hostname,port,full,query.<k>.0,query.raw}`, `@web.req.headers.<name>.0` (arrays), `@web.req.cookies.*`, `@web.req.body` (base64), `@web.req.host`, `@web.req.proto` |
| lmtp | `@lmtp.rcpt[]`, `@lmtp.msg.{subject,text,html,from[].addr,to[],headers.*,attachments[],raw}` (`text`/`html` are the parsed bodies; `raw` is the b64 original), `@lmtp.listener`; spam verdict under `@mail.spam.{score,verdict}` when an upstream Rspamd stamped it |
| cron | `@cron.job`, `@cron.tenant` |
| tcp | `@tcp.listener`, `@tcp.{local,remote}.{ip,port}` |


## Flow control

Once operations results are merged, the chassis looks to see if flow control should be altered.

By default the chassis moves from the current step, to the next step in the stack, noting that steps do not
need to be sequential and can be sparse. (eg: step 2 to step 200).

Operations may set these in their response JSON which will effect the flow.

```jsonc
# stop the execution, return the context as is
{
    _txc: {
        halt: true 
    }
}
```

| Field | Effect |
|---|---|
| `_txc.halt = true` | Terminate after this scope's merge; return the document |
| `_txc.goto = "stack/0"` or `"200"` | Jump to a stage (bare number = current stack) |
| `_txc.ttl = N` | Lower (never raise) the remaining hop budget ([fuel](./fuel.md)) |
| `_txc.web.res.status` | HTTP response status |
| `_txc.web.res.headers.<name>` | Response headers (arrays) |
| `_txc.web.res.body` | Response body (base64); set in a non-terminal scope it streams |
| `_txc.lmtp.res.{code,msg,recipients[]}` | SMTP verdict ([lmtp](./protocols/lmtp.md)) |

Everything else under `_txc.*` is chassis-owned — writes to reserved
fields (`tenant`, `fuel_used`, `computed.*`, …) are rejected. 

Payload fields (no `_txc.` prefix) are always available; fields starting with `_` are dropped from the final answer by convention.

