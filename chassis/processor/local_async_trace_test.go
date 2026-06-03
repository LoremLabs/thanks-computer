package processor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// withFileSink wires a file-backed trace sink onto pu pointed at a
// temp dir. Returns the dir so the test can inspect resulting files.
func withFileSink(t *testing.T, pu *Unit) string {
	t.Helper()
	dir := t.TempDir()
	sink, err := trace.NewFileSink(dir, trace.ModeFull)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	pu.Sink = sink
	return dir
}

func readTimeline(t *testing.T, traceDir, rid string) []map[string]any {
	t.Helper()
	path := filepath.Join(traceDir, "requests", rid, "timeline.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read timeline for %s: %v", rid, err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse timeline event %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func findEventByName(events []map[string]any, name string) map[string]any {
	for _, ev := range events {
		if ev["event"] == name {
			return ev
		}
	}
	return nil
}

// TestLocalAsyncResumeTraceExists — a complete local-async cycle
// writes a resume trace under the deterministic
// `continuation.ResumeTraceRID(runID, stage)` rid. Admin-ui can
// then cross-navigate from origin → resume.
func TestLocalAsyncResumeTraceExists(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	traceDir := withFileSink(t, pu)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	seedOp(t, pu, "trace-tst", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "trace-tst/100", resCh) }()

	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	resumeRID := continuation.ResumeTraceRID(runID, "trace-tst/100")
	resumeDir := filepath.Join(traceDir, "requests", resumeRID)
	if _, err := os.Stat(resumeDir); err != nil {
		t.Fatalf("expected resume trace at %s: %v", resumeDir, err)
	}
}

// TestLocalAsyncResumeTraceCarriesLinkageEvent — the resume trace
// emits a `continuation.resume` event with run_id + origin_rid +
// stage, matching the shape continuation.go (the remote-worker
// callback handler) writes. Admin-ui depends on this for cross-nav.
func TestLocalAsyncResumeTraceCarriesLinkageEvent(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	traceDir := withFileSink(t, pu)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	seedOp(t, pu, "link-tst", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "link-tst/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	resumeRID := continuation.ResumeTraceRID(runID, "link-tst/100")
	events := readTimeline(t, traceDir, resumeRID)
	link := findEventByName(events, "continuation.resume")
	if link == nil {
		t.Fatalf("resume trace missing continuation.resume event; timeline=%v", events)
	}
	// FileSink flattens TimelineEvent.Fields onto the event document
	// (see chassis/trace/file.go Event()), so the linkage values are
	// top-level keys, not nested under a `fields` map.
	if link["run_id"] != runID {
		t.Errorf("linkage run_id = %v, want %s", link["run_id"], runID)
	}
	if link["stage"] != "link-tst/100" {
		t.Errorf("linkage stage = %v, want link-tst/100", link["stage"])
	}
	if _, ok := link["origin_rid"]; !ok {
		t.Errorf("linkage missing origin_rid field; event=%v", link)
	}
}

// TestLocalAsyncResumeTraceContainsMCPCallStep — the actual MCP call
// (its underlying transport step) lands on the RESUME trace, NOT on
// the suspending request's trace. That's where the `_debug` payload
// becomes visible when WITH debug = true.
func TestLocalAsyncResumeTraceContainsMCPCallStep(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	traceDir := withFileSink(t, pu)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	seedOp(t, pu, "step-tst", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "step-tst/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	// The resume trace should have a step.start for the MCP call.
	// step.start carries the operation URL (step.end omits it).
	resumeRID := continuation.ResumeTraceRID(runID, "step-tst/100")
	events := readTimeline(t, traceDir, resumeRID)
	var foundCallStep bool
	for _, ev := range events {
		if ev["event"] != "step.start" {
			continue
		}
		op, _ := ev["operation"].(string)
		if strings.HasPrefix(op, "mcp+") {
			foundCallStep = true
			break
		}
	}
	if !foundCallStep {
		t.Fatalf("resume trace missing the mcp+* step.start; events=%v", events)
	}
}

// TestLocalAsyncResumeTraceCoversSubsequentScopes — Resume runs
// scope-200+ rules under the resume trace, not silently. Asserts a
// scope-200 step lands on the resume trace.
func TestLocalAsyncResumeTraceCoversSubsequentScopes(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	traceDir := withFileSink(t, pu)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	seedOp(t, pu, "scope-tst", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)
	seedOp(t, pu, "scope-tst", 200, "render", `EMIT .rendered = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "scope-tst/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	resumeRID := continuation.ResumeTraceRID(runID, "scope-tst/100")
	events := readTimeline(t, traceDir, resumeRID)
	var scope200End bool
	for _, ev := range events {
		if ev["event"] != "step.end" {
			continue
		}
		scope, _ := ev["scope"].(float64) // JSON numbers decode as float64
		if int(scope) == 200 {
			scope200End = true
			break
		}
	}
	if !scope200End {
		t.Fatalf("resume trace missing scope-200 step.end; events=%v", events)
	}
}

// TestLocalAsyncNoSinkStillWorks — pu.Sink unset (the default test
// unit) → local async still functions, just untraced. Nil-safety
// guard, so unit tests + non-server callers don't have to wire a
// sink to use local async.
func TestLocalAsyncNoSinkStillWorks(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	// Deliberately leave pu.Sink == nil.

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	seedOp(t, pu, "nosink", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "nosink/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed (no sink, but functionality must remain)", st)
	}
}

// TestLocalAsyncResumeTraceCarriesUsageEvent — the resume trace now emits a
// `request.usage` event (the same one the main request path emits), so the
// trace reader can attribute the resumed run to its tenant + fuel/bytes. The
// keys must be present and flattened onto the event doc.
func TestLocalAsyncResumeTraceCarriesUsageEvent(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	traceDir := withFileSink(t, pu)

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	seedOp(t, pu, "usage-tst", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "usage-tst/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	resumeRID := continuation.ResumeTraceRID(runID, "usage-tst/100")
	events := readTimeline(t, traceDir, resumeRID)
	u := findEventByName(events, "request.usage")
	if u == nil {
		t.Fatalf("resume trace missing request.usage event; timeline=%v", events)
	}
	for _, k := range []string{"tenant", "fuel", "bytes_out"} {
		if _, ok := u[k]; !ok {
			t.Errorf("request.usage missing %q key; event=%v", k, u)
		}
	}
}

// TestLocalAsyncResumeEmitsBillingUsage — the resume convergence emits a
// BILLING usage.UsageEvent (src="continuation") for the resumed segment, so
// async/suspended request work is metered, not just traced. Distinct from the
// request.usage trace event asserted above.
func TestLocalAsyncResumeEmitsBillingUsage(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	us := newChanUsage()
	pu.Usage = us

	stub := newMCPStub(t)
	stub.OnToolsCall = func(_ []byte) []byte {
		return []byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}
	seedOp(t, pu, "bill-tst", 100, "ask",
		`EXEC "`+mcpExecURL(stub.URL, "ask")+`" WITH mode = "async", timeout = "5s"`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "bill-tst/100", resCh) }()
	rcid, _ := waitFor202(t, resCh)
	runID := resolveRunIDFromRcid(t, pu, rcid)
	if st := waitForRunCompleted(t, pu, runID); st != continuation.StateCompleted {
		t.Fatalf("run state = %q, want completed", st)
	}

	// Billing event arrives on the detached resume goroutine; block for it.
	select {
	case ev := <-us.ch:
		if ev.Src != "continuation" {
			t.Errorf("Src = %q, want continuation", ev.Src)
		}
		if !ev.Billable {
			t.Error("Billable = false, want true")
		}
		if ev.RID != continuation.ResumeTraceRID(runID, "bill-tst/100") {
			t.Errorf("RID = %q, want the resume trace rid", ev.RID)
		}
		if ev.Fuel < 0 {
			t.Errorf("Fuel = %d, want >= 0", ev.Fuel)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resume emitted no billing usage event within 3s")
	}
}
