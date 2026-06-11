# Ingress — every protocol, one flow

_Thanks, Computer runs your rules against events — this page covers
where events come from, and how they all end up in the same flow.
([Overview](./overview.md))_

The same stack of rules can answer a web request, an inbound email, a
scheduled tick, and an AI agent's tool call. Whatever the wire
protocol, the chassis wraps what arrives in one **JSON envelope** —
payload plus a `_txc.*` namespace recording where it came from — and
hands it to your rules. A rule doesn't care which channel fired it
unless it asks.

| Channel  | Arrives as                          | Envelope says                  |
| -------- | ----------------------------------- | ------------------------------ |
| Web      | HTTP request on `:8080`             | `@src = "http"`, `@web.req.*`  |
| Email    | Inbound mail via LMTP (behind Postfix) | `@src = "lmtp"`, `@lmtp.*`  |
| Cron     | A scheduled tick                    | `@src = "cron"`                |
| TCP      | Line-delimited JSON on `:5050`      | `@src = "tcp"`                 |
| AI agents | An MCP tool call                   | rules exposed as tools         |

So a support flow can read like a sentence: *when mail arrives for
`support@`, classify it; when the web form posts, enter the same flow;
every morning at nine, sweep what's still open.* One stack, three
channels, no glue code.

## Routing: which rules see the event

Before any rule fires, the ingress router answers two questions: which
**tenant** does this event belong to, and which **op stack** should it
enter? It matches on the channel's natural key — the HTTP `Host`
header, the mail recipient, the cron job name — and stamps the answer
onto the envelope as `@tenant` and `@stack`.

A single-tenant chassis needs none of this; events fall through to the
default `boot` stack and everything just works. Configure routing when
you host multiple tenants or bind custom domains:

```yaml
ingress:
  http:
    hosts:
      acme.example.com: { tenant: acme, stack: acme/web }
  lmtp:
    recipients:
      "support@acme.example": { tenant: acme, stack: acme/support }
```

## Email is a first-class citizen

Inbound mail isn't a webhook bolted on — the chassis speaks LMTP behind
a standard Postfix, so a message becomes a normal event with recipient,
subject, headers, body, and attachments readable by any rule. Each
recipient routes independently to its own tenant and stack:
`support@acme.example` can enter a different flow than
`billing@acme.example`.

## AI agents speak it too

MCP (Model Context Protocol) runs in both directions: rules can expose
themselves as tools that agents like Claude invoke (ingress), and a
rule can call an external MCP tool with `EXEC "mcp+https://…"`
(egress). An agent becomes one more participant in the flow — entering
through the same envelope, leaving the same trace.
