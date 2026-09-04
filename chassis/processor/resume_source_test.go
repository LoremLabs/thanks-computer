package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestResumeRepinsSource: a resumed continuation runs under a fresh context
// (every resume caller starts from Background) and Run's first-Run pin
// block is skipped once the opstack snapshot is attached — so Resume must
// re-pin the originating inlet from the chassis-stamped scope envelope the
// way it re-pins the tenant. Ops that gate on the pinned source
// (txco://websocket/reply, txco://relay) depend on it after a suspend.
func TestResumeRepinsSource(t *testing.T) {
	pu := newContinuableUnit(t)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"done"}]}}`)
	}
	pu.Handle([]byte("txco://probe-source"), event.OpsHandlerFunc(
		func(ctx context.Context, _ string, _, _ []byte) (event.Payload, error) {
			return event.Payload{
				Raw:  `{"probe":{"src":"` + SourceScope(ctx) + `","tenant":"` + TenantScope(ctx) + `"}}`,
				Type: event.JSON,
			}, nil
		}))

	seedOp(t, pu, "acme", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)
	seedOp(t, pu, "acme", 200, "probe", `EXEC "txco://probe-source"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"src":"websocket"}}`, "acme/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}
	res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
	if !ok {
		t.Fatal("no result.json")
	}
	if got := gjson.GetBytes(res, "probe.src").String(); got != "websocket" {
		t.Fatalf("SourceScope in the resumed scope = %q, want websocket (result: %s)", got, res)
	}
}
