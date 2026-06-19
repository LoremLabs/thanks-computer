# Secrets — Segment your data



## TL;DR

If you're an end-user, this may be what you're looking for:

```bash
# Hidden TTY prompt; the value is never on the command line.
txco auth tenant secrets set STRIPE_API_KEY \
  --description "Stripe live key, rotated 2026-06-19"
```

In a txcl rule's `WITH` clause, reference a secret by name. The
chassis materializes the cleartext into the op handler's private
buffer at execution time; the value never enters `op.Input`,
trace events, mock fixtures, continuations, or logs.

```txcl
EXEC "https://api.stripe.com/v1/charges"
  WITH secrets.headers.authorization.secret = "STRIPE_API_KEY",
       secrets.headers.authorization.format = "Bearer {}",
       method = "POST"
```

For operators:

1. **Bootstrap is automatic**: the chassis mints a master key on first
   boot at `./chassis/data/secrets/txco-master.key`
   (`--secret-master-key` to relocate).
2. **Manage secrets via CLI**: `txco auth tenant secrets {set,
   generate, list, show, describe, rotate, revoke}`. Operator-supplied
   values come from a TTY prompt; chassis-generated values are printed
   exactly once.
3. **There is no reveal command.** To inspect a value, rotate the
   secret. Both `rotate` (with a new operator value) and `rotate
   --generate` (chassis mints) show you the value once.
4. **Back up the master-key file separately from the runtime DB.**
   Losing it makes every stored secret permanently unrecoverable.

## 1. Bootstrap

The secret store **auto-bootstraps on first chassis boot** — same
convention as the runtime DB. No explicit setup step required for
the default path. The chassis mints a 32-byte master key at
`./chassis/data/secrets/txco-master.key` (or wherever
`--secret-master-key` points) the first time it doesn't find one
there. On first mint you'll see this in the logs:

```
INFO  secret store: minted new master key — BACK THIS UP; losing it makes every stored secret unrecoverable  path=…/txco-master.key
INFO  secret store enabled  path=…/txco-master.key key_version=1
```

On every subsequent boot the existing key is loaded and only the
second line appears.

### Where the file lands

| Scenario | Default path |
|---|---|
| `txco serve` (production) | `./chassis/data/secrets/txco-master.key` |
| `txco dev` (local dev) | `<workspace>/.txco/dev/secrets/txco-dev-master.key` (gitignored) |
| Explicit override | `--secret-master-key /your/path` (or `TXCO_SECRET_MASTER_KEY=…`) |
| Library / embedder opt-out | Set `SecretMasterKeyPath` to empty string |

For production, point the flag at an operator-owned root such as
`/data/secrets/txco-master.key`. The auto-mint logic creates any
missing parent directories with `0700` perms; the key file itself
is `0600`.

### Explicit init 

Rarely you may want a different path before first boot. If so:

```bash
# Production: pre-mint at the production path before first chassis
# boot, so the operator chooses the location deliberately.
txco auth secrets init --path /data/secrets/txco-master.key
```

`init` is also the verb for **forced rotation** of an existing key
(see §6 — disaster). It refuses to overwrite unless `--force` is
passed, and `--force` then prompts for an "overwrite" confirmation.
**`init` refuses any path that points at a directory** — it suggests
the canonical filename instead.

### If the load fails

If the configured path exists but is malformed (wrong perms, wrong
size), you'll see a WARN and the chassis boots with the secret
store off:

```
WARN  secret store disabled: master key load failed  path=… err=…
```

The data plane stays up; any op declaring `secrets.*` in its
WITH clause then fails loud with `secret_store_unavailable`.

### Verify

```bash
# Generate a probe secret. The value is printed once on stdout;
# anything but a 43-char base64-url string means something's off.
txco auth tenant secrets generate PROBE_KEY --tenant default

# Confirm metadata visible:
txco auth tenant secrets list --tenant default

# Cleanup (the name frees for re-creation):
txco auth tenant secrets revoke PROBE_KEY --tenant default
```

## 2. Manage secrets

All operator-facing CRUD is under `txco auth tenant secrets`.
See `--help` on any verb for full flag reference.

### Store a vendor-supplied value

The operator already has a value (a Stripe key from the Stripe
dashboard, an OAuth client secret, etc.). `set` prompts for it via
TTY hidden input — it never appears on the command line, in
shell history, or in `ps` output.

