<!-- nav: TXCL -->

# TXCL

The Thanks Computer Language (TXCL) is a domain specific language which controls when our operators will execute and what they will contribute to the next step in the flow. 

Every clause is optional. When present, clauses appear in this order:

```
[WHEN     <condition> | * ]                          # fire only when this matches
[SET      <path> = <value> [, …] ]                   # set fields before dispatch
[SELECT   * | <branch> [AS <path>] [DEFAULT <v>] … ] # project the op's input
[WITH     <key> = <value> [, …] ]                    # per-call chassis directives
[PRIORITY <int> ]                                    # tie-breaker among matches
[EXEC     "op:// | http(s):// | txco:// | ai://chat | mcp+https://" ]                 # dispatch to an operation
[EMIT     <path> = <value> [, …] ]                   # overlay onto the response (after EXEC)
```

## Example

```txcl
WHEN .tz == "ams"
SET .source = "thanks-computer"
WITH timeout = 2000
PRIORITY 5
EXEC "https://timeapi.io/api/v1/time/current/zone?timezone=Europe%2FAmsterdam"
```

## Start here

The smallest resonator emits a value into the flow's next step:

```txcl
EMIT .hello = "world"
```

Every event gets `{"hello": "world"}` merged into its envelope — no condition, no dispatch.

Dispatch to an operation with `EXEC`:

```txcl
EXEC "https://timeapi.io/api/v1/time/current/unix"
```

Forward every event verbatim to the URL; the HTTP response merges into the envelope.

Add a condition:

```txcl
WHEN .tz == "ams" 
EXEC "https://timeapi.io/api/v1/time/current/zone?timezone=Europe%2FAmsterdam"
```

Now only events whose JSON has `.tz == "ams"` reach the handler.

Add a transformation:

```txcl
WHEN .x == 1
SET .source = "thanks-computer"
EXEC "http://localhost:9000/echo"
```

The handler receives `{"x": 1, "source": "thanks-computer"}` (plus the standard envelope fields `_ts`, `_txc`). `SET` before any `SELECT` modifies the input that gets forwarded.

## Clauses

A resonator has up to seven clauses, all optional, in this order:

| Clause     | What it does                                                   |
| ---------- | -------------------------------------------------------------- |
| `WHEN`     | Fire only when this matches (`*` or omitted = always)          |
| `SET`      | Set fields on the event before dispatch                        |
| `SELECT`   | Project — `*`, or a branch list (with optional `AS`/`DEFAULT`) |
| `WITH`     | Per-call chassis directives (e.g., `timeout`)                  |
| `PRIORITY` | Tie-breaker among matches at the same stage (integer)          |
| `EXEC`     | Dispatch target — `op://`, `http(s)://`, `txco://`, `ai://chat`, `mcp+https://` |
| `EMIT`     | Overlay values onto the response, after `EXEC`                 |

## Lexical structure

### Comments

```txcl
# this is a comment to end of line
WHEN .x == 1   # trailing comments work too
```

### Whitespace

Spaces, tabs, newlines, and carriage returns are equivalent. A resonator on multiple lines is identical to the same resonator on one line.

### Keywords

Case-insensitive. Both `WHEN` and `when` work; both `SELECT` and `select`; etc.

### Strings

Double-quoted, with `\"` escape for embedded quotes:

```txcl
EXEC "http://example.com/path"
SET .name = "alice \"the great\""
```

### Numbers

Integers and floats:

```txcl
WHEN .count == 42
WHEN .ratio < 0.75
WHEN .delta == -3
```

Negatives are accepted via a leading `-` directly preceding a digit.

### Booleans and null

```txcl
WHEN .enabled == true
WHEN .draft == false
WHEN .ref == null
```

## Conditions

- `==  !=  <  <=  >  >=` (numbers, lexical strings), 
- `=~` / `!~` (regex, `/pattern/` literals), 
- `&&` and `||`, 
- prefix `!`, parentheses for grouping. 

A comma in `WHEN` is an AND.


### Branch paths

