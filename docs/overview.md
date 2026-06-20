<!-- nav: Overview -->

# Thanks, Computer

## Long-term goals. Everyday decisions. 

Thanks, Computer is a programmable runtime for durable, human-in-the-loop workflows. Pause an operation for a slow model, a webhook, or a human approver — for days — and resume exactly once, surviving restarts. Any language, plain JSON, every step replayable. Most workflow engines automate people *out* of a process; here a person — or a slow AI — is a first-class participant, not an exception.

Bundle a set of operations into a reusable _stack_, and curate stacks into a **department** — a Support department, an Invoicing department, an Onboarding department — that runs a real business function on your own data, carrying its context across emails, web requests, schedules, approvals, and AI.

> Your LLM has context. Why shouldn't your work? 

### What people build with it:

- a support department that drafts replies with AI and pauses for a human to approve
- qualify sales opportunities against your ideal customer profile
- prioritize customer success work around retention
- guide partnership pipelines with strategic priorities
- keep fundraising and business development efforts in context
- give AI workflows the company context behind the task
      
## How it works

Work is broken into small steps called **operations**, organized into an **op
stack**. Each operation is gated by a [**resonator**](./resonators.md) — a trigger condition that fires only when the right kind of event passes by, and says what to do when it does:

```txcl
WHEN @web.req.url.path == "/opportunity"
EXEC "https://api.example.com/enrich-opportunity"
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

You can think of operations as being black boxes. They receive JSON, emit JSON, and **can be written in any language**. Emails, web requests, AI tool calls, and schedules all become events flowing through the same shared document.

A stack's operations may span many systems, but its behavior remains visible:

- **Visible decisions:** Replay every flow step-by-step.
- **The why travels with the work.** Context stays attached as work moves.
- **Built for waiting.** [Continuations](./continuations.md) let an operation
  suspend — for a slow model, a webhook, a human reviewer — and resume days
  later, exactly once, surviving restarts. 
- **Any language, plain JSON.** Read JSON, write JSON.

## Run anywhere

Self-host the open-source chassis on your own infrastructure, or let our cloud be the place to run it — managed reliability, email deliverability, and fleet scale, so your departments run like infrastructure instead of a script you babysit. The CLI `txco` targets either. The chassis is free and open source under the MPL-2.0 — self-host it, commercial use included.


**Try it in two minutes:** [Quickstart](./quickstart.md) 

```bash
txco demo
```
