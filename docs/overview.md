# Thanks, Computer

**Run a part of your business as a page of readable rules.**

Thanks, Computer (TxCo) is a distributed runtime that turns a department of
your organization — a support desk, an intake pipeline, an approvals process —
into a small stack of plain-text rules. An event arrives from anywhere: a web
request, an inbound email, a cron tick, an AI agent calling a tool. Every rule
that resonates with it fires in parallel — invoking your services, running
sandboxed code, asking an AI model, or pausing for hours while a human weighs
in. The results merge into one shared document, and that document is the
answer.

The bet behind it: most operational software isn't an application, it's
coordination — between systems, services, people, and time. Today that
coordination hides in glue code, queues, and inboxes. TxCo makes it a
first-class, inspectable artifact: a flow you can read, version, trace, and
share.

## How it works

Work is broken into small steps called **operations**, organized into an **op
stack**. Each operation is gated by a **resonator** — a few lines of
[TXCL](./txcl.md) saying when it should fire and what it should do:

```txcl
WHEN @web.req.url.path == "/invoice"
EXEC "op://extract-invoice"
EMIT .processed_at = &now("rfc3339")
```

Everything at the same step runs in parallel; each operation's output
deep-merges into the shared event, which carries the flow to the next step.
Operations execute only when their conditions match — nothing runs that
doesn't need to. An operation can be no code at all (the rule itself shapes
the flow), any HTTP service in any language, sandboxed JavaScript running on the
chassis, or an external AI tool.

Because the event is shared external memory — not parameters threaded through
call chains — humans and AI participate in the same conversation: an AI op
drafts, a human op approves, a service op ships, all reading and writing the
same document.

## What makes it different

- **Every protocol, one flow.** Web, email (SMTP), cron, raw TCP, and MCP
  ingress all land in the same envelope and the same rules. The same stack
  that answers `https://ops.example.com` answers `support@ops.example.com`
  and answers an AI agent's tool call.
- **Decisions are visible.** Logic lives in readable text files, not YAML
  sidecars or buried application code. Every flow leaves a full trace you can
  replay step by step — debuggable for computers and AI alike.
- **Built for waiting.** Continuations let an operation suspend — for a slow
  model, a webhook, a human reviewer — and resume days later, exactly once,
  surviving restarts.
- **Nothing to deploy.** One static binary is the whole chassis. Small logic
  runs as sandboxed Wasm on the chassis itself: no containers, no cold
  starts, safe multi-tenant isolation by default.

## Where it's going

Op stacks are made to travel. A stack is a versioned, signed artifact —
distributable through standard OCI registries — so a working department
(invoicing, triage, onboarding) becomes something you install, not rebuild.
A growing fleet runtime adds custom domains, instant rollback, and a control
plane, so `ops.yourcompany.com` becomes programmable operational authority:
every channel into your business — mail, web, agents — arriving at rules you
wrote, can read, and can trust.

The open-source chassis is complete on its own (Mozilla Public License 2.0);
the hosted service layers on top without forking it.

**Try it in five minutes:** [Quickstart](./quickstart.md) · `txco demo`
