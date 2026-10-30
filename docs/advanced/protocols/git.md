# Git — a repository per thing, pushed to by a stock client

The `git` personality is a git smart-HTTP server on the web head. A tenant's
repositories live at `https://<host>/git/<repo>.git`, on every hostname the
head serves for that tenant; a developer clones, edits, commits and pushes
with the git they have. Nothing is installed, no repository is materialised
on disk: the objects live in the chassis's content-addressed store
(`filecas`, the store that holds stack `FILES/` and blobs), their ids and
the refs in a small index (`chassis/gitstore`).

The head knows repositories, refs and the files a push changes, and
nothing about what a repository holds. What a push MEANS is a stack's:
before any ref moves the head asks the tenant's `_git` stack (the **answer**
lane, default deny), and after the reply it tells it (the **observe** lane)
— the two lanes the contacts and calendar heads have. A stack reads and
writes repositories itself with `txco://repo/*`, one commit per logical
change, so nobody but a developer needs git.

What it is for: configuration a developer edits as files and a stack
compiles — an agent's instructions, skills, policies. What it is not: a
general-purpose git host. There is no web UI, no pull requests, no LFS, no
submodules, no SSH, no protocol v2 (clients fall back to v0 on their own),
and SHA-1 object ids only (a `--object-format=sha256` repository fails on
the client's side).

## Turn it on

`txco dev --git` serves `http://<dev host>:<web port>/git/<repo>.git` with
Basic auth allowed over plaintext and the index at `.txco/dev/git.db`. On
a deployment add `git` to `--personalities` (with `web`); the head answers
over TLS only, unless `--git-insecure-auth`.

```
txco serve --personalities cron,tcp,web,admin,git
```

The index backend is `--git-store` (`sqlite`, a file at `--git-db-path`;
the hosted build registers `postgres`, shared across nodes, so an op on one
node writes what the head on another serves). The objects' content goes
wherever `--filecas-store` goes.

## Repositories: `txco://repo/create`

A stack makes a repository of the tenant it runs in:

```
WITH name      = "paris",
     principal = "pony:paris",
     policy    = &object("push", "stack"),
     into      = "_repo"
EXEC "txco://repo/create"
```

- `name` is the path segment a client clones (`/git/paris.git`) and the
  instance of the credential scopes (`git:paris:fetch`, `git:paris:push`):
  `[A-Za-z0-9_][A-Za-z0-9._-]{0,63}`, no `/`.
- `principal` is who the repository is for — the identity the application
  issues credentials to. It is recorded, not enforced at the door (see
  Access).
- `policy.push` is `stack` (default: a push lands only when the tenant's
  `_git` stack says so) or `open` (no lane).
- `quotas.{max_object_bytes, max_pack_bytes, max_repo_bytes, max_objects}`
  override the head's caps for this repository.

The create is idempotent: the same name again answers the existing row with
`created: false`. The answer: `{id, name, created, principal, created_by,
policy.push, head}`. `txco://repo/remove WITH name` deletes the index rows
(the objects' content stays, content-addressed and shared); only the stack
that made the repository may remove it.

## Access: a credential's scopes, and the tenant

A client signs in with HTTP Basic: the username is an address the chassis
bound to a principal (`paris@pony.example.com` → `pony:paris`, the binding
the mail, calendar, contacts and drive accounts make), the password a
credential of that principal (`txco://credential/create`) whose scopes
cover the door:

| Operation | Scope the credential must cover |
|---|---|
| clone, fetch, `ls-remote` | `git:<repo>:fetch` |
| push | `git:<repo>:push` |

```
WITH principal      = "pony:paris",
     scopes         = &array("git:paris:fetch", "git:paris:push"),
     label          = "ranch:git",
     password_style = "words",
     into           = "_cred"
EXEC "txco://credential/create"
```

Access is the credential's scopes and the hostname's tenant, nothing else:
the head never compares the signed-in principal with the repository's
`principal`, so a team member, another agent or a CI system holding a
`git:<repo>:push` credential pushes the same way the owner does. Who is
ISSUED such a credential is the application's policy. Ownership
(`principal`, `created_by`) matters to the administrative ops alone.

A wrong password, an unknown user, the right password for another door and
a disabled user all answer `401` the same way; a repository the tenant does
not have answers `404` only to a credential that could have fetched it.
Logins go through the resolver the IMAP, CalDAV, CardDAV and WebDAV heads
share, with its cache and `--login-rate` throttle.

## A push, step by step

1. The commands (`refs/heads/<branch>` only; `refs/txco/*` is read-only
   and `refs/tags/*` is not served; the default branch cannot be deleted).
2. The pack, read into a staging area that resolves deltas against the
   repository (git sends thin packs by default), refused past
   `--git-max-pack-bytes`, `--git-max-objects` (read from the pack header
   before anything is inflated) and `--git-max-object-bytes`.
3. A reachability walk from every new tip: a pack may omit objects, and
   what the walk finds is exactly what is kept.
4. What each command changes, old tree → new tree, with the head's own
   rules: text only by default (`--git-text-only`: a blob that is not valid
   UTF-8 or holds a NUL, a symlink, an executable or a submodule is refused,
   naming the path) and the object cap. A repository whose content policy
   is `stack` (below) keeps the mode rules and hands the text-or-binary
   decision to the lane, which reads each change's `text` and `type`.
   Deleting is always allowed.
5. The objects are stored and the response HEADERS are sent, so a front
   proxy's time-to-headers never counts what follows.
6. **The answer lane**: the tenant's `_git` stack judges the push (policy
   `stack`), within `--git-resp-timeout` (10 s). Absent, late or `ok:
   false` ⇒ refused.
7. The refs move, all of them or none, by compare-and-swap against what
   the client said they were (`atomic` is advertised, so `git push
   --atomic` works and every push behaves that way).
8. The report: `ok <ref>` or `ng <ref> <reason>`, one line each, which git
   prints as `! [remote rejected] main -> main (<reason>)`.
9. **The observe lane**: an accepted push is dispatched into `_git` after
   the reply, fire-and-forget (`--git-observe-sample`,
   `--git-observe-max-inflight`).

A refused push leaves its objects in the store (content-addressed,
harmless; the same bytes are re-pushed after a fix) and moves nothing.
Every move writes an audit row: the ref, from, to, the principal and
credential (a push) or the stack (an op).

## What `_git` hears

Both lanes dispatch the same envelope into `_git/0`, `@src == "git"`:

```json
{
  "_txc": {
    "src": "git",
    "git": {
      "tenant": "default",
      "repo": "paris",
      "phase": "answer",
      "pusher": {"principal": "pony:paris", "credential": "crd_…"},
      "ref": "refs/heads/main", "old": "0000…", "new": "4f2e…", "op": "create",
      "changes": [{"path": "config/MISSION.md", "oid": "…", "size": 221, "mode": "0100644", "op": "add"}],
      "commands": [{"ref": "refs/heads/main", "old": "…", "new": "…", "op": "create", "changes": [ … ]}],
      "commits": [{"oid": "4f2e…", "parents": [], "author": "Dev", "email": "dev@…", "when": "…", "message": "first\n"}],
      "commits_total": 1,
      "node": "web-1"
    },
    "client": {"ip": "203.0.113.9"}
  }
}
```

`ref`/`old`/`new`/`op`/`changes` repeat the first command for the one-ref
push every developer makes; `commands` has them all. `commits` lists the new
commits (at most 100; `commits_total` counts them). The `_txc.git.*` facts
are read-only for the stack; `commands`, `changes` and `commits` may be
deleted (`EMIT @delete`) once consumed. The run acts as the pusher
(`_txc.principal`).

### Answering (`@git.phase == "answer"`)

```
WHEN @src == "git" && @git.phase == "answer" && ._check.ok == true
  EMIT @git.res.ok = true,
       @halt       = true

WHEN @src == "git" && @git.phase == "answer" && ._check.ok != true
  EMIT @git.res.ok   = false,
       @git.res.code = "cannot",
       @git.res.msg  = ._check.msg,
       @halt         = true
```

`@git.res` is the one author-writable path under `@git.*`. `ok` must be
JSON `true`; `msg` is the one line the developer reads; `code` is `cannot`,
`limit` or `unavailable` (informational). A stack that wants the files at
the new commit reads them with `txco://repo/read WITH name = @git.repo,
commit = @git.new`. A repository with policy `stack` and no active `_git`
stack refuses every push ("no _git stack to decide this push").

The observe phase (`@git.phase == "observe"`) carries the same facts after
the push landed; a rule there typically routes into the application's own
stack (`SET @route.stack = "loop", @route.to = "loop/100"; EXEC
"txco://route"`).

## Content policy

Each repository has `policy.content`, set at `repo/create` or changed with
`repo/policy`:

- `text` (the default): the head refuses a blob that is not text, as
  `--git-text-only` says. Configuration is text, and the secret scan a
  stack runs reads text.
- `stack`: the head stops refusing binaries and tells the tenant's `_git`
  stack what each changed file is — `text` (UTF-8 without a NUL) and
  `type` (the media type its first 512 bytes sniff as: `image/png`,
  `application/pdf`, `font/woff2`, `application/zip`, `application/
  octet-stream` when nothing is recognized) on every entry of
  `@git.changes`. The stack refuses what it does not want, by path and by
  type, in the answer lane. A symlink, an executable and a submodule are
  still refused by the head whatever the policy. `stack` needs
  `policy.push: stack`: without a lane nobody would judge, so the pair is
  refused.

An op's `repo/commit` follows the same policy: under `text` a write that
is not text is refused; under `stack` the calling stack applies its own
check before it writes, and a write may carry bytes as `encoding:
"base64"`.

## Reading and writing without git: `txco://repo/*`

| Op | WITH | Answer |
|---|---|---|
| `repo/refs` | `name` | `{head, main, active, refs: [{name, oid}]}` — `main` is the head branch's id, `active` is `refs/txco/active`, "" when absent |
| `repo/read` | `name`, `ref` (default the head branch; `active` for `refs/txco/active`) or `commit`, `prefix`, `paths`, `max_bytes` (1 MiB default, 8 MiB ceiling), `binary` (`skip`, the default: a binary is listed without content and counted; `list`: listed and counted toward nothing; `base64`: its bytes as `content_base64`, counted) | `{found, commit, tree, over_limit, files: [{path, size, mode, oid, content \| (binary: true, type, content_base64?)}]}`; `found: false` for an empty repository |
| `repo/commit` | `name`, `ref` (a branch), `message`, `writes: [{path, content, encoding?: "base64"}]`, `deletes: [path]`, `author: {name, email}`, `expected` (the ref's current id; "" = must not exist), `base` (the parent for a new ref), `tree` (a commit whose tree to copy: a rollback) | `{commit, tree, ref, old, changed}`; `unchanged: true` when nothing differed; `txco_repo_moved` with `current` when `expected` is stale — the lost-update guard |
| `repo/log` | `name`, `ref` or `commit`, `limit` (≤ 100) | `{found, commits: [{commit, parents, tree, author, when, message}]}` first-parent, newest first |
| `repo/diff` | `name`, `base` (empty = the empty tree), `head`, `paths`, `max_bytes` | `{head, changes: [{path, op, from, to, patch}]}` with unified patches for text files under the budget |
| `repo/ref/update` | `name`, `ref` (any `refs/…`, `refs/txco/*` included), `old`, `new` ("" = create / delete) | `{ok, ref, old, new}`; `txco_repo_moved` with `current` otherwise |
| `repo/policy` | `name`, `policy: {push?, content?}` (a field left out stays) | `{name, policy: {push, content}, changed}`; `txco_repo_invalid_arg` for `content: stack` without `push: stack` |

Every error is `<into>.error.{code, message}` with the codes
`txco_repo_disabled`, `txco_repo_no_tenant`, `txco_repo_invalid_arg`,
`txco_repo_not_found`, `txco_repo_forbidden`, `txco_repo_moved`,
`txco_repo_too_large`, `txco_repo_store`. Any stack of the tenant may read
and write any of its repositories; the lanes are not run for an op's
commit (the application wrote it). `ref` and `commit` arguments take a
40-hex id, a full `refs/…` name, or the short names `main` and `active`.

The pattern the ops are for: a push moves `refs/heads/main`; the
application, on the observe lane and by its own policy, moves
`refs/txco/active` with `repo/ref/update` and compiles from it; a UI or an
agent writes with `repo/commit` guarded by `expected`; a rollback is a new
commit on `main` whose `tree` is an older commit's, then `active` follows.
One journal, several doors, and `git log` is the audit a developer already
knows how to read.

## Limits

| Flag | Default | What |
|---|---|---|
| `--git-max-pack-bytes` | 16 MiB | one request body |
| `--git-max-object-bytes` | 1 MiB | one object (a file) |
| `--git-max-objects` | 5000 | objects in one push |
| `--git-max-repo-bytes` | 64 MiB | a repository's stored objects |
| `--git-text-only` | true | refuse non-text blobs (unless the repository's content policy is `stack`), symlinks, executables, submodules |
| `--git-resp-timeout` | 10 s | the answer lane's wait |

There is no garbage collection: objects no ref reaches stay indexed, as the
blob plane's do. Keep repositories small — these caps assume configuration,
not source trees.

## Client notes

- Any git ≥ 2.x works; the protocol v2 the client asks for first is not
  advertised and it falls back to v0 unaided.
- A credential helper (`git config credential.helper`) remembers the
  password; the username is the bound address.
- `git fetch origin refs/txco/active` shows what the application activated.
- `git push --atomic` is honoured; every push is all-or-nothing anyway.
- `git clone --depth` fails: shallow clones are not served.
