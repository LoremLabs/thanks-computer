# Sagas — carrying the why

_In Thanks, Computer, an [arc](./arcs.md) is one ongoing matter — an
invoice dispute, an onboarding. This page is about the level above: the
mission an arc serves, and how that context travels into the work.
([Overview](./overview.md))_

Every piece of work serves something bigger, and most systems lose
track of it by the second handoff. The ticket doesn't know about the
quarter's objective. The script doesn't know why it runs. The person —
or the AI — doing the task gets the *what* with the *why* stripped off.

Thanks, Computer's answer is to make intent data. A **saga** is a
mission that spans many arcs, the way an arc spans many events:

| Level     | What it is                        | Lives for        |
| --------- | --------------------------------- | ---------------- |
| **Saga**  | A mission — *why* this matters    | quarters, years  |
| **Arc**   | A matter — *what* we're resolving | days, weeks      |
| **Event** | A beat — *now*, one thing happening | milliseconds–days |

Because every operation reads the same shared document, the why rides
along with the what. Stamp the saga onto arcs as they open:

```txcl
WHEN @stack == "support/dispute"
SET .saga.name = "q3-retention",
    .saga.goal = "keep churn under 2%",
    .saga.metric.churn_target = 2,
    .saga.metric.current_churn = 2.7
```

Now every downstream participant works *in context*. The AI op drafting
a refund reply writes a different email when the document says the
mission is retention. The human approver sees the goal next to the
amount. The cron sweep can prioritize arcs by the saga they serve. The
objective isn't in a slide deck the task never met — it's a field in
the envelope, three steps away from every decision.

If your organization runs OKRs, the mapping is direct: a saga is an
objective, its arcs are the initiatives advancing it, and the beats are
the work — with the alignment carried in-band instead of reconstructed
at review time.

## Departments cut the other way

A department (an op stack) isn't a rung on this ladder — it's the
*playbook* that handles arcs of a kind. Sagas usually cross
departments: "launch in Europe" includes arcs handled by legal,
billing, and support. The ladder says why work exists; departments say
how a kind of work gets done.

## All of this is optional

None of these layers is machinery you must adopt. Events work alone.
Name an arc when beats need to belong together; name a saga when arcs
do. They're conventions over the shared document — fields you choose to
carry — which is exactly what makes them cheap to start and impossible
to outgrow. (And to be clear for the distributed-systems reader: this
is the storyteller's saga, not the saga *pattern* — no transaction or
compensation semantics implied.)

Start small: run your first stack on bare events. The day a customer
matter spans two emails and a cron sweep, give it an arc. The day
someone asks "which of these actually serve the launch?" — that's the
day you have a saga.
