package processor

import (
	"context"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// Frames on the continuation paths (docs/todo-stack-call.md §2.6, Slice B):
// a resume and a detached op build their context in one place, and a call
// made from detached work keeps its frame.

// A `call://` under `mode = "continuable"` runs on a detached context with
// no opstack snapshot, so the callee's Run goes through its first-Run block.
// That block must not install a root frame over the call's: the callee stays
// boxed to its stack and never runs the parent's scopes. Both branches of
// the race — the upstream answering before continue_after, and the
// promotion — dispatch on that context.
func TestContinuableCallKeepsItsFrame(t *testing.T) {
	for _, tc := range []struct {
		name, continueAfter string
		slow                bool
	}{
		{"sync", "2s", false},
		{"promoted", "50ms", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pu := newContinuableUnit(t)
			if tc.slow {
				pu.Handle([]byte("txco://dawdle"), event.OpsHandlerFunc(
					func(ctx context.Context, _ string, _, _ []byte) (event.Payload, error) {
						select {
						case <-time.After(200 * time.Millisecond):
						case <-ctx.Done():
						}
						return event.Payload{Raw: `{}`, Type: event.JSON}, nil
					}))
				seedOp(t, pu, "app/onboard", 120, "dawdle", `EXEC "txco://dawdle"`)
			}
			seedOp(t, pu, "app", 100, "a100", `EMIT .ran.app100 = true`)
			seedOp(t, pu, "app", 200, "a200", `EMIT ._call.output.leaked = true`)
			seedOp(t, pu, "app/onboard", 100, "c100", `EMIT .ran.child100 = true`)
			seedOp(t, pu, "app/onboard", 150, "c150", `EMIT ._call.output.done = true`)
			seedOp(t, pu, "c", 100, "call", `EXEC "call://app/onboard" WITH mode = "continuable", continue_after = "`+tc.continueAfter+`", timeout = "5s"`)
			seedOp(t, pu, "c", 200, "after", `EMIT .after = true`)

			resCh := make(chan event.Payload, 2)
			go func() { _ = pu.Run(context.Background(), `{"_txc":{"stack":"c","src":"http"}}`, "c/0", resCh) }()
			var final string
			if tc.slow {
				// Promoted: the client gets a 202, the detached work finishes
				// the call, and the resume runs the caller's next scope; the
				// run's result is the caller's final envelope.
				rcid, _ := waitFor202(t, resCh)
				runID := resolveRunIDFromRcid(t, pu, rcid)
				if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
					t.Fatalf("run state = %q", st)
				}
				res, ok, _ := pu.Runs.ReadResult(context.Background(), runID)
				if !ok {
					t.Fatal("no result")
				}
				final = string(res)
			} else {
				select {
				case p := <-resCh:
					final = p.Raw
				case <-time.After(5 * time.Second):
					t.Fatal("no answer")
				}
			}
			if !gjson.Get(final, "_call.ok").Bool() || !gjson.Get(final, "_call.output.done").Bool() {
				t.Errorf("the call: %s", final)
			}
			if gjson.Get(final, "_call.output.leaked").Exists() {
				t.Errorf("the callee ran the parent's scopes (its frame was lost): %s", final)
			}
			if !gjson.Get(final, "after").Bool() {
				t.Errorf("the caller did not continue: %s", final)
			}
		})
	}
}

// A deferred-join resume re-pins the source from the scope envelope, as a
// barrier resume does: an op that gates on the pinned source still sees the
// inlet that started the run after the join.
func TestDeferredResumeRepinsSource(t *testing.T) {
	pu := newContinuableUnit(t)
	stub := newAsyncStub(t) // acks 202, never calls back
	pu.Handle([]byte("txco://probe-source"), event.OpsHandlerFunc(
		func(ctx context.Context, _ string, _, _ []byte) (event.Payload, error) {
			return event.Payload{
				Raw:  `{"probe":{"src":"` + SourceScope(ctx) + `","tenant":"` + TenantScope(ctx) + `"}}`,
				Type: event.JSON,
			}, nil
		}))
	seedOp(t, pu, "site", 100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async", join_at_scope = 200`)
	seedOp(t, pu, "site", 200, "probe", `EXEC "txco://probe-source"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"src":"websocket"}}`, "site/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	opc, _, _, _ := stub.captured()
	ctx := context.Background()
	stage, _, _ := pu.Runs.ReadDeferredSuspendedAt(ctx, runID, opc)
	if _, err := pu.Runs.RecordDeferredTerminal(ctx, runID, opc, "completed", continuation.TerminalMeta{}, []byte(`{"summary":"S"}`)); err != nil {
		t.Fatal(err)
	}
	pu.DriveDeferredResume(runID, stage)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q", st)
	}
	res, ok, _ := pu.Runs.ReadResult(ctx, runID)
	if !ok {
		t.Fatal("no result")
	}
	if got := gjson.GetBytes(res, "probe.src").String(); got != "websocket" {
		t.Errorf("SourceScope after a deferred resume = %q, want websocket: %s", got, res)
	}
}
