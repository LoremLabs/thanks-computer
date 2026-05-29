# Admin HTTP API

A second HTTP server on `--admin-addr` (default `:8081`) is the only network surface that mutates the `ops` table. Front it with TLS via Caddy or your reverse proxy of choice; basic auth is the only built-in authentication.

The `txco` CLI talks to these endpoints — see [cli.md](./cli.md). Direct API use is supported for CI scripts, custom tooling, or replacing the CLI entirely.

**Note**: the admin API only sees concrete URLs in rule txcl. The `op://NAME` symbolic references documented in [cli.md](./cli.md#operation-references--exec-opname) are resolved client-side by `txco apply` before the bundle is POSTed. Likewise, `mock: deny` policy strips `mock_res` from the bundle on the client; the chassis itself doesn't know which target is dispatching.

## Authentication

The chassis supports three modes, selected with `--auth-mode`:

- `signed` (production default) — every request must carry RFC 9421 signature headers. See [auth.md](./auth.md) for the developer flow.
- `basic` — HTTP basic auth using `--admin-user` / `--admin-pass`. Legacy mode kept for migration.
- `open` — no auth; the server logs a WARN at boot. Local development only.

### Signed requests

Signed requests carry four headers; the CLI produces them via `txco auth` and `chassis/cli/client/signing.go`:

| Header             | Value                                                                                                |
|--------------------|------------------------------------------------------------------------------------------------------|
| `Content-Digest`   | `sha-256=:<base64-sha256-of-body>:` (empty-body digest pinned for GETs)                              |
| `Signature-Input`  | `sig1=("@method" "@target-uri" "@authority" "content-digest");keyid="key_…";alg="ed25519";created=<unix>;nonce="<22-char-base64>"` |
| `Signature`        | `sig1=:<base64-ed25519-signature>:`                                                                  |

Server-side policy:

- Covered components are fixed (`@method @target-uri @authority content-digest`).
- `created` must be within ±5 minutes of the chassis clock.
- `(key_id, nonce)` must not have been seen in the last 10 minutes.
- The public key for `key_id` must exist in the actor registry and not be revoked.

The implementation wraps [yaronf/httpsign](https://github.com/yaronf/httpsign) — see `chassis/auth/signature/`.

### Bootstrapping a signing key

`POST /auth/dev/enroll` exchanges a shared secret for an actor + key pair. Two modes share this endpoint:

- **Auto-bootstrap (default).** If the chassis is started with no `--auth-dev-enroll-secret` and the `actors` table is empty, it generates a 4-word secret at boot and prints it in a `WARN` line (`secret=<word-word-word-word>`). That secret is single-use: once any actor is enrolled, this endpoint returns 404 to all callers, even those replaying the original secret.
- **Explicit secret.** If `--auth-dev-enroll-secret=<s>` (or `TXCO_AUTH_DEV_ENROLL_SECRET=<s>`) is set, that value is used instead and stays valid for multiple enrolments — the operator manages its lifecycle.

Both modes log a startup `WARN`. Auto-bootstrap is environment-agnostic (works in `--env=prod` too — the safety boundary is the empty-registry state, not the env name).

Request:

```sh
curl -sS -X POST http://localhost:8081/auth/dev/enroll \
  -H 'X-Txco-Enroll-Secret: my-dev-secret' \
  -H 'Content-Type: application/json' \
  -d '{"public_key_b64":"<base64-ed25519-pubkey>","algorithm":"ed25519","label":"laptop","kind":"human"}'
```

Response (200):

```json
{"actor_id":"actor_01HV…","key_id":"key_01HV…","capabilities":["admin:all"]}
```

In normal workflows the CLI runs this for you: `txco auth bootstrap-local --secret <s>`.

### Whoami

`GET /auth/whoami` echoes the caller's auth context. Useful when verifying that signing wiring is correct.

```json
{"source":"signed","actor_id":"actor_01HV…","key_id":"key_01HV…","capabilities":["admin:all"]}
```

`source` is one of `signed`, `basic`, or `open`.

### Revocation

`POST /auth/keys/<keyID>/revoke` and `POST /auth/actors/<actorID>/revoke` mark keys (or all of an actor's keys) revoked. Requires `actor:revoke` capability. Future requests with the revoked key fail with `401 revoked_key`.

```json
{"revoked":true,"key_id":"key_01HV…"}
```

### Invitations

Once a first admin is enrolled, additional teammates onboard via invitation tokens. Four endpoints:

| Method · Path | Auth | Body / response |
|---|---|---|
| `POST /auth/invitations` | signed (`actor:invite`) | req `{label?, kind?, ttl_seconds?}` · res `{invitation_id, token, expires_at}` |
| `GET /auth/invitations` | signed (`actor:read`) | res `{invitations: [...]}` with derived `status` per row |
| `POST /auth/invitations/{id}/revoke` | signed (`actor:revoke`) | res `{revoked: true, invitation_id}` |
| `POST /auth/invitations/consume` | unsigned | req `{token, public_key_b64, algorithm, label?, kind?}` · res `{actor_id, key_id, capabilities}` |

Implementation notes:

- **Token entropy**: ≥96 bits (8 words from the EFF long wordlist, hyphen-joined). Stored as `hex(sha-256(token))` — the raw token is returned exactly once when the invitation is minted and is never recoverable from the DB.
- **Single-use**: the consume handler runs a `BEGIN IMMEDIATE` transaction and a conditional `UPDATE … WHERE consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?` — only the first concurrent caller sees `RowsAffected == 1` and proceeds to mint the actor + key + capability rows. Anyone else gets a `401 invalid_token`.
- **TTL**: defaults to 24h, server caps at 7d. The clamp happens in the create handler.
- **Capabilities**: v1 always issues `["admin:all"]`. The `capabilities` column on `actor_invitations` is `NOT NULL` and JSON-encoded, so future fine-grained roles materialise without a schema change.
- **Opaque rejections**: all of "expired", "revoked", "consumed", and "unknown token" return the same `401 invalid_token` body. A caller can't probe one to learn about another. (The unified-401 choice: the token IS the credential on an otherwise-public endpoint, so a bad token is fundamentally an auth failure, not a missing-resource one.)

```sh
# Mint:
curl -sS -X POST http://localhost:8081/auth/invitations \
  -H 'Signature-Input: …' -H 'Signature: …' -H 'Content-Digest: …' \
  -H 'Content-Type: application/json' \
  -d '{"label":"alice","ttl_seconds":3600}'
# → {"invitation_id":"inv_…","token":"word-word-…","expires_at":"…"}

# Redeem (unsigned):
curl -sS -X POST http://localhost:8081/auth/invitations/consume \
  -H 'Content-Type: application/json' \
  -d '{"token":"word-word-…","public_key_b64":"…","algorithm":"ed25519"}'
# → {"actor_id":"actor_…","key_id":"key_…","capabilities":["admin:all"]}
```

`/healthz`, `/auth/dev/enroll`, and `/auth/invitations/consume` are always unauthenticated. Everything else requires signed (or basic, in mixed-mode) auth.

## Endpoints

### `GET /healthz`

Liveness probe. Returns `200 OK` with body `ok\n` when the server is running. No JSON, no auth.

```sh
curl http://localhost:8081/healthz
# ok
```

### `GET /v1/ops`

List rules in the `ops` table. Optional `?stack=<prefix>` filter matches the exact stack and any descendants (`prefix/...`).

```sh
curl -u alice:secret 'http://localhost:8081/v1/ops?stack=website'
```

Response:

```json
{
  "ops": [
    {"stack": "website", "scope": 100, "name": "main", "txcl": "EXEC \"http://...\""},
    {"stack": "website", "scope": 100, "name": "audit", "txcl": "EXEC \"http://...\""},
    {"stack": "website/canary", "scope": 100, "name": "main", "txcl": "EXEC \"http://...\"", "mock_req": "{\"x\":1}"}
  ]
}
```

Rules are ordered by `(stack, scope, name, txcl)`. `mock_req` and `mock_res` are present only when populated. Multiple rules per `(stack, scope)` are first-class — they run in parallel at that stage.

### `POST /v1/ops/import`

Apply a bundle of rules. Identity is `(stack, scope, name)`: a rule at the same identity whose body matches verbatim is skipped (idempotent); anything else replaces the existing rule. Rules at *other* identities — including different names at the same `(stack, scope)` — are untouched. Each replace writes an entry to `op_revisions`.

`name` is required on every rule (the CLI derives it from the source filename, e.g. `OPS/website/100/audit.txcl` → name `audit`). Rules with empty `name` are rejected with 400.

Request body:

```json
{
  "ops": [
    {"stack": "website", "scope": 100, "name": "main", "txcl": "EXEC \"http://example.com/web\""},
    {"stack": "website", "scope": 100, "name": "audit", "txcl": "EXEC \"http://example.com/audit\""},
    {"stack": "website/canary", "scope": 100, "name": "main", "txcl": "EXEC \"http://example.com/canary\"", "mock_req": "{}", "mock_res": "{}"}
  ]
}
```

Response (200):

```json
{
  "applied": 1,
  "skipped_unchanged": 1,
  "revisions": [42]
}
```

`revisions` lists the `rev_id` values written to `op_revisions` for the *applied* rules (not the skipped ones).

The whole import is one SQLite transaction. Any txcl that fails to parse rejects the entire bundle (400 Bad Request) and rolls back; the server state is unchanged.

Error response (400):

```json
{
  "error": "txcl parse error",
  "detail": {
    "index": 1,
    "stack": "website",
    "scope": 200,
    "err": "exec missing execname"
  }
}
```

## ops schema (with name column)

Migration `0004_op_name.sql` added a `name` column for CLI-driven rule identity:

```sql
ALTER TABLE ops ADD COLUMN name TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX ops_stack_scope_name_idx
  ON ops (stack, scope, name)
  WHERE name != '';
```

The original `UNIQUE(stack, scope, txcl)` constraint stays — it dedupes by content. The new partial index dedupes named rules by `(stack, scope, name)`. Pre-existing rows with `name = ''` are unaffected by the partial index and remain valid; they just can't be upserted via the admin API (which requires a non-empty name).

## op_revisions

Schema (added by migration `0003_op_revisions.sql`):

```sql
CREATE TABLE op_revisions (
  rev_id      INTEGER PRIMARY KEY AUTOINCREMENT,
  stack       TEXT NOT NULL,
  scope       INTEGER NOT NULL,
  txcl        TEXT,
  mock_req    TEXT,
  mock_res    TEXT,
  applied_at  TEXT NOT NULL,        -- RFC3339
  applied_by  TEXT,                 -- basic-auth user, "" if no-auth
  deleted     INTEGER NOT NULL DEFAULT 0
);
```

Append-only audit trail. Every successful apply that changes a rule writes one row. `deleted=1` is reserved for a future `apply --prune` flow; v1 never sets it.

A future `txco rollback` will read the previous non-deleted revision at a `(stack, scope)` and replay it through `/v1/ops/import`.

## What's not v1

- Per-rule delete endpoint
- `apply --prune` reconciliation (delete server-side rules absent from a local tree)
- `promote` / `rollback` verbs
- Pagination / streaming for `/v1/ops` (current implementation buffers the whole list)
- Bearer tokens / mTLS / OIDC

## See also

- [cli.md](./cli.md) — the `txco` CLI that drives this API
- [overview.md](./overview.md) — the chassis runtime model
