# Continuations 

Built for waiting, continuations allow [Thanks, Computer](https://www.thanks.computer) operations to run and resume for human-scale work.

> I'll get back to you

## Work is waiting

Most operational work is waiting. 

- The model is still thinking
- The webhook hasn't fired
- A manager hasn't decided 

A **continuation** lets
an operation say "I'll get back to you" — the flow suspends, durably,
and resumes exactly once when the answer arrives. Waiting is most of a
long-running matter's life; continuations are how the chassis waits
without burning a connection, a thread, or a restart.

<img width="735" height="429" alt="image" src="https://github.com/user-attachments/assets/90047385-091a-4f08-8330-2857961f2337" />


## The shape

One directive turns a normal HTTP operation into a long-running one: `WITH mode = "async"`

```txcl
WHEN .doc.kind == "contract"
WITH mode = "async", timeout = "2h"
EXEC "https://reviewer.example.com/analyze"
```

Three parties, three moves:

1. **The chassis calls your worker** with the op's input plus a
   callback contract: a `callback_url`, an expiry, and a single-use
   bearer token (`X-Txco-Continuation-Token` header).
2. **The worker acks immediately** — return `202 Accepted` and go do
   the slow thing. The flow suspends at this step's barrier; state is
   persisted.
3. **The worker calls back whenever it's done** — `POST` to the
   `callback_url` with `{"status": "completed", "output": {…}}` and
   the token. The output merges into the shared document and the flow
   advances from exactly where it stopped.

Meanwhile, the original caller isn't left hanging: JSON clients get a
`202` with a continuation id and poll the *same URL* with
`?_txc.continuation=<id>`; browsers get redirected to an
auto-refreshing wait page that turns into the answer when it lands.

## Fast when it can be: `continuable`

`WITH mode = "continuable"` is the hybrid: the operation gets a grace
window (`WITH continue_after`, default 5s) to answer synchronously
like any normal op. Only if it's still running at the deadline does the
chassis promote it to a continuation. Quick answers stay quick; slow
ones become durable — the rule doesn't have to know in advance which
it will get.

Continuable is **op-level metadata, not a scheme gate** — it works on
any EXEC target. The headline case is `ai://chat`:

```txcl
WITH mode = "continuable", continue_after = "2s", provider = "openrouter", prompt = "…"
EXEC "ai://chat"
```

A fast model reply behaves exactly like a sync op (billing stamps and
all); a slow one promotes, and the resumed flow runs the rest of the
pipeline with no inlet clock. `mode = "async"` stays HTTP-shaped
deliberately: async *is* the worker-callback contract (202 ack +
`callback_url` + single-use token), which only an external worker that
speaks it can honor. On transports where the timer can't plausibly win
(a sub-millisecond `txco://` op), continuable is pointless but legal —
the op just always completes synchronously. One transport-specific
caveat: a `compute://` error under continuable records a failed
terminal and the flow continues (best-effort continuation semantics)
rather than taking the sync path's loud fatal halt.

**Non-web inlets.** The 202/wait-page story is the web inlet's. On an
LMTP-driven stack, a promotion closes the mail session with whatever
verdict is already on the envelope; if no verdict was emitted yet, the
chassis synthesizes a **250 accept** — a durable suspend *is*
acceptance (the run is persisted and will complete), so bouncing or
tempfailing a message the flow is still processing would be wrong. A
pre-emitted verdict (broadcast code or per-recipient array) always
wins, so emit it in a scope before the suspending op when you want
something other than 250.

## Why it's safe to wait

- **Durable.** Suspended state is files on disk, not memory. The
  chassis can restart — or crash — mid-wait; the callback still lands.
- **Exactly once.** The token is single-use and the first callback
  wins; duplicates are acknowledged but ignored. On resume, completed
  operations are never re-run — the flow continues from the suspend
  point, not from the top.
- **Context rides along.** Tenant, trace identity, and
  [fuel/TTL budgets](./advanced/fuel.md) travel with the suspended
  flow, so a resumed request is still the same accountable request.

:::note
**The first callback wins.** The token is single-use and completed steps are never
re-run, so make your worker's first `POST` the authoritative answer — the chassis
won't retry a callback that reported failure.
:::

## What to build with it

- **Slow AI.** A deep-research model that takes twenty minutes is just
  a worker that acks now and calls back later.
- **Webhook round-trips.** Kick off a payment, a build, a signature
  request — the provider's webhook handler is your callback.
- **Humans in the loop's flow.** The worker doesn't have to be software: a
  service that emails someone "approve / reject" and POSTs the
  callback when they click is a person, wired in through the same
  contract. The approval step is just an operation that takes a day.

