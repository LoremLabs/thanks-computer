# Schemas — stacks that describe themselves

_In Thanks, Computer, every participant in a flow reads and writes one
shared JSON document — this page is about optionally writing that shape
down, so people and machines can read it. ([Overview](./overview.md))_

The shared document is the contract between everything in a stack —
services in different languages, AI ops, human approvals, the next
team's integration. But by default it's a contract nobody wrote down:
the shape lives in everyone's head, one merge away from drift.

Good practice: ship a [JSON Schema](https://json-schema.org) with your
stack describing the shape it expects on entry and the shape of its
answer. The chassis never enforces it — rules fire on whatever arrives,
exactly as before. A schema is documentation that machines can also
read, and that's precisely what makes it pay:

- **Installing a package.** A stack's schema tells you what to wire
  before you `txco apply` — the required fields are a checklist, not
  archaeology.
- **Ops in any language.** Generate types, stubs, or validators from
  the schema in whatever language your op is written in — the
  JSON Schema toolchain already exists everywhere.
- **AI agents.** A tool call needs a declared input shape; a stack
  with a schema is already most of the way to being an MCP tool an
  agent can discover and call correctly.
- **Reviewable change.** When the shape changes, the schema diff shows
  it in the pull request — before a downstream op finds out at 2 a.m.

## The convention

Put a `schema.json` at the stack root, next to its rules, with two
named definitions — what comes in, what goes out:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "support/dispute",
  "description": "Resolves a disputed invoice; opens on inbound mail or web form.",
  "$defs": {
    "input": {
      "type": "object",
      "required": ["invoice_id"],
      "properties": {
        "invoice_id": { "type": "string" },
        "amount":     { "type": "number" }
      }
    },
    "output": {
      "type": "object",
      "properties": {
        "resolution": { "enum": ["credited", "upheld", "escalated"] },
        "saga":       { "type": "object", "description": "mission context carried by this arc" }
      }
    }
  }
}
```

Describe the *public* shape. Fields prefixed with `_` are private by
convention and dropped from the answer ([TXCL](./txcl.md)) — leave them
out of the schema, along with the chassis's own `_txc.*` envelope.
Documenting the saga fields your stack carries is worth the lines: the
schema then records not just the shape of the work but the intent
riding with it ([Sagas](./sagas.md)).

