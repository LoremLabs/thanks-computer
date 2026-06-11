# Tutorial — the approval flow

_One real flow, built end to end: a customer email arrives, AI drafts
the reply, a human approves it, the chassis sends it. Three rules, one
small service of your own. ([Overview](./overview.md) ·
[Quickstart](./quickstart.md) first if you haven't.)_

This is the shape of most operational work — something arrives, a
machine does the legwork, a person decides, the system follows
through — so it makes a good first stack. Here is the whole thing:

```
OPS/support/
  0100_DRAFT/draft.txcl       # AI drafts a reply
  0200_APPROVE/approve.txcl   # a human decides (the flow waits)
  0300_SEND/send.txcl         # approved → send it
  0300_SEND/declined.txcl     # declined → record and stop
```

The numbered directories are the **steps** of the flow, in order.
Every rule below is shown in full.

## Step 1 — AI drafts, with the mission attached

`OPS/support/0100_DRAFT/draft.txcl`:

```txcl
WHEN @src == "lmtp"
SET .saga.name = "q3-retention",
    .saga.goal = "keep churn under 2%"
WITH system = "You draft warm, concise support replies. The mission is retention: keep this customer.",
     prompt  = "Draft a reply to this customer email:\n\n{{@lmtp.msg.text}}",
     intent  = "draft_support_reply"
EXEC "ai://chat"
```

Reading it: fire when mail arrives (`@src == "lmtp"`); stamp the
[saga](./sagas.md) onto the document — sagas are just fields you
choose, no registration — so every later participant knows
*why* this matter exists; hand the email's text to
[`ai://chat`](./ai.md) (`{{@lmtp.msg.text}}` reads the parsed message
body from the envelope). The model's draft merges back as `.text`.

## Step 2 — a human decides, and the flow waits

`OPS/support/0200_APPROVE/approve.txcl`:

```txcl
WHEN .text != ""
WITH mode = "async", timeout = "2h"
EXEC "https://approve.internal.example.com/review"
```

`mode = "async"` makes this a [continuation](./continuations.md): the
flow suspends — durably, surviving restarts — until your reviewer
service calls back.

The reviewer service is yours, and it's small. It receives the
document (the email, the draft, the saga) plus a callback contract — a
`callback_url` and a single-use token in the
`X-Txco-Continuation-Token` header. It does three things:

1. Returns `202 Accepted` immediately.
2. Puts the draft in front of a person — email yourself an
   approve/decline link, post it to a channel, render a page.
3. When the person clicks, POSTs the verdict to the `callback_url`:

```json
{"status": "completed", "output": {"approved": true, "reply": "<the final text>"}}
```

`output` is arbitrary JSON — it deep-merges into the document exactly
like any operation's response — and the flow advances, whether the
click came two minutes or two hours later.

## Step 3 — follow through

Two rules at the same step; only the one that resonates fires.

`OPS/support/0300_SEND/send.txcl`:

```txcl
WHEN .approved == true
SET ._sendmail.subject = &concat("Re: ", @lmtp.msg.subject),
    ._sendmail.from    = "support@acme.example",
    ._sendmail.to      = @lmtp.msg.from.0.addr,
    ._sendmail.body    = .reply
EXEC "txco://sendmail"
EMIT @halt = true
```

`OPS/support/0300_SEND/declined.txcl`:

```txcl
WHEN .approved == false
EMIT .outcome = "declined", @halt = true
```

The send rule assembles the [`_sendmail` contract](./advanced/protocols/sendmail.md)
straight from the document — reply-to address from the original mail,
body from the approved text. (Outbound mail requires a configured
relay, and the `from` domain must be a verified hostname of your
tenant — the anti-spoof rule.)

## Run it

Mail reaches the stack through the LMTP head behind a Postfix — wiring
in [lmtp.md](./advanced/protocols/lmtp.md). But you don't need mail to
develop the flow: [mocks](./authoring/mocks.md) let you run the whole
stack first — point step 1 and step 2 at fixtures
(`mock-response.json` + `X-Txco-Mocks: support/**`), fire events with
`curl`, and watch the logic route, merge, and halt before any real
model, reviewer, or mailbox exists.

```sh
txco apply
txco trace last
```

The [trace](./trace.md) is the payoff: three steps, the model's
tokens, the suspend at step 2 with the hours-long gap plainly visible
in the timings, the resume, the send — the whole story of one matter,
on disk, readable by you or by an AI you ask to debug it.

## What you just used

One stack exercised most of the system: multi-protocol
[ingress](./ingress.md), [AI as an operation](./ai.md), intent as data
([sagas](./sagas.md)), a human in the loop via
[continuations](./continuations.md), outbound
[email](./advanced/protocols/sendmail.md), parallel rules at a step,
and the [trace](./trace.md). Every piece is swappable the same way —
the approver could become a Slack bot, the model a different provider,
the channel a web form — without touching the rules around it.
