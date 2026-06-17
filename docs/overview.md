<!-- nav: Overview -->

# Thanks, Computer

## Long-term goals. Everyday decisions. 


Thanks, Computer is a runtime for **building systems that remember**—an _intelligence matrix_ in which people, software, AI, and automated processes cooperate through shared context, memory, and goals.

By composing small, reusable _stacks_, you can build systems that remember what they’re trying to accomplish and carry that understanding across emails, web requests, schedules, approvals, and AI interactions.

> Your LLM has context. Why shouldn't your work? 

### Put your goals to work:

- qualify sales opportunities against your ideal customer profile
- prioritize customer success work around retention
- guide partnership pipelines with strategic priorities
- keep fundraising and business development efforts in context
- give AI workflows the company context behind the task
      
## How it works

Work is broken into small steps called **operations**, organized into an **op
stack**. Each operation is gated by a **resonator** — its firing condition, a
few lines of [TXCL](./txcl.md): like a tuning fork, it rings only when the
right kind of event passes by, and says what to do when it does:

```txcl
WHEN @web.req.url.path == "/opportunity"
EXEC "https://api.example.com/enrich-opportunity"
EMIT .qualified_at = &now("rfc3339")
```

Everything at the same step runs in parallel; each operation's output
deep-merges into the shared document, which carries the flow to the next step.


```stack
opportunities

50 get_mission get_okrs
100 enrich
200 score review
250 assign
300 followup
```

Because the event is shared external memory — not parameters threaded through call chains — humans, AI, and services can collaborate on the same work: an AI operation drafts, a human operation approves, a service operation ships, all reading and writing the same context document.

## Composable by design

You can think of operations as being black boxes. They receive JSON, emit JSON, and **can be written in any language**. Emails, web requests, AI tool calls, and schedules all become events interacting in the same intelligence matrix.

A stack's operations may span many systems, but its behavior remains visible:

- **Visible decisions:** Replay every flow step-by-step.
- **The why travels with the work.** Context stays attached as work moves.
- **Built for waiting.** [Continuations](./continuations.md) let an operation
  suspend — for a slow model, a webhook, a human reviewer — and resume days
  later, exactly once, surviving restarts. 
- **Any language, plain JSON.** Read JSON, write JSON.

## Run anywhere

You can self-host the open source chassis on your infrastructure, or let our cloud run the matrix fleet for you. Same stacks, same rules — the CLI `txco apply` targets either.

**Try it in two minutes:** [Quickstart](./quickstart.md) 

```bash
txco demo
```