```bash
# Hidden TTY prompt; the value is never on the command line.
txco auth tenant secrets set STRIPE_API_KEY \
  --tenant acme \
  --description "Stripe live key, rotated 2026-05-20"
```

If a secret by that name already exists, `set` rotates it (writes a
new version) rather than failing. The old version row is preserved
in `tenant_secret_versions` for audit history; the resolver only
ever sees the latest.

### Mint a fresh value (chassis-generated)

When you don't have a vendor-issued value — e.g. an HMAC signing
secret you're about to share with a webhook — let the chassis mint
one. The value is printed **once** to stdout; capture it
immediately or rotate to see it again.

```bash
txco auth tenant secrets generate WEBHOOK_HMAC \
  --tenant acme \
  --byte-len 32 \
  --description "Stripe webhook signing"
# stdout: jGWvsjCkKWXq0irVAZlSXNp1-qxQDh_yKpjzkrOlLVk
```

Format: base64-url no-padding. 32 bytes → 43 chars. Adjust
`--byte-len` for longer keys; max 4096.

### Rotate

```bash
# Operator-supplied new value (TTY prompt; no shell history):
txco auth tenant secrets rotate STRIPE_API_KEY --tenant acme

# Chassis mints a new random value, prints once:
txco auth tenant secrets rotate WEBHOOK_HMAC --tenant acme --generate
```

After rotate, the active version of the secret is the new one. Old
encrypted versions stay in the DB for audit; the resolver routes
all reads to the latest.

### Inspect (metadata only — value never shown)

```bash
# List active secrets in a tenant:
txco auth tenant secrets list --tenant acme

# Show metadata for one:
txco auth tenant secrets show STRIPE_API_KEY --tenant acme

# Update description without rotating the value:
txco auth tenant secrets describe STRIPE_API_KEY \
  --tenant acme \
  --set "Stripe live key — rotated by alice on 2026-05-20"
```

**There is no `reveal` command.** Per design, the value never leaves
the chassis once stored — to inspect it, rotate it (the rotate path
shows the new value once).

### Revoke

```bash
txco auth tenant secrets revoke STRIPE_API_KEY --tenant acme
```

Soft-delete: the row is marked `revoked_at = now`; the encrypted
versions stay in the DB for audit. The `(tenant, stack, name)` slot
is freed — you can immediately re-create a secret with the same
name (it gets `version_no = 1` as a fresh identity).

## 3. Reference secrets from txcl ops

In a txcl rule's `WITH` clause, reference a secret by name. The
chassis materializes the cleartext into the op handler's private
buffer at execution time; the value never enters `op.Input`,
trace events, mock fixtures, continuations, or logs.

### Templated header (the 90% case)

```txcl
EXEC "https://api.stripe.com/v1/charges"
  WITH secrets.headers.authorization.secret = "STRIPE_API_KEY",
       secrets.headers.authorization.format = "Bearer {}",
       method = "POST"
```

The `format` template has exactly one `{}` placeholder; the
materialized cleartext fills it. Bearer tokens, GitHub `token <t>`
legacy, Vendor-API custom prefixes — all fit this shape.

### Raw substitution (no format)

```txcl
EXEC "https://api.vendor.com/things"
  WITH secrets.headers.x-api-key.secret = "VENDOR_KEY"
```

### Body field

```txcl
EXEC "https://api.partner.com/auth"
  WITH secrets.body.client_secret.secret = "PARTNER_OAUTH_SECRET"
```

The path under `secrets.*` mirrors the outbound request — `headers.X`
sets HTTP header `X`; `body.X.y.z` overlays the JSON body at that
path.

### Computed (HMAC, JWT, etc.)

For computed credentials (HMAC over a body, JWT signing,
base64(`user:pass`) for Basic auth), use a separate signing op
followed by a normal request:

```txcl
# Step 1 — compute the HMAC. op.Secrets consumed here only.
EXEC "txco://hmac-sign"
  WITH secrets.key.secret = "STRIPE_WEBHOOK_SECRET",
       algorithm   = "sha256",
       input_path  = "body",
       output_path = "_txc.computed.stripe_sig"

# Step 2 — make the call. The digest is a normal envelope value.
EXEC "https://api.stripe.com/v1/webhook-callback"
  SET @web.req.headers.stripe-signature = @_txc.computed.stripe_sig
  WITH method = "POST"
```

Two computed-secret ops ship with the chassis:

- `txco://hmac-sign` — HMAC-SHA256/SHA512, hex or base64 digest.
- `txco://basic-auth-encode` — base64(`user:password`) for HTTP Basic.

