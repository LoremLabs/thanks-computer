package processor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// waitForRunCompleted polls the continuation store for a run to
// reach a terminal state. Local async runs detached, so tests
// can't assume Resume has finished by the time the 202 lands.
// 5s is enough for in-process MCP roundtrips through a stub.
func waitForRunCompleted(t *testing.T, pu *Unit, runID string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := pu.Runs.RunState(context.Background(), runID)
		if st == continuation.StateCompleted || st == continuation.StateFailed {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach terminal within 5s", runID)
	return ""
}

// resolveRunIDFromRcid maps the rcid the client sees in the 202 body
// back to the internal runID — same path the continuation poll uses.
func resolveRunIDFromRcid(t *testing.T, pu *Unit, rcid string) string {
	t.Helper()
	runID, err := pu.Runs.ResolveRunContinuation(context.Background(), rcid)
	if err != nil {
		t.Fatalf("ResolveRunContinuation(%q): %v", rcid, err)
	}
	return runID
}

// TestLocalAsyncSuspendResumeEndToEnd — the happy path. An MCP op
// at acme/100 with mode = "async" returns a 202 immediately; the
// chassis runs ExecMCPHTTP in a detached goroutine, writes the
// terminal, and triggers Resume which advances to acme/200's EMIT.
// Final run result should carry both the MCP `text` AND the
// post-scope EMIT field.
func TestLocalAsyncSuspendResumeEndToEnd(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"async-hi"}]}}`)
	}

	seedOp(t, pu, "acme", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask_question")+`" WITH mode = "async", timeout = "5s"`)
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"q":"x"}`, "acme/100", resCh) }()

	rcid, loc := waitFor202(t, resCh)
	if rcid == "" || loc != "/?_txc.continuation="+rcid {
		t.Fatalf("bad 202: rcid=%q loc=%q", rcid, loc)
	}

	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok {
		t.Fatal("no result.json after local-async completion")
	}
	if got := gjson.GetBytes(res, "text").String(); got != "async-hi" {
		t.Fatalf("result missing MCP text: %s", res)
	}
	if !gjson.GetBytes(res, "resumed").Bool() {
		t.Fatalf("result missing acme/200 EMIT: %s", res)
	}
}

// TestLocalAsyncOpTimeoutFails — when the MCP server hangs, the
// per-op `WITH timeout` should expire and the terminal record
// stage failure (mirroring sync-op timeout behavior).
func TestLocalAsyncOpTimeoutFails(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	// Stub that hangs on tools/call until the test cleanup tears
	// it down. initialize / initialized still respond normally so
	// the lifecycle reaches the call phase.
	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		time.Sleep(2 * time.Second)
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`)
	}

	// 200ms timeout — well below the 2s hang above.
	seedOp(t, pu, "slow", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "200ms"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "slow/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)

	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateFailed {
		t.Fatalf("expected failed run on op timeout, got %q", st)
	}
}

// TestLocalAsyncSchemeFilterRejectsBareHTTP — `WITH mode = "async"`
// on a plain http:// URL stays on the remote-worker path
// (isLocalAsyncOp returns false). Confirms the scheme gate works.
// Upstreams that need automatic continuation (no worker contract)
// belong on `mode = "continuable"`, not on `mode = "async"`.
func TestLocalAsyncSchemeFilterRejectsBareHTTP(t *testing.T) {
	stub := newMCPStub(t) // any URL — won't be hit
	op := mcpOp(stub.URL, "tool", `{}`, `{"mode":"async"}`)
	// Override to bare http:// (no mcp+ prefix).
	op.Resonator.Exec = stub.URL
	if isLocalAsyncOp(op) {
		t.Error("isLocalAsyncOp must return false for bare http:// even with mode=async")
	}
	if !isAsyncOp(op) {
		t.Error("isAsyncOp should still accept bare http:// with mode=async (remote worker path)")
	}
}

// TestLocalAsyncSchemeFilterAcceptsMCPHTTPS — both mcp+http:// and
// mcp+https:// must classify as local async.
func TestLocalAsyncSchemeFilterAcceptsMCPHTTPS(t *testing.T) {
	for _, ex := range []string{
		"mcp+http://example.com/mcp#tool",
		"mcp+https://example.com/mcp#tool",
	} {
		op := mcpOp("http://x", "tool", `{}`, `{"mode":"async"}`)
		op.Resonator.Exec = ex
		if !isLocalAsyncOp(op) {
			t.Errorf("isLocalAsyncOp(%q) = false, want true", ex)
		}
	}
}

// TestLocalAsyncWithoutMode — mcp+http:// without WITH mode = "async"
// must stay on the sync path (legacy behavior).
func TestLocalAsyncWithoutMode(t *testing.T) {
	op := mcpOp("http://x", "tool", `{}`, ``) // no mode
	op.Resonator.Exec = "mcp+http://example.com/mcp#tool"
	if isAsyncOp(op) {
		t.Error("mcp+http:// without mode=async must be sync")
	}
	if isLocalAsyncOp(op) {
		t.Error("mcp+http:// without mode=async must not be local async")
	}
}

// TestLocalAsyncEmitOverlay — EMIT overlays on a local-async rule
// must land on the terminal record so the final merged result
// carries them. Parity with sync + remote-async EMIT handling.
func TestLocalAsyncEmitOverlay(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"core"}]}}`)
	}

	seedOp(t, pu, "emit-tst", 100, "ask",
		fmt.Sprintf(`EXEC %q WITH mode = "async", timeout = "5s" EMIT .tagged_by = "local-async"`,
			mcpExecURL(stub.URL, "ask")))

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "emit-tst/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok {
		t.Fatal("no result.json")
	}
	if got := gjson.GetBytes(res, "text").String(); got != "core" {
		t.Errorf("result missing MCP text: %s", res)
	}
	if got := gjson.GetBytes(res, "tagged_by").String(); got != "local-async" {
		t.Errorf("EMIT overlay missing on terminal: %s", res)
	}
}

// TestLocalAsyncMCPFailureRecordsFailedTerminal — when the MCP
// server returns a JSON-RPC error at tools/call, the chassis
// records a failed terminal. The run reaches Failed state via
// Resume's failure-handling path (FailStage on a failed op).
func TestLocalAsyncMCPFailureRecordsFailedTerminal(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"bad arg"}}`)
	}

	seedOp(t, pu, "fail-tst", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "fail-tst/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)

	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateFailed {
		t.Fatalf("run state = %q, want failed (MCP JSON-RPC error → failed terminal)", st)
	}
}
