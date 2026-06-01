package processor

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// recordingTracer captures Step + TimelineEvents in-memory for
// assertions. Implements trace.RequestTracer.
type recordingTracer struct {
	mu     sync.Mutex
	steps  []trace.StepInfo
	events []trace.TimelineEvent
}

func (r *recordingTracer) Step(s trace.StepInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, s)
}
func (r *recordingTracer) Event(ev trace.TimelineEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
}
func (r *recordingTracer) End(string, []byte) {}

func (r *recordingTracer) stepByOpName(name string) *trace.StepInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.steps {
		if r.steps[i].Name == name {
			return &r.steps[i]
		}
	}
	return nil
}

// --- extractOpDebug unit tests ---

func TestExtractOpDebugAbsentReturnsUnchanged(t *testing.T) {
	in := `{"text":"hi","_txc":{"ok":true}}`
	stripped, present := extractOpDebug(in)
	if present {
		t.Errorf("present = true; expected false (no _txc_op_debug field)")
	}
	if stripped != in {
		t.Errorf("stripped should equal input when nothing to extract")
	}
}

func TestExtractOpDebugStripsField(t *testing.T) {
	in := `{"text":"hi","_txc_op_debug":{"rendered_prompt":"hello world","tokens":42},"_txc":{"ok":true}}`
	stripped, present := extractOpDebug(in)
	if !present {
		t.Fatalf("present = false; expected true")
	}
	if strings.Contains(stripped, "_txc_op_debug") {
		t.Errorf("stripped still contains _txc_op_debug: %q", stripped)
	}
	if gjson.Get(stripped, "text").String() != "hi" {
		t.Errorf("strip damaged other fields")
	}
}

// --- end-to-end through Run ---

type stubOpsHandler struct {
	raw string
}

func (s stubOpsHandler) Route(_ context.Context, _ string, _, _ []byte) (event.Payload, error) {
	return event.Payload{Raw: s.raw, Type: event.JSON}, nil
}

// TestRunStripsOpDebugFromEnvelopeButKeepsInStepOutput proves the two
// load-bearing properties of the pattern:
//
//  1. The final envelope (what `resCh` delivers, what downstream rules
//     and the inlet see) does NOT contain `_txc_op_debug`.
//  2. The captured `trace.Step.Output` for that op DOES contain
//     `_txc_op_debug` — diagnostic data is preserved in the per-step
//     `out.json` trace file even though it doesn't propagate.
//
// This is the design: trace shows everything the handler produced;
// the envelope shows only what propagates downstream. No duplicate
// `op.debug` timeline event needed — step.Output already carries the
// debug content.
func TestRunStripsOpDebugFromEnvelopeButKeepsInStepOutput(t *testing.T) {
	pu, _ := newTestUnit(t)

	pu.Handle([]byte("txco://debug-stub"), stubOpsHandler{
		raw: `{"text":"stub-ok","_txc_op_debug":{"rendered_prompt":"stub-rendered","tokens":7}}`,
	})

	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"dbgtest", 0, "c", `EXEC "txco://debug-stub"`,
	); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	rt := &recordingTracer{}
	ctx := trace.WithContext(context.Background(), rt)
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(ctx, `{}`, "dbgtest/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Property 1: stripped from the envelope.
	select {
	case payload := <-resCh:
		if strings.Contains(payload.Raw, "_txc_op_debug") {
			t.Errorf("final envelope still contains _txc_op_debug: %s", payload.Raw)
		}
		if got := gjson.Get(payload.Raw, "text").String(); got != "stub-ok" {
			t.Errorf("text = %q, want stub-ok (envelope=%s)", got, payload.Raw)
		}
	default:
		t.Fatal("no response on resCh")
	}

	// Property 2: preserved in the step's captured Output.
	step := rt.stepByOpName("c")
	if step == nil {
		t.Fatalf("no step recorded; recorded=%+v", rt.steps)
	}
	if !strings.Contains(string(step.Output), "_txc_op_debug") {
		t.Errorf("step.Output should contain _txc_op_debug (pre-strip capture); got %s", step.Output)
	}
	if !strings.Contains(string(step.Output), "stub-rendered") {
		t.Errorf("step.Output should contain the stamped debug content; got %s", step.Output)
	}

	// No spurious op.debug timeline event (the per-step trace is the
	// single source).
	for _, ev := range rt.events {
		if ev.Event == "op.debug" {
			t.Errorf("unexpected op.debug timeline event — debug lives in step.Output, not as a separate event")
		}
	}
}