Custom signing schemes: register your own op handler that reads
`op.Secrets` via `secrets.BagFromContext(ctx)`.

## 4. Capabilities

Two capabilities gate secret-store admin endpoints:

| Capability      | Permits |
|-----------------|---------|
| `secret:*:read` | List secrets, read metadata. **Never the value.** |
| `secret:*:write` | Create, generate, rotate, `rotate --generate`, describe, revoke. |

Op-time materialization (the data-plane path that fills
`op.Secrets`) is **not** gated on per-actor capabilities — it's the
chassis acting on the tenant's behalf inside the tenant's own
request scope.

## 5. Audit log

Admin actions emit structured logs at **info**. Op-time
materialization logs at **debug** (per-request frequency makes info
too noisy and risks publishing business behavior). For observability,
consume the `chassis.secret.materialize` metric — incremented per
secret reference with labels `txco.tenant.slug` and
`txco.secret.name`.

```
INFO  secret_action  actor_id=actor_abc tenant_id=tnt_xyz secret_name=STRIPE_API_KEY action=rotate outcome=ok
```

Grep tokens: `secret_action` for admin events; `secret_materialize`
for op-time (debug). Value bytes are never logged at any level.

## 6. Disaster: master-key loss

**This is the catastrophic failure mode.** If
`/data/secrets/txco-master.key` is lost or corrupted, every secret
in the store becomes opaque ciphertext that no one can decrypt.
There is no in-chassis recovery path.

### Mitigation (preventive)

1. **Back up the master-key file separately from the runtime DB.**
   Different disk. Different access controls. If the runtime DB and
   the master key are on the same backup volume, you have a single
   point of compromise — the attacker who steals the backup steals
   both.
2. **Document the file's location** in your operator-side
   disaster-recovery doc. The chassis doesn't self-document where
   the file lives; only the operator knows.
3. **Encrypt the backup at rest** with a different key/passphrase.
   GPG-encrypted to a hardware-backed key, AWS KMS, etc. The
   master key is itself a credential that protects credentials;
   layer accordingly.

### Recovery (after loss)

If the master-key file is irretrievably lost:

1. **Accept that the existing store is gone.** Encrypted ciphertexts
   in `tenant_secret_versions` are now binary noise. There is no
   way to recover the cleartexts.
2. **Plan vendor-secret rotation.** Every stored secret needs to be
   re-issued by its source: rotate Stripe key in the Stripe
   dashboard, re-mint OAuth secrets, regenerate webhook signing
   keys with each vendor, etc. This is the same exercise as any
   credential-compromise incident.
3. **Mint a new master key** with `txco auth secrets init --path
   <path>`. Restart the chassis.
4. **Truncate the abandoned ciphertexts** (or leave them — they're
   inert without the old MK; the unique-index WHERE clause
   excludes revoked rows, so re-creating with the same names just
   works):
   ```sql
   DELETE FROM tenant_secret_versions;  -- optional cleanup
   DELETE FROM tenant_secrets;           -- (or UPDATE … SET revoked_at)
   ```
5. **Re-create each secret** via `txco auth tenant secrets set …`
   with the newly-issued values from step 2.

This is operationally painful by design. The alternative — chassis
auto-backup of the master key — would create a second leak surface
elsewhere; the explicit responsibility is the better trade.

## 8. Quick reference

| Action | Command |
|---|---|
| Mint master key (explicit; rarely needed — auto-mints on first boot) | `txco auth secrets init --path /data/secrets/txco-master.key` |
| Store operator value | `txco auth tenant secrets set NAME --tenant T` (TTY prompt) |
| Mint random value | `txco auth tenant secrets generate NAME --tenant T --byte-len 32` |
| List | `txco auth tenant secrets list --tenant T` |
| Show metadata | `txco auth tenant secrets show NAME --tenant T` |
| Rotate (operator value) | `txco auth tenant secrets rotate NAME --tenant T` |
| Rotate (chassis mints) | `txco auth tenant secrets rotate NAME --tenant T --generate` |
| Update description | `txco auth tenant secrets describe NAME --set "new" --tenant T` |
| Revoke | `txco auth tenant secrets revoke NAME --tenant T` |

| Capability | Allowed actions |
|---|---|
| `secret:*:read` | list, show |
| `secret:*:write` | set, generate, rotate, `rotate --generate`, describe, revoke |
