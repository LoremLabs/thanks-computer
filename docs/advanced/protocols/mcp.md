# MCP — Use MCP tools in operations

_MCP (Model Context Protocol) is how AI agents and tool servers talk.
The chassis speaks it outbound today: an [operation](../../ops.md) can call a tool on any
MCP server with one `EXEC`._

```txcl
WHEN .question != ""
WITH mode = "async"
EXEC "mcp+https://mcp.deepwiki.com/mcp#ask_question"
```

The URL is the MCP server endpoint; the `#fragment` names the tool.
The op's input becomes the tool call's arguments; the tool's result
merges into the document like any other op. Because agent tools are
often slow, `WITH mode = "async"` and `"continuable"` both work —
a long-running tool is just a [continuation](../../continuations.md).

Working example: `examples/mcp-quickstart/` (rules calling DeepWiki).
The `txco demo` curriculum has a live MCP track.

## A server and a tool from data

The literal `EXEC` names the server and the tool. When an op must call a
server the envelope names, `WITH` supplies either, and the literal only
picks the scheme:

```txcl
WHEN ._call.ok == true
  WITH url       = ._call.url,      # http(s)://… (or mcp+http(s)://…); a #fragment names the tool
       tool      = ._call.tool,     # wins over any #fragment
       args_path = "_call.args",    # the one envelope value sent as the tool's arguments
       into      = "_res",          # nest the answer here
       timeout   = 30000
  EXEC "mcp+https://invalid/"
```

| `WITH` | |
|---|---|
| `url` | The endpoint, in place of the literal's. It must be `http(s)` underneath, so it cannot leave the egress-guarded client. |
| `tool` | The tool's name, in place of the `#fragment`. |
| `args_path` | The envelope path whose value (an object) is the call's `arguments`, exactly: none of the envelope's other keys reach the server. A path with nothing at it, or one that is not an object, fails the op and sends nothing. Without it the arguments are the op's input minus `_txc` and `_ts`. |
| `into` | Nests the answer under that key, so two calls merge side by side. It also changes how a failure is answered: see below. |
| `method` | `tools/call` (the default) or `tools/list`. |

## Listing a server's tools

```txcl
WHEN ._want_tools == true
  WITH url = ._server.url, method = "tools/list", into = "_tools"
  EXEC "mcp+https://invalid/"
```

The answer is `{tools: [{name, description, inputSchema, annotations, …}]}`,
each tool as the server wrote it. The chassis follows the server's cursor
for up to 20 pages and 200 tools, and says `truncated: true` when the
server had more. A tool's `description` and `annotations` are the server's
own words about itself: treat them as data from a stranger.

## The answer

| The tool's result | The op's answer |
|---|---|
| one text block | `{text: "…"}` |
| several blocks, or a block that is not text | `{content: [...]}` |
| `structuredContent` | kept beside it, under that name |
| `isError: true` (the tool says it failed) | `isError: true` beside the text; the op still succeeded |

## When a call fails

Without `WITH into`, a failed call is a failed op: its output is dropped
and the trace says why.

With `WITH into`, a failure at run time is **data**, and the op succeeds, so
a later op can say what went wrong:

```txcl
WHEN ._res.error.code == "txco_mcp_http" && ._res.error.status == 401
  EMIT ._reply = "that server wants a sign-in"
```

| `<into>.error.code` | |
|---|---|
| `txco_mcp_dial` | No answer at all: the address did not resolve, refused the connection, or the egress policy refused it. |
| `txco_mcp_timeout` | The op's deadline passed. |
| `txco_mcp_http` | An HTTP status of 400 or more; `status` holds it. |
| `txco_mcp_rpc` | The server answered a JSON-RPC error; `rpc_code` holds its code (`-32602`: bad arguments). |
| `txco_mcp_too_large` | The answer is over `--op-payload-max`. |
| `txco_mcp_bad_answer` | The answer was not JSON-RPC. |
| `txco_connection_*` | A [connection](../grants.md#a-connection-as-a-value) the call names could not be used (`_missing`, `_reconnect`, …); nothing was sent. |

`phase` says where it failed: `init`, `initialized`, `call` or `list`.
`message` is at most 300 characters and may quote the server. A mistake in
the op itself (a `url` that is not http(s), no tool, an `args_path` that
names nothing) is an error either way.

## Credentials

A header set from a secret rides **every** request of the session, the
handshake included, since a server that requires authorization refuses
`initialize` without it:

```txcl
WITH secrets.headers.authorization.secret = "DOCS_MCP_KEY",
     secrets.headers.authorization.format = "Bearer {}"
```

A principal's own token at the server, from a
[connection](../grants.md#a-connection-as-a-value), is named the same way
it is on an `https` op, and is decided the same way (the pinned principal
and capability, the op's stack, the endpoint's origin):

```txcl
WITH secrets.headers.authorization.connection = "linear"
```

Sessions are kept per tenant, endpoint **and credential**, so two principals
at one server never share a session. `secrets.body.*` is not applied: a
secret never enters a tool's arguments.

## Probing a server via the CLI

```sh
txco mcp doctor https://mcp.example.com/mcp
```

Runs the initialize handshake and prints the server's tool list — the
fastest way to find the right `#tool-name` and check reachability.

## The other direction: Serving MCP tools

Exposing a stack's rules *as* MCP tools (so Claude or another agent
can call your flow) is something stacks can implement themselves. As an example, 
[examples/mcp-server](https://github.com/LoremLabs/thanks-computer/tree/main/examples/mcp-server) is a working MCP server written entirely as a Thanks, Computer stack.
