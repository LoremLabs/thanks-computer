# Thanks, Computer

**Computational continuity for your work.**

Most software forgets.

A ticket doesn't know the objective it serves. An AI agent doesn't know why
it's acting. An approval request arrives stripped of the context that created
it. The work moves; the why stays behind.

[Thanks, Computer](https://www.thanks.computer/) (TxCo) is built the opposite way, believing that context, memory, and 
intent should travel with the work. Events arrive from
anywhere — a web request, an inbound email, a cron tick, an AI agent. Every
rule that resonates fires: invoking your services, running code, asking an AI
model, or pausing while a human weighs in. The results merge into one shared
document, and that document — the what, still carrying its why — is the
answer.

That continuity holds across every timescale of your work:

| Level                  | What it is                          | Lives for         |
| ---------------------- | ----------------------------------- | ----------------- |
| **[Saga](./sagas.md)** | A mission — *why* this matters      | quarters, years   |
| **[Arc](./arcs.md)**   | A matter — *what* we're resolving   | days, weeks       |
| **Event**              | A beat — *now*, one thing happening | milliseconds–days |

Because every operation reads the same shared document, a participant three steps downstream — human or AI — still knows what matters and why. Use TxCo to run support, approvals, grants, or reporting — each as a small stack of plain-text rules that call your APIs or AI to move your work forward.

Underneath is a simple view: operational software isn't an application, it's
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
deep-merges into the shared document, which carries the flow to the next step.
Operations execute only when their conditions match — nothing runs that
doesn't need to. An operation can be no code at all (the rule itself shapes
the flow), any HTTP service in any language, sandboxed JavaScript running on
the chassis, or an external AI tool.

Because the event is shared external memory — not parameters threaded through
call chains — humans and AI work the same matter together: an AI op drafts, a
human op approves, a service op ships, all reading and writing the same
document.

## What makes it different

- **Every protocol, one flow.** Web, email, cron, TCP, and MCP
  ingress all land in the same envelope and the same rules. The same stack
  that answers `https://ops.example.com` answers `support@ops.example.com`
  and answers an AI agent's tool call.
- **Decisions are visible.** Logic lives in readable text files, not YAML
  sidecars or buried application code. Every flow leaves a full trace you can
  replay step by step — debuggable for computers and AI alike.
- **The why travels with the work.** Stamp an arc with the saga it serves and
  the objective rides in the envelope — services, AI, and humans all decide
  in context, instead of working from a goal they never met.
- **Built for waiting.** [Continuations](./continuations.md) let an operation
  suspend — for a slow model, a webhook, a human reviewer — and resume days
  later, exactly once, surviving restarts.
- **Nothing to deploy.** One static binary is the whole chassis. Small logic
  runs as sandboxed Wasm on the chassis itself: no containers, no cold
  starts, safe multi-tenant isolation by default.
- **Any language, plain JSON.** An operation is any HTTP service that reads
  JSON and returns JSON — if you can write a handler, you can write an op.

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

**Try it in two minutes:** [Quickstart](./quickstart.md) · `txco demo`