A branch is `.` followed by one or more dot-separated segments. Branches use [gjson](https://github.com/tidwall/gjson) syntax — the dotted form covers the common case:

```txcl
.x
.user.email
._txc.web.req.method
```

Segments may contain hyphens, so HTTP header keys are addressable without quoting:

```txcl
.web.res.headers.content-type.0
```

For a key that contains a character the bare run can't carry — notably a literal `.` or a space — quote that segment with `."..."`. The quote characters are not part of the key; a literal `.` inside is escaped for gjson/sjson so it stays one segment:

```txcl
.a."b.c".d          # path segments: a, "b.c", d
.headers."content-type".0
```

### Regexes

Regex literals are bounded by `/`, used only as the right-hand side of `=~` (matches) or `!~` (does not match):

```txcl
WHEN .url =~ /^https?:\/\/example\.com\//
WHEN .ua  !~ /(?i)bot|spider|crawler/
```

Forward slashes inside the pattern must be escaped as `\/`. Regex syntax is Go's `regexp` package (RE2).

### String escapes

Double-quoted strings recognize the standard escapes: `\"`, `\\`, `\n`, `\r`, `\t`. Any other `\x` sequence is left as the two literal bytes (so unknown escapes don't silently disappear).

```txcl
SET .body = "line one\nline two\n"   # contains real newline bytes
```

## Shorthand

Two pieces of sugar keep common resonators compact. Both are lexer-level rewrites — the parser, AST, and runtime see ordinary tokens, so the sugars are fully interchangeable with their long forms.

### `@foo` — `._txc.` prefix

`@foo` expands to `._txc.foo` anywhere a branch path is allowed (WHEN, SET, etc.). Use it to keep chassis-control reads and writes readable:

```txcl
WHEN @web.req.url.path == "/healthz"        # equivalent to ._txc.web.req.url.path
SET @halt = true                             # equivalent to ._txc.halt
EXEC "txco://noop"
```

A leading `@` must be followed by an identifier byte (letter or `_`). `@.foo`, bare `@`, and `@1` are lex errors.

### `b64"..."` — base64-encoded string literal

`b64"hello"` is a string literal whose value is the base64 encoding of the UTF-8 bytes of `hello` (`aGVsbG8=`). String escapes apply _before_ encoding — `b64"not found\n"` encodes a real `0x0a` newline, not the two characters `\` and `n`.

```txcl
SET @web.res.body = b64"not found\n"
# the envelope ends up with ._txc.web.res.body = "bm90IGZvdW5kCg=="
```

`b64` only acts as a typed-literal prefix when _immediately_ followed by `"` (no whitespace). Anywhere else it remains an ordinary identifier.

## Functions

Anywhere a literal or `@path` value is accepted as the right hand side of `SET`, `EMIT`, `WITH`, or `SELECT … DEFAULT`, you can also call a registered runtime function with `&name(args...)`. In `WHEN`, a function call is allowed on the right-hand side only when **every argument is a literal** — see [WHEN — filter](#when--filter) for why.

```txcl
SET .id  = &uuid()
SET .ts  = &now("rfc3339")
SET .x   = &concat("hello-", @user.name)
SET .obj = &object("a", 1, "b", &array("nested"))
```

Function calls compose — arguments can be literals, `@`-paths, or other function calls — so multi-step value computation that previously required chaining `EXEC` ops with intermediate envelope keys collapses to one nested expression:

```txcl
# Decode a base64 body and parse the result as JSON in one shot
SET @rpc = &json(&b64decode(@web.req.body))
```

### Functions vs ops

- **`&fn(...)` functions** are **side-effect-free** runtime computation. Synchronous, inline, no bus dispatch, no per-call trace span, no Unit access (no secret store, no HTTP client, no KV). Cheap, quick, composable.
- **`txco://` ops** stay the right shape for anything with **side effects or I/O** — HTTP calls, MCP egress, secret-store reads, KV writes, anything that can suspend via continuation, anything that needs to be visible as its own trace span. Ops dispatch on the bus.

The boundary is firm. The registry is chassis-shipped and curated — operators don't extend it (use a `txco://` op for that). New functions land with discipline: side-effect-free, generally useful across protocol patterns, justified by a real use case.

### Error semantics — strict vs `&try_*`

By default a function call that fails (`&json("not json")`, `&b64decode("xx!")`, `&substr("hi", 0, 99)`) **halts the resonator** — the error surfaces through the op-failure trace surface, no SET/EMIT writes are applied past the failure point. Silently dropping a "couldn't compute this value" write is a footgun (a missing field downstream looks indistinguishable from an explicit empty), so the default is loud.

Each function whose failure mode is recoverable also ships a `&try_*` sibling that returns `null` on the same failure instead of halting:

```txcl
# Strict: malformed body halts the resonator.
SET @rpc = &json(@web.req.body)

# Safe: malformed body produces null; a later WHEN can check.
SET @rpc = &try_json(@web.req.body)
```

Visible at the call site so a reader knows immediately whether failure halts or continues.

### Function registry

#### Codecs

| Function        | Signature       | Notes                                          |
| --------------- | --------------- | ---------------------------------------------- |
| `&b64encode(s)` | string → string | base64 standard encoding                       |
| `&b64decode(s)` | string → string | base64 standard decoding; errors on bad input  |
| `&urlencode(s)` | string → string | percent-encode for query strings / form values |
| `&urldecode(s)` | string → string | percent-decode; errors on malformed `%xx`      |
| `&json(s)`      | string → value  | parse string-of-JSON into an addressable value |
| `&to_json(v)`   | any → string    | serialize a value to a compact JSON string     |

#### JSON path access

For static paths, prefer the native `@a.b.c` syntax — it's shorter and reads better. Reach for `&get` / `&set` / `&has` when the path is computed at runtime or when you're walking an object held in a variable.

| Function              | Signature                    | Notes                                                                |
| --------------------- | ---------------------------- | -------------------------------------------------------------------- |
| `&get(obj, "a.b")`    | value, string → value        | gjson-path lookup; **null on missing path**, error on unwalkable obj |
| `&set(obj, "a.b", v)` | value, string, value → value | sjson-path write; returns the new value                              |
| `&has(obj, "a.b")`    | value, string → bool         | true iff path exists; distinguishes "absent" from "present-but-null" |

The path argument is a string literal (or any value that evaluates to a string), **not** an `@`-path. `&get(@rpc, "params.name")` walks INTO the value at `@rpc`; `@rpc.params.name` walks the envelope directly. Different mental models.

#### Constructors

| Function             | Signature         | Notes                          |
| -------------------- | ----------------- | ------------------------------ |
| `&object()`          | () → object       | empty `{}`                     |
| `&object("k", v, …)` | variadic → object | key-value pairs, left-to-right |
| `&array()`           | () → array        | empty `[]`                     |
| `&array(v, …)`       | variadic → array  | list of values                 |

`&object` semantics on bad inputs (all halt the resonator):

- **Odd arg count** → key without value
- **Non-string key** → object keys must be strings
- **Duplicate key** → last-wins (right-most pair)

#### Generators / time

| Function    | Signature               | Notes                                                                        |
| ----------- | ----------------------- | ---------------------------------------------------------------------------- |
| `&uuid()`   | () → string             | UUID v7 (time-ordered, lexicographically sortable)                           |
| `&now()`    | () → number             | unix seconds                                                                 |
| `&now(fmt)` | string → string\|number | formats: `"unix"` (default), `"millis"`, `"nanos"`, `"rfc3339"`, `"iso8601"` |
| `&tz(zone, "hour"\|"minute", h [, m])` | string, string, int[, int] → number | the UTC **hour** or **minute** of local wall-clock `h:m` (minute `m` defaults 0) in IANA `zone` today (DST-aware) — bridges UTC `@cron.hour`/`@cron.minute` to a local time, incl. fractional offsets like `+05:30` |

#### Strings / hashes

| Function                 | Signature                        | Notes                                                               |
| ------------------------ | -------------------------------- | ------------------------------------------------------------------- |
| `&concat(...)`           | strings → string                 | variadic; non-string args are coerced via `%v`, `nil` becomes empty |
| `&len(s)`                | string\|array\|object\|nil → int | length of string, array, or map; `nil` is 0                         |
| `&split(s, sep)`         | string, string → array           | mirrors `strings.Split`; empty sep splits into individual bytes     |
| `&join(arr, sep)`        | array, string → string           | inverse of `&split`; elements coerced like `&concat` (nil → empty); halts if `arr` isn't an array |
| `&substr(s, start, end)` | string, int, int → string        | half-open, byte-indexed; halts on out-of-range or negative indices  |
| `&repeat(s, n)`          | string, int → string             | `s` repeated `n` times (Perl `x`); `n == 0` → `""`; negative `n` halts; capped at 1 MiB |
| `&pad(s, width, fill)`   | string, int, string → string     | pad `s` to `abs(width)` bytes with `fill`; **positive `width` = left-pad** (`&pad("42",5,"0")` → `"00042"`), **negative = right-pad** (`&pad("hi",-5," ")` → `"hi   "`); `width == 0` halts; never truncates (already-wide `s` passes through); byte-measured |
| `&sha256(s)`             | string → string                  | lowercase hex digest                                                |

#### Arithmetic

All five take exactly two operands — nest calls to fold more (`&add(&add(.a, .b), .c)`). Operands coerce by representation, not semantics: **numeric strings convert** (`"300"`, `"-1.5"`, `"1e5"` — the same coercion `WHEN .t > 100` already applies), while `null`, booleans, and non-numeric strings **halt**. A result that is a whole number within ±2^53 lands as a JSON integer; otherwise as a float. A non-finite result halts — `NaN`/`Inf` would serialize as invalid JSON and corrupt the envelope.

| Function     | Signature               | Notes                                                                                                             |
| ------------ | ----------------------- | ----------------------------------------------------------------------------------------------------------------- |
| `&add(a, b)` | number, number → number | halts on `null` / bool / non-numeric-string operands (all five do)                                                 |
| `&sub(a, b)` | number, number → number | the age/window idiom: `EMIT ._age = &sub(&now("unix"), .t)`, then `WHEN ._age < 300` in a later scope              |
| `&mul(a, b)` | number, number → number | halts on overflow to a non-finite value                                                                            |
| `&div(a, b)` | number, number → number | halts on division by zero; `&div(6, 3)` → `2`, `&div(7, 2)` → `3.5` — no truncating division; pair with `&mod`     |
| `&mod(a, b)` | int, int → int          | Go semantics (result takes the dividend's sign); halts on zero divisor or fractional operands                      |

Precision ceiling: envelope numbers travel as float64, so integers beyond ±2^53 (~9.0 × 10¹⁵) have already lost exactness before any function runs — results out there stay floats rather than pretending to integer precision. Unix timestamps (seconds or millis) are comfortably inside the range.

#### Safe variants (`&try_*`)

| Strict          | Safe                | Failure mode that becomes null                                      |
| --------------- | ------------------- | ------------------------------------------------------------------- |
| `&json(s)`      | `&try_json(s)`      | malformed JSON                                                      |
| `&b64decode(s)` | `&try_b64decode(s)` | invalid base64                                                      |
| `&urldecode(s)` | `&try_urldecode(s)` | malformed percent-encoding                                          |
| `&get(obj, p)`  | `&try_get(obj, p)`  | unwalkable obj (strict `&get` already returns null on missing path) |
| `&substr(s, …)` | `&try_substr(s, …)` | out-of-range or negative indices                                    |

## Operators

| Operator | Semantics             | Allowed value types          |
| -------- | --------------------- | ---------------------------- |
| `==`     | equals                | string, int, float, bool     |
| `!=`     | not equals            | string, int, float, bool     |
| `<`      | less than             | string (lexical), int, float |
| `<=`     | less than or equal    | string, int, float           |
| `>`      | greater than          | string, int, float           |
| `>=`     | greater than or equal | string, int, float           |
| `=~`     | regex matches         | regex literal                |
| `!~`     | regex does not match  | regex literal                |

## WHEN — filter

A resonator with no `WHEN` clause fires for every input. Equivalent to `WHEN *`.

```txcl
WHEN *                        # always
WHEN .src == "cron"           # equality
WHEN .x != null               # any non-null
WHEN .priority < 5            # comparison
WHEN .url =~ /\.json$/        # regex
WHEN .src == "cron", .x == 1  # AND: all must match
```

Comma-separated conditions are conjunctive (every condition must match).

The **left side is always a path**. The right side is a literal — a string, number, bool, null, or regex — or a function call whose arguments are **all literals** (nested calls included):

```txcl
WHEN @cron.hour   == &tz("Asia/Kolkata", "hour", 9)
  && @cron.minute == &tz("Asia/Kolkata", "minute", 9)   # 09:00 IST, in one rule
```

A literal-args call is envelope-independent — a computed *constant* for that evaluation — so the predicate still compares one path against one known value. That's the line, and it's deliberate: a path argument (`&concat("", .owner)`) would turn the comparison into path-vs-path, which `WHEN` must not express — the key-shape authorization pattern (construct the lookup key from the authenticated identity instead of writing `sender == owner` checks) relies on it being impossible. Path args are a parse error.

Two more `WHEN`-specific rules: `=~` / `!~` take a regex literal only, and a call that **fails** (divide by zero, unknown zone, unregistered name) makes the condition **false** — never an error, even under `!=`. WHEN is a total filter; one bad rule must not halt every request through its scope. The cost is a silent no-match, which is why `txco lint` flags unknown function names in WHEN at authoring time.

To gate on a value computed **from the envelope**, `EMIT` it in an earlier scope and compare against a literal (`EMIT`, not `SET` — SET only shapes its own op's input view and is discarded at scope advance; EMIT merges into the envelope the next scope sees):

```txcl
# scope 100: compute
EMIT ._age = &sub(&now("unix"), ._verify.t)

# scope 200: gate
WHEN ._age < 300
```

This is deliberate, not a gap. A predicate that can't compare two paths keeps access control **key-shaped** — you construct a lookup key from the authenticated identity instead of writing `sender == owner` checks — and a predicate that can't call functions stays total: it evaluates to true or false, never halts mid-filter.

## SET — set fields

`SET` writes values onto the event before `EXEC` dispatches. Comma-separate multiple fields:

```txcl
SET .meta.tag = "scheduled"
SET .a = "x", .b = 7, .c = true
```

A value may be a string, int, float, bool, or a function call (`&uuid()`, `&json(…)` — see [Functions](#functions)).

## SELECT — projection

```txcl
SELECT *                                   # whole envelope (same as omitting SELECT)
SELECT .body, .headers                     # the op receives ONLY these branches
SELECT .body AS .payload                   # rename on the way in
SELECT @web.req.url.query.q.0 AS .q DEFAULT "fallback"   # rename + fallback
```

`SELECT` governs `input → op`: what the operation *receives*. If you don't write a `SELECT` clause, the whole input passes through. `SELECT *` is just an explicit way to spell "everything." With one or more branches, the dispatched input becomes **only the assigned destinations**, plus the standard envelope identity — `_ts`, and `_txc.op`/`_txc.step` for handler transports (`txco://`, `http(s)://`, `compute://`). Everything else — including the rest of `_txc.*` — is dropped before dispatch, which is how you keep internal envelope state out of an external service.

The pieces compose per branch:

- `SELECT .a` is shorthand for `SELECT .a AS .a`.
- `AS <dest>` renames: the op sees the value at `<dest>`.
- `DEFAULT <literal>` substitutes when the source path is **missing or the empty string**. Without a `DEFAULT`, a missing source still writes the destination as `""` — a selected key always has at-least-empty presence.
- `SELECT *` must stand alone (no mixing with a branch list).

Selected values also **persist**: each assignment is overlaid onto the rule's response (like a synthetic `EMIT`, with your own `EMIT` winning on conflict), so a `SELECT` with no `EXEC` works as a plain envelope write that merges into the scope — the "use the query param if present, else fall back" one-liner. Persistence is additive; the projection narrows only what the op receives, never what the scope keeps.

Two interactions worth knowing: `WITH` values resolve against the **full** pre-projection input, so a chassis directive may reference paths the projection drops; and `ai://chat` prompt templates render over the op's input, so a `{{@path}}` of an unselected path renders empty — select what the prompt needs.

## WITH — per-call modifiers

```txcl
WITH timeout = 1000                  # 1 second, expressed as ms
WITH timeout = "500ms"               # also valid: any time.ParseDuration string
WITH timeout = 2000, label = "v2"    # free-form key/value pairs
```

WITH carries **chassis directives about this op** — they tell the chassis how to run the op, but they don't reach the op target. `timeout` is the only key the chassis currently consumes: numeric values are treated as milliseconds; string values are parsed by Go's `time.ParseDuration` (so `"500ms"`, `"2s"`, etc. all work). Bad parse falls back to the global `--op-timeout` default (5s).

A per-op `timeout` is capped by `--op-timeout-max` (default `10m`). A resonator asking for more is rejected at dispatch with a chassis-level error log; the op is dropped from the merge for that request.

Other keys are accepted by the parser but currently unused at runtime. Reserved for future per-op config.

### `redact` and `omit` — trace-log scrubbing

```txcl
WITH redact = "user.email, user.ssn"           # mask these paths with "[REDACTED]"
WITH omit   = "_txc.lmtp.msg.attachments"      # delete these paths entirely
```

Two reserved WITH keys scrub **trace logs** only — runtime data is untouched: the resonator's own WHEN and SELECT still read the full envelope (though what the EXEC *receives* is the SELECT-projected view when the rule selects). Both take a comma-separated list of gjson dot-paths (exact match, no wildcards):

- `redact` masks each value with `"[REDACTED]"` (the field stays — use when "something was here" matters, e.g. credentials).
- `omit` deletes the path entirely (use for bulky data like attachments or raw bodies).

A path listed in both → **omit wins**. Hints are static (literal strings only; an `@path` is skipped), scoped per `(tenant, stack)` with union semantics across an EXEC jump, and picked up by `txco apply` without a restart. See [trace.md](../trace.md).

## PRIORITY — tie-breaker

```txcl
PRIORITY 2
```

A signed integer. When multiple resonators at the same stage match, but you want only the highest-priority resonator to win the selection. Default is 0. Everything of the same priority
fires in parallel.

## EXEC — dispatch target

```txcl
EXEC "op://classify"                # sandboxed nano-op (your JS/TS)
EXEC "https://api.example.com/op"   # your HTTP service (POST)
EXEC "txco://sendmail"              # chassis builtin
EXEC "ai://chat"                    # a model, via the AI registry
EXEC "mcp+https://mcp.example.com"  # a tool on an external MCP server
```

Five schemes are supported:

| Scheme | Dispatch path |
| --- | --- |
| `op://NAME` | A sandboxed WebAssembly [nano-op](../../authoring/nano-ops.md) compiled from your JS/TS, run in-process on the chassis — no service to deploy. |
| `http://` / `https://` | POSTs the envelope as JSON to the URL — the SELECT-projected view when the rule selects; the response body merges back in (`https` adds TLS). |
| `txco://NAME` | In-process chassis [builtin](../builtins.md) via `ExecCore`; the name after `txco://` is looked up in the registry (`noop`, `static`, `sendmail`, …). |
| `ai://chat` | A chat model via the chassis's AI registry — see [ai](../../ai.md). |
| `mcp+http://` / `mcp+https://` | Calls a tool on an external [MCP](../protocols/mcp.md) server (egress). |


## EMIT — response overlay

Where `SET` shapes the input before dispatch, `EMIT` overlays values onto this resonator's **response**, after `EXEC`. Pair it with `EXEC` to enrich a handler's response, or use it alone as a synthetic emitter:

```txcl
EXEC "https://api.example.com/lookup"
EMIT .checked_at = &now("rfc3339")     # add a field to the merged response

EMIT .hello = "world"                  # no EXEC — emit a value directly
```

`EMIT` also drives streaming and HTTP response control — see [Streaming the response body](#streaming-the-response-body).

## Worked examples

### Filter and forward

```txcl
WHEN .user.id =~ /^u_/
EXEC "https://hook.example.com/users"
```

### Enrich, with timeout and priority

```txcl
WHEN ._txc.src == "http"
SET .meta.source = "txco"
WITH timeout = 2000
PRIORITY 5
EXEC "https://hook.example.com/processed"
EMIT .meta.processed_at = &now("rfc3339")
```

`SET` decorates and `SELECT` projects the request before dispatch; `EMIT` adds to the response after.

### Cron heartbeat

```txcl
WHEN ._txc.src == "cron"
EXEC "txco://heartbeat"
```

Fires only on cron-tick events; routes to a local op named `heartbeat`.

## Merging responses

When `EXEC` succeeds and the target returns JSON, the response is **deep-merged** into the envelope before the next stage. Object fields are merged recursively; arrays are appended; scalars are overwritten.

```
existing: {"output":["hello"]}
response: {"output":["world"]}
result:   {"output":["hello","world"]}

existing: {"output":"hello"}
response: {"output":["world"]}
result:   {"output":["world"]}        # type change wins
```

There's no guaranteed merge order across parallel resonators at the same stage. Design your responses to compose by namespacing into distinct branches.

## Control flow via `_txc.*`

`_txc` is the chassis's internal namespace within the JSON envelope. Inlets populate it with request metadata (`_txc.src`, `_txc.rid`, `_txc.web.req.*`); ops can set it in their **response body** to direct the chassis at runtime. Control fields are read after each stage's responses are merged, then **stripped** from the envelope so they don't accumulate or leak to the user.

| Field       | Type   | Description                                                                                                                                                                                                                   |
| ----------- | ------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `_txc.halt` | bool   | If truthy on any op at the current stage, terminate the pipeline after this stage's responses are merged. Remaining stages are not run; the merged envelope (sans `_txc.halt`) is returned to the inlet.                      |
| `_txc.goto` | string | Jump to the named stage. Numeric values (`"1008"`) are interpreted as a scope within the current stack. Fully-qualified values (`"boot/foo/3"`) jump to that exact stage. Stripped from the envelope after the jump is taken. |

The convention is **transport-agnostic**: an HTTP op signals control flow by including `_txc.*` in its JSON response, exactly the same way a local `txco://` op would. Examples:

```jsonc
// HTTP responder ending the pipeline early
{ 
    "reason": "duplicate", 
    "_txc": { 
        "halt": true 
        } 
    }
```

```jsonc
// HTTP responder jumping to a different stage or stack
{ 
    "_txc": { 
        "goto": "users/signup-fast/0" 
        } 
}
```

Other `_txc.*` fields exist for things like setting the HTTP response status (`_txc.web.res.status`) — those are read by the inlet, not the pipeline. New control verbs slot in under the same namespace as needs arise. Each inlet stamps its own read-only facts there too (`@web.req.*`, `@lmtp.*`, `@imap.*`, `@dns.*`, `@websocket.*`); see [protocols](../protocols/README.md).

## Streaming the response body

For an HTTP response the chassis normally buffers the whole body and writes it once, at the end of the pipeline. To stream instead — flushing bytes to the client as the pipeline produces them — write `@web.res.body` in a **non-terminal** scope (one the pipeline continues past). Each such write is flushed to the client immediately and then cleared, so the next scope starts fresh:

```txcl
# scope 100 — open the response and flush the first piece (no @halt:
# the pipeline continues, so this body is sent as a chunk)
EMIT @web.res.status = 200,
     @web.res.headers.content-type.0 = "text/plain; charset=utf-8",
     @web.res.body = b64"first part\n"

# scope 200 — flush more as later work completes
EMIT @web.res.body = b64"second part\n"

# scope 1000 — the final piece, with @halt, ends the stream
EMIT @web.res.body = b64"done\n",
     @halt = true
```

Two consequences follow from how HTTP works, not from a chassis choice:

- **The first flushed byte locks the head.** Status and headers are captured at the first `@web.res.body` flush; setting `@web.res.status` or a header in a later scope has no effect once streaming has begun (the bytes are already on the wire).
- **Streamed responses use chunked transfer encoding** — there is no `Content-Length`.

A body written in the **terminal** scope (the common case: `@web.res.body` + `@halt` in the same `EMIT`) is **not** streamed; it's buffered and written once with a `Content-Length`, exactly as before. So nothing changes for ordinary single-body endpoints — streaming is opt-in purely by writing the body across more than one scope. Breakpoints (`--debug-breakpoints`) suppress streaming so the full envelope can still be dumped.

## Stack overlay and lanes

Stack names with slashes form a hierarchy: `website`, `website/canary`, `website/canary/eu`. When the runtime looks up resonators at `(stack, scope)` and finds nothing, it peels the trailing slash-segment and retries — `(website/canary, 500)` falls back to `(website, 500)`, then `(  , 500)`, then gives up. Storage stays sparse: an empty `website/canary` tree means _fully fall back to `website`_; a `website/canary` tree containing only scope 100 means _override 100, inherit everything else_.

Wildcard stack patterns (the boot lookup uses `boot/%`) skip the fallback walk — a wildcard already matches across stacks at one level, and peeling it would surface unrelated resonators.

To put an event into a lane, write a boot resonator that sets `_txc.goto`:

```txcl
WHEN @web.req.headers.x-canary == "1"
SET @goto = "website/canary/0"
EXEC "txco://noop"
```

(`@` is the shorthand for `._txc.` — see [Shorthand](#shorthand). Hyphenated header keys work in branch paths without quoting.)

From there, prefix fallback handles per-scope inheritance automatically. There's no `slot` column or per-lane materialization; lane _is_ the stack prefix.


## `WITH` — directives

| Key | Applies to | Meaning |
|---|---|---|
| `timeout` | any EXEC | Per-call wall clock (ms or `"2h"`); capped by `--op-timeout-max` |
| `method` | http(s) | HTTP verb override (default POST) |
| `secrets.headers.<h>.secret` / `.format` | http(s), builtins | Splice a stored secret into the request; `format = "Bearer {}"` templates it ([runbook](../runbook-secret-store.md)) |
| `secrets.body.<path>.secret` | http(s) | Same, into the JSON body (the projected body when the rule SELECTs) |
| `mode = "async"` | http(s), mcp+ | Worker acks 202 now, calls back later ([continuations](../../continuations.md)). HTTP-shaped by design — async *is* the worker-callback contract |
| `mode = "continuable"` | any EXEC | Answer synchronously if quick; promote to a continuation at the deadline. Op-level metadata, not a scheme gate — `ai://chat`, `mcp+`, `txco://`, anything |
| `continue_after` | continuable | The promotion deadline (default `--continue-after-default`, 5s) |
| `redact` / `omit` | any | Scrub paths from [trace](../trace.md) artifacts (runtime data untouched) |
| `debug = true` | any | Surface extra op debug detail to the trace |
| `prompt`, `system`, `messages`, `model`, `provider`, `schema`, `intent`, `limits.*` | ai://chat | The chat request — see [ai](../../ai.md) |

## `SET` vs `SET PRE`

"SET PRE" and "SET POST" name **positions**, not keywords — `PRE` never
appears in the syntax. A `SET` written **before** any `SELECT` (or in a
rule with no `SELECT` at all) is the PRE form: it decorates **only this
op's input** — the value never merges forward. A `SET` written **after**
the `SELECT` is the POST form: it overlays the merged envelope after the
scope completes (set-if-absent). Use the PRE form for scratch values a
prompt template or handler needs once:

```txcl
SET @body_text = .ticket.description
WITH prompt = "Summarize: {{@body_text}}"
EXEC "ai://chat"
```

Ordering with the other input-shaping clauses: pre-`SELECT` `SET`
decoration runs first, `WITH` values resolve next (against that
decorated, still-full input), and `SELECT` projection runs last, just
before dispatch. So a `SET` value is visible to `WITH` and usable as a
`SELECT` source, but it reaches the dispatched input only if the rule
selects it (or has no `SELECT` at all).

