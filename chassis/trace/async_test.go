package trace

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingSink is a base sink used to observe AsyncSink dispatch.
// Captures everything passed to its tracer for inspection.
type recordingSink struct {
	mu          sync.Mutex
	begins      []RequestInfo
	steps       []StepInfo
	events      []TimelineEvent
	ends        []recordedEnd
	closedCount int
}

type recordedEnd struct {
	status string
	final  []byte
}

func (s *recordingSink) Begin(info RequestInfo) RequestTracer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begins = append(s.begins, info)
	return &recordingTracer{sink: s}
}

func (s *recordingSink) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closedCount++
	return nil
}

func (s *recordingSink) snapshot() (int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.begins), len(s.steps), len(s.events), len(s.ends)
}

type recordingTracer struct {
	sink *recordingSink
}

func (t *recordingTracer) Step(info StepInfo) {
	t.sink.mu.Lock()
	defer t.sink.mu.Unlock()
	t.sink.steps = append(t.sink.steps, info)
}

func (t *recordingTracer) Event(ev TimelineEvent) {
	t.sink.mu.Lock()
	defer t.sink.mu.Unlock()
	t.sink.events = append(t.sink.events, ev)
}

func (t *recordingTracer) End(status, reason string, final []byte) {
	t.sink.mu.Lock()
	defer t.sink.mu.Unlock()
	t.sink.ends = append(t.sink.ends, recordedEnd{status: status, final: final})
}

// TestAsyncSinkForwardsToBase locks in the basic happy path: events
// enqueued via the async tracer are eventually delivered to the base.
func TestAsyncSinkForwardsToBase(t *testing.T) {
	rec := &recordingSink{}
	s := NewAsyncSink(rec, AsyncOpts{BufferSize: 8, BodyCapBytes: 1 << 20})

	tr := s.Begin(RequestInfo{RID: "r1", StartedAt: time.Now()})
	tr.Step(StepInfo{Stack: "x", Scope: 1, Name: "a"})
	tr.Event(TimelineEvent{Event: "noted"})
	tr.End("ok", "", []byte(`{}`))

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, st, e, en := rec.snapshot()
	if b != 1 || st != 1 || e != 1 || en != 1 {
		t.Errorf("recorded begins=%d steps=%d events=%d ends=%d, want 1 each", b, st, e, en)
	}
	if rec.closedCount != 1 {
		t.Errorf("base sink closed %d times, want 1", rec.closedCount)
	}
}

// TestAsyncSinkCapsBodies verifies that bodies larger than BodyCapBytes
// are truncated before the queue and that the original size is
// preserved in InputBytes/OutputBytes so meta.json can still report it.
func TestAsyncSinkCapsBodies(t *testing.T) {
	rec := &recordingSink{}
	s := NewAsyncSink(rec, AsyncOpts{BufferSize: 4, BodyCapBytes: 16})

	bigIn := make([]byte, 1000)
	bigOut := make([]byte, 500)
	for i := range bigIn {
		bigIn[i] = 'I'
	}
	for i := range bigOut {
		bigOut[i] = 'O'
	}
	tr := s.Begin(RequestInfo{
		RID:       "cap",
		Payload:   make([]byte, 100),
		StartedAt: time.Now(),
	})
	tr.Step(StepInfo{Input: bigIn, Output: bigOut})
	tr.End("ok", "", make([]byte, 200))

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()

	// root in.json's payload should be capped + original size recorded.
	if got := rec.begins[0].PayloadBytes; got != 100 {
		t.Errorf("Begin PayloadBytes=%d, want 100", got)
	}
	if got := len(rec.begins[0].Payload); got != 16 {
		t.Errorf("Begin Payload len=%d, want 16 (capped)", got)
	}

	// Step bodies capped + original sizes preserved.
	if got := rec.steps[0].InputBytes; got != 1000 {
		t.Errorf("Step InputBytes=%d, want 1000", got)
	}
	if got := len(rec.steps[0].Input); got != 16 {
		t.Errorf("Step Input len=%d, want 16", got)
	}
	if got := rec.steps[0].OutputBytes; got != 500 {
		t.Errorf("Step OutputBytes=%d, want 500", got)
	}
	if got := len(rec.steps[0].Output); got != 16 {
		t.Errorf("Step Output len=%d, want 16", got)
	}

	// End's final payload also capped.
	if got := len(rec.ends[0].final); got != 16 {
		t.Errorf("End final len=%d, want 16", got)
	}
}

