# Schemas — Stacks that describe themselves

In [Thanks, Computer](https://www.thanks.computer), every participant in a flow reads and writes one
shared JSON document — while not required, storing the schema shape helps document for everyone. 

The shared document is the contract between everything in a stack —
services in different languages, AI ops, human approvals, the next
team's integration.

Good practice: ship a [JSON Schema](https://json-schema.org) with your
stack describing the shape it expects on entry and the shape of its
answer. 

The chassis currently does not enforce it — rules fire on whatever arrives,
exactly as before. A schema is documentation that machines can also
read, and that's precisely what makes it pay off:

- **Installing a package.** A stack's schema tells you what to wire
  before you `txco apply` — the required fields are a checklist, not
  archaeology.
- **Ops in any language.** Generate types, stubs, or validators from
  the schema in whatever language your op is written in — the
  JSON Schema toolchain already exists everywhere.
- **AI agents.** A tool call needs a declared input shape; a stack
  with a schema already declares the shape an MCP tool would expose —
  most of the way there, though serving it as a tool is something you
  wire up yourself ([MCP](./advanced/protocols/mcp.md)).
- **Reviewable change.** When the shape changes, the schema diff shows
  it in the pull request — before a downstream op finds out at 2 a.m.

## Documentation convention: `schema.json`

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
        "mission":    { "type": "object", "description": "the mission context this stack carries" }
      }
    }
  }
}
```

Then others who [use your stack](./advanced/txco-oci-packages.md) won't have to guess what an operation requires or emits. This can help provide clues to your editor for use in autocompletion too.