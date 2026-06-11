# Arcs — what Thanks, Computer manages

_Thanks, Computer runs stacks of rules that respond to events — this
page names what all those events add up to. ([Overview](./overview.md))_

Every platform has a unit of attention. Chat apps manage
**conversations**. Web apps manage **sessions**. Ticket systems manage
**tickets**. Thanks, Computer manages **arcs**: ongoing matters that
open with an event, advance across channels and participants, and
eventually resolve.

An invoice dispute is an arc. It opens when a customer's email arrives.
A classifier reads it, a service pulls the account, a human approves
the credit two days later, a cron sweep nudges the ones still waiting,
a payment webhook closes it out. Five channels, three kinds of
participant, a week of elapsed time — one arc.

## The shape of an arc

Most real operational work looks like this, and almost no software is
built for it:

- **It spans channels.** The same matter is touched by email, web,
  schedule, and an AI agent's tool call — and shouldn't care which.
- **It spans participants.** Services compute, AI drafts, humans
  decide. All of them read and write the same shared document.
- **It spans time — and mostly waits.** Minutes of compute inside
  weeks of elapsed time. The platform's job is to wake the right
  operation when something happens, and otherwise cost nothing.
- **It resolves.** An arc isn't a stream to monitor; it's a story with
  an ending. And it leaves a [trace](./trace.md) — the arc's complete
  record, readable by a person or an AI.

## An arc is not a workflow

A workflow is drawn in advance: boxes, arrows, every path anticipated.
An arc *unfolds*. Each event — a mail arriving, a form post, a cron
tick — is a beat that merges into the arc's shared document, and the
next step's [resonators](./txcl.md) read what's there and decide what
fires. The classifier doesn't know a VIP handler exists; it emits
`.tier = "vip"` and whatever resonates with that, runs. The path is
discovered, not wired — which is why adding a new behavior to a live
arc is writing one rule, not redrawing a diagram.

## How the pieces map

| Piece               | Role in the arc                                  |
| ------------------- | ------------------------------------------------ |
| Event               | A beat — one thing happening, on any channel     |
| Shared document     | The arc's memory — every operation's output merges here |
| Resonators          | The arc's attention — deciding which operations care about this beat |
| Op stack            | The department's playbook for arcs of this kind  |
| Trace               | The arc's record — what happened and why         |

A stack handles many arcs at once, the way a support desk handles many
cases: one playbook, many open matters, each at its own point in the
story.

The ladder continues upward, too: arcs can belong to a **saga** — the
longer mission a matter serves — so the *why* travels with the work.
See [Sagas](./sagas.md).