// TestAsyncSinkDropsOnFullBuffer locks in non-blocking semantics. The
// request path must NEVER block on a backed-up trace queue. We choke
// the worker (slow base) and fill the buffer; subsequent enqueues
// increment the drop counter rather than backing up.
func TestAsyncSinkDropsOnFullBuffer(t *testing.T) {
	// Slow base: blocks on each Step until the test releases it.
	slow := &slowSink{release: make(chan struct{})}

	var dropped atomic.Int64
	s := NewAsyncSink(slow, AsyncOpts{
		BufferSize:   2,
		BodyCapBytes: 1 << 16,
		DropCounter:  &dropped,
	})
	tr := s.Begin(RequestInfo{RID: "drop", StartedAt: time.Now()})

	// First call enters the worker (which then blocks on slow.release).
	// Subsequent 2 fill the buffer (BufferSize=2). The 4th onward
	// should be dropped.
	const totalSteps = 10
	for i := 0; i < totalSteps; i++ {
		tr.Step(StepInfo{Stack: "x", Scope: i, Name: "s"})
	}

	// Wait briefly for the drop counter to settle (the loop is fast
	// but enqueue is non-blocking).
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && dropped.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if dropped.Load() == 0 {
		t.Errorf("expected DropCounter > 0, got 0 (worker may not have stalled)")
	}

	// Release the worker so Close can drain.
	close(slow.release)
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// slowSink's Step blocks until release is closed. Used to force the
// AsyncSink buffer to fill up so we can exercise the drop path.
type slowSink struct {
	release chan struct{}
}

func (s *slowSink) Begin(RequestInfo) RequestTracer { return &slowTracer{release: s.release} }
func (s *slowSink) Close(context.Context) error     { return nil }

type slowTracer struct{ release chan struct{} }

func (t *slowTracer) Step(StepInfo) {
	<-t.release // block until test signals
}
func (t *slowTracer) Event(TimelineEvent)        {}
func (t *slowTracer) End(string, string, []byte) {}

// TestAsyncSinkCloseDrains verifies that Close waits for in-flight
// events to flush before returning. A reader observing the underlying
// sink after Close sees all events that were ever enqueued.
func TestAsyncSinkCloseDrains(t *testing.T) {
	rec := &recordingSink{}
	// Buffer must comfortably hold N steps + N events + 1 end with no
	// drops, so we can assert "Close drains EVERYTHING enqueued."
	s := NewAsyncSink(rec, AsyncOpts{BufferSize: 256, BodyCapBytes: 1 << 16})

	tr := s.Begin(RequestInfo{RID: "drain", StartedAt: time.Now()})
	const N = 50
	for i := 0; i < N; i++ {
		tr.Step(StepInfo{Stack: "x", Scope: i, Name: "s"})
		tr.Event(TimelineEvent{Event: "ev"})
	}
	tr.End("ok", "", []byte(`{}`))

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, st, e, en := rec.snapshot()
	if st != N {
		t.Errorf("steps after drain = %d, want %d", st, N)
	}
	if e != N {
		t.Errorf("events after drain = %d, want %d", e, N)
	}
	if en != 1 {
		t.Errorf("ends after drain = %d, want 1", en)
	}
}

// TestAsyncSinkCloseRespectsContextDeadline verifies that a short ctx
// deadline returns ctx.Err() instead of waiting forever. Useful so a
// chassis shutdown can give up on trace flushes rather than hang.
func TestAsyncSinkCloseRespectsContextDeadline(t *testing.T) {
	slow := &slowSink{release: make(chan struct{})}
	defer close(slow.release)

	s := NewAsyncSink(slow, AsyncOpts{BufferSize: 4})
	tr := s.Begin(RequestInfo{RID: "ctx", StartedAt: time.Now()})
	tr.Step(StepInfo{Name: "block"}) // worker grabs this and blocks

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := s.Close(ctx)
	if err == nil {
		t.Error("expected ctx error from Close (drain timed out), got nil")
	}
}

// TestAsyncSinkEndToEndWithFileSink wires AsyncSink onto a real
// FileSink and verifies the trace tree on disk reflects what was
// enqueued — including truncated input bytes in meta.json.
func TestAsyncSinkEndToEndWithFileSink(t *testing.T) {
	dir := t.TempDir()
	base, err := NewFileSink(dir, ModeFull)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	s := NewAsyncSink(base, AsyncOpts{BufferSize: 16, BodyCapBytes: 8})

	tr := s.Begin(RequestInfo{
		RID:       "e2e",
		Src:       "http",
		StartedAt: time.Now(),
		Payload:   []byte(`{"large":"this exceeds eight bytes"}`),
	})
	tr.Step(StepInfo{
		Stack: "svc", Scope: 100, Name: "op",
		Operation: "http://x", Transport: "http",
		Input:     []byte(`{"in":"more than 8 bytes here"}`),
		Output:    []byte(`{"out":"also long enough"}`),
		StartedAt: time.Now(), FinishedAt: time.Now(),
		Status: "ok",
	})
	tr.End("ok", "", []byte(`{"final":"long final body"}`))

	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reqDir := filepath.Join(dir, "requests", "e2e")
	stepDirs, _ := os.ReadDir(filepath.Join(reqDir, "steps"))
	if len(stepDirs) != 1 {
		t.Fatalf("steps count = %d, want 1", len(stepDirs))
	}
	stepDir := filepath.Join(reqDir, "steps", stepDirs[0].Name())

	// in.json should be the truncated prefix (8 bytes).
	inBytes, _ := os.ReadFile(filepath.Join(stepDir, "in.json"))
	if len(inBytes) != 8 {
		t.Errorf("in.json size = %d, want 8 (cap)", len(inBytes))
	}

	// meta.json should show the ORIGINAL size + truncated flag.
	mb, _ := os.ReadFile(filepath.Join(stepDir, "meta.json"))
	var meta map[string]any
	if err := json.Unmarshal(mb, &meta); err != nil {
		t.Fatalf("parse meta: %v", err)
	}
	if got := int(meta["input_bytes"].(float64)); got != len(`{"in":"more than 8 bytes here"}`) {
		t.Errorf("meta.input_bytes = %d, want %d (original)", got, len(`{"in":"more than 8 bytes here"}`))
	}
	if truncated, _ := meta["input_truncated"].(bool); !truncated {
		t.Errorf("meta.input_truncated should be true; meta=%v", meta)
	}
}
