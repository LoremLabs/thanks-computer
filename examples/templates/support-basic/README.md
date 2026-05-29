# support-basic template

A minimal customer-support pipeline showing the layout conventions:

- **Numeric scope dirs with optional descriptive suffix**: `0000_SETUP/` is scope 0, `0100_TRIAGE/` is scope 100. The integer prefix is the scope; the underscore-suffix is for human readability and is ignored by the chassis. Leading zeros pad to 4 digits for sortable filesystem listings.
- **Multiple rules per scope** (the central feature of the chassis): each `<name>.txcl` file in a scope dir is a separate parallel rule. The filename minus `.txcl` is the rule's name, used as the upsert identity by `txco apply`.
- **Symbolic operation references**: rules use `EXEC "op://NAME"` instead of hardcoded URLs. `txco apply --target <env>` resolves each `op://` to a real URL using the operations map in your workspace's `txco.yaml`. Same rule files apply to dev / staging / prod with the URL swapping at apply time.

## Stages

| Scope | Dir | Rules | Operation refs |
|---|---|---|---|
| 0 | `0000_SETUP/` | `audit.txcl`, `enrich.txcl` | `op://AUDIT` (audit) — enrich uses txco://noop |
| 100 | `0100_TRIAGE/` | `classify.txcl` | `op://CLASSIFY` |
| 200 | `0200_NOTIFY/` | `notify.txcl` | `op://NOTIFY` |

## After init

You'll need a workspace-level `txco.yaml` that defines the `CLASSIFY`, `NOTIFY`, and `AUDIT` operations. A complete example pairing with this template is at [`txco.yaml.example`](./txco.yaml.example) — copy it to your workspace root as `txco.yaml` and customize URLs per environment.

```sh
txco init support --from github:loremlabs/txco-templates/support-basic
cp <where-you-cloned>/examples/templates/support-basic/txco.yaml.example ./txco.yaml
# edit txco.yaml to point at your real services
txco apply --target dev
```

## Layout

```
support-basic/
├── txco.yaml.example        # sample workspace config (copy to <workspace>/txco.yaml)
├── 0000_SETUP/
│   ├── audit.txcl           # EXEC "op://AUDIT"
│   └── enrich.txcl          # EXEC "txco://noop"
├── 0100_TRIAGE/
│   ├── classify.txcl        # EXEC "op://CLASSIFY"
│   ├── mock-request.json
│   └── mock-response.json
└── 0200_NOTIFY/
    └── notify.txcl          # EXEC "op://NOTIFY"
```

After `txco init support --from github:loremlabs/txco-templates/support-basic`, the workspace contains rules at:

- `(support, 0)` × 2 (audit, enrich)
- `(support, 100)` × 1 (classify)
- `(support, 200)` × 1 (notify)
