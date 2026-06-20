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
