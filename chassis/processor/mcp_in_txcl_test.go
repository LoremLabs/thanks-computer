package processor

import (
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/txcl/lexer"
	"github.com/loremlabs/thanks-computer/chassis/txcl/parser"
)

// TestE2E_MCPServerInTxcl is the PR-4 acceptance test: it proves
// the claim from internal docs/todo-mcp-server-in-txcl.md §6.1 — that an
// entire MCP server can be written in txcl rules using only the
// expression-language function registry, no new `txco://` ops.
//
// The flow exercises every category of function added in PR 4:
//
//   - `&b64decode(@web.req.body)`  — decode the HTTP request body
//   - `&json(...)`                  — parse it as JSON-RPC
//   - `&get(...)`                   — extract method, id, params
//   - `&object(...)` / `&array(...)` — build the JSON-RPC response
//
// Plus the PR-3 pilot:
//
//   - `&uuid()`                     — mint a session id at init
//
// The test simulates two rules from a pure-txcl MCP server: the
// envelope-parser at the front and one tool handler (`echo`). It
// runs both through DecorateInput/OverlayResponse and verifies the
// resulting envelope shape matches what the web outlet would
// serialize as a valid JSON-RPC response.
func TestE2E_MCPServerInTxcl(t *testing.T) {
	pu, _ := newTestUnit(t)

	// --- step 1: parse the incoming web body --------------------
	//
	// A pure-txcl MCP server's first rule decodes the base64'd
	// request body and parses it as JSON, exposing the JSON-RPC
	// fields as addressable envelope keys for subsequent rules.
	//
	// `@rpc` writes under `_txc.rpc` (the chassis-internal
	// namespace); this is the right home for parsed request data
	// — the web outlet strips `_`-prefixed keys on the way out so
	// the chassis plumbing doesn't leak into the response body.
	parseRule := `SET @rpc = &json(&b64decode(@web.req.body))`

	// Build a realistic envelope as the web inlet would: the body
	// is a JSON-RPC `tools/call` for the `echo` tool, base64'd.
	rpcRequest := `{"jsonrpc":"2.0","id":"req-7","method":"tools/call","params":{"name":"echo","arguments":{"text":"hello mcp"}}}`
	encodedBody := base64.StdEncoding.EncodeToString([]byte(rpcRequest))
	envelope, _ := sjson.Set(`{"_txc":{"web":{"req":{}}}}`, "_txc.web.req.body", encodedBody)

	res := parser.New(lexer.New(parseRule)).ParseEvent()
	if res == nil || res.SetPre == nil {
		t.Fatalf("parse-rule produced no SET PRE; got %#v", res)
	}

	envelope, err := pu.DecorateInput(envelope, res.SetPre.Overrides)
	if err != nil {
		t.Fatalf("step 1 (parse body): %v", err)
	}

	// Verify the rpc subtree landed under _txc and is addressable.
	if got := gjson.Get(envelope, "_txc.rpc.method").String(); got != "tools/call" {
		t.Fatalf("_txc.rpc.method: got %q (envelope: %s)", got, envelope)
	}
	if got := gjson.Get(envelope, "_txc.rpc.params.name").String(); got != "echo" {
		t.Fatalf("_txc.rpc.params.name: got %q", got)
	}

	// --- step 2: handle the echo tool ---------------------------
	//
	// One rule per tool. The handler builds the JSON-RPC response
	// envelope in one nested expression — &object/&array compose
	// the structure, &get pulls fields out of the parsed request.
	echoRule := `EMIT .res = &object("jsonrpc", "2.0", "id", &get(@rpc, "id"), "result", &object("content", &array(&object("type", "text", "text", &get(@rpc, "params.arguments.text")))))`

	res = parser.New(lexer.New(echoRule)).ParseEvent()
	if res == nil || res.Emit == nil {
		t.Fatalf("echo-rule produced no EMIT; got %#v", res)
	}

	// env=envelope (where PathRefs read @rpc.id and the rest from),
	// output=envelope (where the EMITted `.res` is written). For
	// this test both happen to be the same document — in the live
	// chassis they're op.Input (env) and op.Output (write target).
	envelope, err = pu.OverlayResponse(envelope, envelope, res.Emit.Overrides)
	if err != nil {
		t.Fatalf("step 2 (echo handler): %v", err)
	}

	// --- assert the JSON-RPC response shape ---------------------
	//
	// The web outlet would project the envelope (minus _-prefixed
	// keys) as the response body, producing the JSON-RPC reply
	// the MCP client expects.
	cases := []struct {
		path string
		want string
	}{
		{"res.jsonrpc", `"2.0"`},
		{"res.id", `"req-7"`},
		{"res.result.content.0.type", `"text"`},
		{"res.result.content.0.text", `"hello mcp"`},
	}
	for _, tc := range cases {
		got := gjson.Get(envelope, tc.path).Raw
		if got != tc.want {
			t.Errorf("%s: got %s, want %s\n  envelope: %s",
				tc.path, got, tc.want, envelope)
		}
	}
}

// TestE2E_TryFunctionsRecoverGracefully proves the try_ variants
// behave as designed: a malformed input produces null (not a halt),
// and downstream rules can detect and handle it. Mirrors the
// "untrusted webhook payload" pattern from the design doc §5.
func TestE2E_TryFunctionsRecoverGracefully(t *testing.T) {
	pu, _ := newTestUnit(t)

	// Bad base64 body (e.g., a webhook that promised JSON but sent
	// garbled bytes). Using `&try_b64decode` lets the rule
	// continue; using bare `&b64decode` would halt.
	rule := `SET .parsed = &try_json(&try_b64decode(@web.req.body))`

	envelope := `{"_txc":{"web":{"req":{"body":"not!valid!base64!"}}}}`

	res := parser.New(lexer.New(rule)).ParseEvent()
	if res == nil || res.SetPre == nil {
		t.Fatalf("parse produced no SET PRE; got %#v", res)
	}

	got, err := pu.DecorateInput(envelope, res.SetPre.Overrides)
	if err != nil {
		t.Fatalf("DecorateInput should not error with try_ variants: %v", err)
	}
	if gjson.Get(got, "parsed").Type != gjson.Null {
		t.Errorf("parsed should be null (recovered); got body %s", got)
	}
}
