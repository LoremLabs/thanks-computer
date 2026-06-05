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
func (r *recordingTracer) End(string, string, []byte) {}

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

// blockingHandler signals once it is in-flight, then blocks until release
// is closed (regardless of ctx) — so an abandoning ctx.Done() leaves its
// op provably still in flight. After release it returns an error, taking
// the best-effort drop path (no send on the unbuffered `responses`), so
// the goroutine exits cleanly during test teardown.
type blockingHandler struct {
	started chan struct{}
	release chan struct{}
}

func (h blockingHandler) Route(_ context.Context, _ string, _, _ []byte) (event.Payload, error) {
	close(h.started)
	<-h.release
	return event.Payload{}, context.Canceled
}

// TestRunFlushesInflightOpOnCancel proves Part B: when a request is
// abandoned (ctx cancelled) while a sync op is still running, Run records
// a "cancelled" step naming that op (so a stalled op is no longer
// invisible) AND returns an enriched reason naming it (which becomes the
// trace's top-level "why" in server.runWithTrace).
func TestRunFlushesInflightOpOnCancel(t *testing.T) {
	pu, _ := newTestUnit(t)

	h := blockingHandler{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) }) // unblock the leaked op goroutine after assertions
	pu.Handle([]byte("txco://block-stub"), h)

	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		"cxltest", 0, "c", `EXEC "txco://block-stub"`,
	); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	rt := &recordingTracer{}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = trace.WithContext(ctx, rt)
	resCh := make(chan event.Payload, 1)

	runErr := make(chan error, 1)
	go func() { runErr <- pu.Run(ctx, `{}`, "cxltest/0", resCh) }()

	<-h.started // the op has reached Route → it is dispatched + in-flight
	cancel()    // abandon the request while the op is still running

	err := <-runErr
	if err == nil || !strings.Contains(err.Error(), "canceled while running") {
		t.Fatalf("Run err = %v, want an enriched cancel reason", err)
	}
	if !strings.Contains(err.Error(), "cxltest/0") {
		t.Errorf("reason should name the stalled op (cxltest/0): %v", err)
	}

	st := rt.stepByOpName("c")
	if st == nil {
		t.Fatalf("no step recorded for the in-flight op; recorded=%+v", rt.steps)
	}
	if st.Status != "cancelled" {
		t.Errorf("step status = %q, want cancelled", st.Status)
	}
	if st.Error == "" {
		t.Errorf("cancelled step has no reason")
	}
}
