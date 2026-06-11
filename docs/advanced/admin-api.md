# Admin HTTP API

A second HTTP server on `--admin-addr` (default `:8081`) is the only
network surface that mutates chassis state — rules, tenants, hostnames,
actors. It also serves the admin UI at `/admin/`. Front it with TLS via
your reverse proxy of choice.

The `txco` CLI is the normal client (`txco apply`, `txco auth …`).
Direct API use is supported for CI and custom tooling. Endpoints are
**tenant-scoped**: resources live under `/v1/tenants/{tenant}/…`.

**Note**: the API only sees concrete state. `op://NAME` symbolic
references are resolved client-side by `txco apply` (which also uploads
the compiled wasm to the computes endpoint) before anything is POSTed.

## Authentication

`--auth-mode` is one of three values (default `both`):

- `signed` — every request must carry RFC 9421 signature headers.
- `basic` — HTTP basic auth via `--admin-user` / `--admin-pass`.
- `both` — accept either; signed callers get their registered actor
  identity, basic callers get a synthetic `admin:all` context.

With `both` and *neither* basic credentials *nor* enrolled signing
keys, the chassis runs **open-dev**: requests are admitted with an
`admin:all` context and `source: "open"`. Local development only.

### Signed requests

Signed requests carry three signature headers, produced by the CLI
(`chassis/cli/client/signing.go`):

| Header            | Value                                                                 |
| ----------------- | --------------------------------------------------------------------- |
| `Content-Digest`  | `sha-256=:<base64-sha256-of-body>:` (empty-body digest pinned for GETs) |
| `Signature-Input` | `sig1=("@method" "@path" "@query" "@authority" "content-digest");keyid="key_…";alg="ed25519";created=<unix>;nonce="…"` |
| `Signature`       | `sig1=:<base64-ed25519-signature>:`                                   |

Server-side policy:

- Covered components are fixed: `@method @path @query @authority
  content-digest`.
- `created` must be within ±5 minutes of the chassis clock (default
  skew window).
- `(key_id, nonce)` must not have been seen within the last 10 minutes
  (replay protection).
- The public key for `key_id` must exist in the actor registry and not
  be revoked.

### Bootstrapping the first key

`POST /auth/dev/enroll` exchanges a shared secret (sent as the
`X-Txco-Enroll-Secret` header) for an actor + key pair:

- **Auto-bootstrap (default).** With no `--auth-dev-enroll-secret` and
  an empty `actors` table, the chassis generates a 4-word secret at
  boot and prints it in a `WARN` line. Single-use: once any actor is
  enrolled the endpoint returns 404, even to the original secret.
- **Explicit secret.** `--auth-dev-enroll-secret=<s>` (or
  `TXCO_AUTH_DEV_ENROLL_SECRET`) stays valid for multiple enrolments;
  the operator manages its lifecycle.

```sh
curl -sS -X POST http://localhost:8081/auth/dev/enroll \
  -H 'X-Txco-Enroll-Secret: my-dev-secret' \
  -H 'Content-Type: application/json' \
  -d '{"public_key_b64":"<base64-ed25519-pubkey>","algorithm":"ed25519","label":"laptop","kind":"human"}'
# → {"actor_id":"actor_…","key_id":"key_…","capabilities":["admin:all"]}
```

In normal workflows the CLI does this: `txco auth bootstrap-local`.

### Teammates: invitations

After the first admin, teammates onboard via invitation tokens.
Create/list/revoke are tenant-scoped; consume is global and unsigned:

| Method · Path | Auth |
|---|---|
| `POST /v1/tenants/{t}/auth/invitations` | signed (`actor:invite`) |
| `GET /v1/tenants/{t}/auth/invitations` | signed (`actor:read`) |
| `POST /v1/tenants/{t}/auth/invitations/{id}/revoke` | signed (`actor:revoke`) |
| `POST /auth/invitations/consume` | unsigned — `{token, public_key_b64, algorithm, …}` |

Tokens are word-list strings (≥96 bits), stored only as a SHA-256
hash, single-use (conditional-update consume), TTL 24h by default and
capped at 7d. Expired, revoked, consumed, and unknown tokens all return
the same `401 invalid_token` — callers can't probe one state to learn
another.

## Endpoint map

Always unauthenticated: `GET /healthz` (returns `ok`),
`POST /auth/dev/enroll`, `POST /auth/invitations/consume`. Everything
else goes through auth middleware.

Global:

| Endpoint | What |
|---|---|
| `GET /auth/whoami` | Echo the caller's auth context (`source`: `signed` \| `basic` \| `open`) |
| `POST /auth/keys/{keyID}/revoke` | Revoke a key (`actor:revoke`) |
| `GET·DELETE /auth/browser/session` | Browser-session introspection / logout (admin UI) |
| `GET·POST /v1/tenants` | List / create tenants |

Tenant-scoped, under `/v1/tenants/{tenant}`:

| Endpoint | What |
|---|---|
| `GET /ops` | List the tenant's compiled rules |
| `GET /stacks` · `GET /stacks/{name}` | List stacks / stack detail |
| `POST /stacks/{name}/draft` | Open a new draft version |
| `PUT·PATCH·DELETE /stacks/{name}/versions/{n}/files` | Edit the draft's files |
| `POST /stacks/{name}/versions/{n}/validate` | Parse-check a version |
| `POST /stacks/{name}/activate` | Atomic pointer flip to a version |
| `GET /stacks/{name}/versions` · `/diff` | History / compare |
| `PUT·HEAD /computes/{alg}/{digest}` | Upload / probe content-addressed wasm |
| `GET·POST /hostnames` · `DELETE /hostnames/{h}` | Hostname bindings ([ingress.md](./protocols/routing.md)) |
| `POST /hostnames/{h}/attach` · `/challenges` | Bind to a stack / start ownership verification |
| `GET·POST /auth/members` · `DELETE /auth/members/{actor}` | Tenant membership |
| `GET /auth/actors` · `POST /auth/actors/{id}/revoke` | Actor list / revoke |
| `GET /traces/requests.json` · `/requests/{rid}.json` · `/traces/stream` | Trace list / detail / live stream ([trace.md](./trace.md)) |

Also present: `POST /v1/cli` (the admin UI's command bridge),
`POST /v1/fleet/resync`, and `GET·PUT /v1/dns/config`.

### Stack versions, not rule imports

Rules change through versioned stacks: open a **draft**, put files,
**validate**, then **activate** — an atomic pointer flip with full
history (`/versions`, `/diff`), which is also the rollback path
(activate a prior version). `txco apply` drives this flow for you.

The pre-tenancy endpoints (`GET /v1/ops`, `POST /v1/ops/import`, and
the global actor/invitation routes) are retired: they return
`410 route_retired` with a JSON pointer to the tenant-scoped
replacement.
