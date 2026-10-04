# Inspect — ask a stack what is true now

_`txco trace` answers "what just happened?". `txco inspect` answers "what is the
current state, and why?": it asks the tenant's own rules, because only the stack
knows how its state is keyed. The question becomes an ordinary event,
`@src == "inspect"`, and the rule that knows the answer replies with a **card**._

```sh
txco inspect crm company openai                 # the card, rendered
txco inspect crm company openai --json          # the card document
txco inspect crm company openai --arg window=30d
```

The arguments are a stack, then optionally a noun and an id, then any `--arg k=v`.
Over the admin API the same question is
`POST /v1/tenants/{tenant}/inspect` with `{"stack", "noun", "id", "args"}`.
(`txco package inspect` is a different verb: it reads a package's manifest.)

## Answering

The chassis routes the question to the tenant's `_inspect` stack. A rule there gates
on the stack and noun it can explain, and writes the card at `._inspect.card`:

```txcl
# OPS/_inspect/100/company.txcl
WHEN @src == "inspect" && @inspect.stack == "crm" && @inspect.noun == "company"
  EMIT ._inspect.card = &object(
         "title", "Company",
         "sections", &array(&object(
           "title", "Request",
           "rows", &array(&array("Id", @inspect.id))
         ))),
       @halt = true
```

The question is at `@inspect.stack`, `@inspect.noun`, `@inspect.id` and
`@inspect.args.<k>`. The chassis writes those, so a rule cannot be fooled about
which tenant asked or what.

A card is `{title, sections: [{title, rows: [[label, value], …]}], raw}`. A value
may be any JSON. `raw` is optional: the domain JSON behind the card, for `--json`
readers. Real inspectors look things up first — `txco://kv/get` a snapshot,
`txco://read-file` a manifest, a nano-op that shapes the card.

When no rule answers (no `_inspect` stack, or none for that stack and noun), the
answer is `404 no_inspector`.

A package that ships an `_inspect/` stack makes every install of it debuggable.

## Example

[`examples/inspect-hello`](../../examples/inspect-hello) — an inspector that echoes the
question back as a card.
