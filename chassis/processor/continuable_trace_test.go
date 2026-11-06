package processor

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// traceRig is a unit with a real continuation store and a real file sink, so
// a test reads back what a person would: through the trace reader.
type traceRig struct {
	pu   *Unit
	sink trace.Sink
	dir  string
}

func newTraceRig(t *testing.T) *traceRig {
	t.Helper()
	pu, _ := newTestUnit(t)
	fsys, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fsys)
	pu.Conf.TraceDetachedBytes = 1 << 24 // newTestUnit's zero would keep metadata only
	dir := t.TempDir()
	sink, err := trace.NewFileSink(dir, trace.ParseMode("full"))
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	pu.Sink = sink
	return &traceRig{pu: pu, sink: sink, dir: dir}
}

// run starts a traced request the way runWithTrace does, and answers its
// result channel and the function that ends its trace.
func (g *traceRig) run(ctx context.Context, rid, stage string) (chan event.Payload, func()) {
	ctx = context.WithValue(WithIssuedSecrets(ctx), config.CtxKeyRid, rid)
	tr := ScrubbingTracer(ctx, g.sink.Begin(trace.RequestInfo{RID: rid, Src: "http", Stack: stage, StartedAt: time.Now()}))
	ctx = trace.WithContext(ctx, tr)
	resCh := make(chan event.Payload, 4)
	go func() { _ = g.pu.Run(ctx, `{"_txc":{"src":"http"}}`, stage, resCh) }()
	return resCh, func() { tr.End("ok", "", nil) }
}

func (g *traceRig) get(t *testing.T, rid string) trace.RequestDetail {
	t.Helper()
	rd, err := trace.OpenReader("file", trace.StoreConfig{Dir: g.dir, Mode: trace.ModeFull})
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	d, err := rd.Get(context.Background(), rid, true)
	if err != nil {
		t.Fatalf("Get(%s): %v", rid, err)
	}
	return d
}

// ended waits for a trace to close: a run reaches its terminal state a moment
// before its trace is written shut.
func (g *traceRig) ended(t *testing.T, rid string) trace.RequestDetail {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if d := g.get(t, rid); d.Status != "in-flight" {
			return d
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("trace %s never ended", rid)
	return trace.RequestDetail{}
}

func stepsOf(d trace.RequestDetail, stack string) map[string]trace.Step {
	out := map[string]trace.Step{}
	for _, s := range d.Steps {
		if s.Stack == stack {
			out[s.Name] = s
		}
	}
	return out
}

func countNamed(d trace.RequestDetail, name string) int {
	n := 0
	for _, s := range d.Steps {
		if s.Name == name {
			n++
		}
	}
	return n
}

// seedCallee is a caller whose scope 100 calls stack `agent` continuably, and
// a callee of two rules, the first of which takes as long as the stub does.
func seedCallee(t *testing.T, pu *Unit, stubURL, with string) {
	t.Helper()
	seedOp(t, pu, "agent", 100, "think", fmt.Sprintf(`EXEC "%s"`, stubURL))
	seedOp(t, pu, "agent", 200, "after", `EMIT ._call.output.done = true`)
	seedOp(t, pu, "acme", 100, "dispatch", `EXEC "call://agent" WITH mode = "continuable", into = "_d", `+with)
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)
}

// The whole point: a promoted call's steps are on the resume trace, and the
// origin has one pending row for it.
func TestContinuablePromotedCallIsTraced(t *testing.T) {
	g := newTraceRig(t)
	stub, _ := delayedStub(t, 300*time.Millisecond, `{"thought":"yes"}`)
	seedCallee(t, g.pu, stub.URL, `continue_after = "100ms", timeout = "10s"`)

	const rid = "CbPromoted000001"
	resCh, end := g.run(context.Background(), rid, "acme/100")
	rcid, _ := waitFor202(t, resCh)
	end()

	runID := resolveRunIDFromRcid(t, g.pu, rcid)
	if st := waitForRunCompleted(t, g.pu, runID); st != continuation.StateCompleted {
		t.Fatalf("state = %q", st)
	}

	origin := g.ended(t, rid)
	if got := stepsOf(origin, "agent"); len(got) != 0 {
		t.Errorf("the origin holds the callee's steps: %v", got)
	}
	if countNamed(origin, "dispatch") != 1 || stepsOf(origin, "acme")["dispatch"].Status != "pending" {
		t.Errorf("origin dispatch: %+v", origin.Steps)
	}

	resume := g.ended(t, continuation.ResumeTraceRID(runID, "acme/100"))
	callee := stepsOf(resume, "agent")
	think, after := callee["think"], callee["after"]
	if think.Name == "" || after.Name == "" {
		t.Fatalf("the callee's steps are missing from the resume trace: %+v", resume.Steps)
	}
	if think.Out == nil {
		t.Errorf("a step under the ceiling lost its body: %+v", think)
	}
	if n := countNamed(resume, "dispatch"); n != 1 {
		t.Errorf("the op has %d rows on the resume trace, want 1: %+v", n, resume.Steps)
	}
	render := stepsOf(resume, "acme")["render"]
	if render.Name == "" {
		t.Fatalf("the resumed scope is missing: %+v", resume.Steps)
	}
	if !(think.StartedAt < after.StartedAt && after.StartedAt < render.StartedAt) {
		t.Errorf("out of order: think %s, after %s, render %s", think.StartedAt, after.StartedAt, render.StartedAt)
	}
	// The trace spans the work, not just what came after it.
	if !(resume.StartedAt <= think.StartedAt) {
		t.Errorf("trace starts at %s, after the op's first step at %s", resume.StartedAt, think.StartedAt)
	}
}

// A call that beats the timer has its steps on the request's own trace, and
// one row for the op.
func TestContinuableSyncWinCallIsTraced(t *testing.T) {
	g := newTraceRig(t)
	stub, _ := delayedStub(t, 0, `{"thought":"yes"}`)
	seedCallee(t, g.pu, stub.URL, `continue_after = "5s", timeout = "10s"`)

	const rid = "CbSyncWin0000001"
	resCh, end := g.run(context.Background(), rid, "acme/100")
	select {
	case p := <-resCh:
		if p.Type == event.ErrorStr {
			t.Fatalf("run failed: %s", p.Raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	end()

	d := g.ended(t, rid)
	callee := stepsOf(d, "agent")
	if callee["think"].Name == "" || callee["after"].Name == "" {
		t.Fatalf("the callee's steps are missing: %+v", d.Steps)
	}
	if n := countNamed(d, "dispatch"); n != 1 {
		t.Errorf("the op has %d rows, want 1: %+v", n, d.Steps)
	}
	if got := stepsOf(d, "acme")["dispatch"].Transport; got != "continuable" {
		t.Errorf("transport = %q", got)
	}
}

// A secret the request issued is scrubbed by value from everything detached
// work writes, whichever trace it lands on, raw and base64.
func TestContinuableScrubsIssuedSecretsFromDetachedSteps(t *testing.T) {
	const secret = "s3cret-issued-once-0123456789"
	cases := map[string]string{
		"promoted": `continue_after = "100ms", timeout = "10s"`,
		"sync win": `continue_after = "5s", timeout = "10s"`,
	}
	for name, with := range cases {
		t.Run(name, func(t *testing.T) {
			g := newTraceRig(t)
			delay := time.Duration(0)
			if name == "promoted" {
				delay = 300 * time.Millisecond
			}
			// The callee answers the secret, and the envelope carries it on.
			stub, _ := delayedStub(t, delay, `{"password":"`+secret+`","b64":"`+
				base64.StdEncoding.EncodeToString([]byte("pre "+secret+" post"))+`"}`)
			seedCallee(t, g.pu, stub.URL, with)

			ctx := WithIssuedSecrets(context.Background())
			NoteIssuedSecret(ctx, secret)
			const rid = "CbSecrets0000001"
			resCh, end := g.run(ctx, rid, "acme/100")

			var runID string
			p := <-resCh
			if strings.Contains(p.Raw, "continuation") {
				rcid := continuationIDOf(t, p)
				runID = resolveRunIDFromRcid(t, g.pu, rcid)
				waitForRunCompleted(t, g.pu, runID)
			}
			end()
			g.ended(t, rid)
			checked := ""
			if runID != "" {
				g.ended(t, continuation.ResumeTraceRID(runID, "acme/100"))
				checked = continuation.ResumeTraceRID(runID, "acme/100")
			} else {
				checked = rid
			}

			// Not vacuous: the callee's step was written, scrubbed.
			if s := stepsOf(g.get(t, checked), "agent")["think"]; s.Out == nil ||
				!strings.Contains(fmt.Sprint(s.Out), "[REDACTED]") {
				t.Fatalf("the step that carries the secret: %+v", s.Out)
			}
			forms := append([][]byte{[]byte(secret)}, base64Forms(secret)...)
			err := filepath.WalkDir(g.dir, func(path string, d fs.DirEntry, werr error) error {
				if werr != nil || d.IsDir() {
					return werr
				}
				b, rerr := os.ReadFile(path)
				if rerr != nil {
					return rerr
				}
				for _, f := range forms {
					if bytes.Contains(b, f) {
						t.Errorf("%s holds the secret", path)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// continuationIDOf reads the continuation id out of a 202.
func continuationIDOf(t *testing.T, p event.Payload) string {
	t.Helper()
	ch := make(chan event.Payload, 1)
	ch <- p
	rcid, _ := waitFor202(t, ch)
	return rcid
}

// The deadline that fails a detached op also unwinds it, and what it writes as
// it goes is the evidence of why it timed out. It must reach the trace, and
// nothing may be written after the trace has ended.
func TestContinuableTimeoutKeepsItsEvidence(t *testing.T) {
	g := newTraceRig(t)
	stuck := make(chan struct{})
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stuck:
		}
	}))
	t.Cleanup(func() { close(stuck); stub.Close() })
	seedCallee(t, g.pu, stub.URL, `continue_after = "100ms", timeout = "400ms"`)

	const rid = "CbTimeout0000001"
	resCh, end := g.run(context.Background(), rid, "acme/100")
	rcid, _ := waitFor202(t, resCh)
	end()

	runID := resolveRunIDFromRcid(t, g.pu, rcid)
	if st := waitForRunCompleted(t, g.pu, runID); st != continuation.StateFailed {
		t.Fatalf("state = %q, want failed", st)
	}
	resume := g.ended(t, continuation.ResumeTraceRID(runID, "acme/100"))
	think := stepsOf(resume, "agent")["think"]
	if think.Name == "" {
		t.Fatalf("the op that was running at the deadline left no step: %+v", resume.Steps)
	}
	if think.Status == "ok" {
		t.Errorf("think status = %q", think.Status)
	}
	if countNamed(resume, "after") != 0 {
		t.Errorf("a scope after the stuck one ran: %+v", resume.Steps)
	}
}

// A promoted op has one row, and it says what happened: the operation, and how
// long the op itself took, not how long it was waited on. The origin's pending
// row is the sync window, not the time the durable writes took.
func TestContinuablePromotedRowsHaveHonestTimes(t *testing.T) {
	g := newTraceRig(t)
	const work = 400 * time.Millisecond
	stub, _ := delayedStub(t, work, `{"thought":"yes"}`)
	seedCallee(t, g.pu, stub.URL, `continue_after = "100ms", timeout = "10s"`)

	const rid = "CbTimes000000001"
	resCh, end := g.run(context.Background(), rid, "acme/100")
	rcid, _ := waitFor202(t, resCh)
	end()
	runID := resolveRunIDFromRcid(t, g.pu, rcid)
	waitForRunCompleted(t, g.pu, runID)

	row := stepsOf(g.ended(t, continuation.ResumeTraceRID(runID, "acme/100")), "acme")["dispatch"]
	if row.Operation != "call://agent" {
		t.Errorf("operation = %q", row.Operation)
	}
	if row.DurationMs < work.Milliseconds() || row.DurationMs > work.Milliseconds()+500 {
		t.Errorf("the op took about %s, its row says %d ms", work, row.DurationMs)
	}

	pending := stepsOf(g.ended(t, rid), "acme")["dispatch"]
	if pending.DurationMs < 100 || pending.DurationMs > 250 {
		t.Errorf("the pending row should be the sync window, about 100 ms: %d", pending.DurationMs)
	}
}

// Model facts reach a step inside a detached call, whichever model kind it is,
// and a call refused before it reached a provider still shows as one.
func TestContinuableModelCallsCarryTheirFacts(t *testing.T) {
	g := newTraceRig(t)
	withTestBackend(t, "facts-backend", &testBackend{
		name: "facts-backend",
		resp: chat.Response{Text: "hello", Model: "gpt-test", Provider: "facts-backend",
			TokensIn: 123, TokensOut: 45, LatencyMS: 67, Retries: 1},
	})
	stub, _ := delayedStub(t, 300*time.Millisecond, `{}`)
	seedOp(t, g.pu, "agent", 100, "slow", fmt.Sprintf(`EXEC "%s"`, stub.URL))
	seedOp(t, g.pu, "agent", 200, "think", `EXEC "ai://chat" WITH provider = "facts-backend", prompt = "hi"`)
	seedOp(t, g.pu, "agent", 300, "refused", `EXEC "ai://chat" WITH provider = "no-such-provider", prompt = "hi"`)
	seedOp(t, g.pu, "acme", 100, "dispatch", `EXEC "call://agent" WITH mode = "continuable", into = "_d", continue_after = "100ms", timeout = "10s"`)

	const rid = "CbModel000000001"
	resCh, end := g.run(context.Background(), rid, "acme/100")
	rcid, _ := waitFor202(t, resCh)
	end()
	runID := resolveRunIDFromRcid(t, g.pu, rcid)
	waitForRunCompleted(t, g.pu, runID)

	callee := stepsOf(g.ended(t, continuation.ResumeTraceRID(runID, "acme/100")), "agent")
	think := callee["think"]
	if think.Model == nil {
		t.Fatalf("no facts on the model step: %+v", think)
	}
	m := *think.Model
	if m.Kind != "chat" || m.Provider != "facts-backend" || m.Model != "gpt-test" ||
		m.TokensIn != 123 || m.TokensOut != 45 || m.LatencyMS != 67 || m.Retries != 1 {
		t.Errorf("facts = %+v", m)
	}
	if think.Out == nil {
		t.Errorf("the model call lost its body: %+v", think)
	}
	refused := callee["refused"]
	if refused.Model == nil || refused.Model.Kind != "chat" || refused.Model.ErrorCode == "" {
		t.Errorf("a refused call should still read as a model call: %+v", refused.Model)
	}
	if callee["slow"].Model != nil {
		t.Errorf("a step that called no model has facts: %+v", callee["slow"].Model)
	}
}

// Both halves of a run answer the same when asked which pony it was.
func TestContinuableTagsBothTraces(t *testing.T) {
	g := newTraceRig(t)
	stub, _ := delayedStub(t, 300*time.Millisecond, `{}`)
	seedCallee(t, g.pu, stub.URL, `continue_after = "100ms", timeout = "10s", trace_tags = &object("pony", "eager.moss", "run", "run_abc")`)

	const rid = "CbTags0000000001"
	resCh, end := g.run(context.Background(), rid, "acme/100")
	rcid, _ := waitFor202(t, resCh)
	end()
	runID := resolveRunIDFromRcid(t, g.pu, rcid)
	waitForRunCompleted(t, g.pu, runID)

	resumeRID := continuation.ResumeTraceRID(runID, "acme/100")
	for _, id := range []string{rid, resumeRID} {
		d := g.ended(t, id)
		if d.Tags["pony"] != "eager.moss" || d.Tags["run"] != "run_abc" {
			t.Errorf("%s: tags = %v", id, d.Tags)
		}
	}
	rd, err := trace.OpenReader("file", trace.StoreConfig{Dir: g.dir, Mode: trace.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	list, err := rd.List(context.Background(), trace.ListQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Traces) != 2 {
		t.Fatalf("listed %d", len(list.Traces))
	}
	for _, s := range list.Traces {
		if s.Tags["pony"] != "eager.moss" {
			t.Errorf("%s lists tags %v", s.RID, s.Tags)
		}
	}
}

func TestTraceTagsAreBounded(t *testing.T) {
	var pairs []string
	for i := 0; i < 20; i++ {
		pairs = append(pairs, fmt.Sprintf(`"k%d":"v"`, i))
	}
	tags := traceTags(operation.Operation{Meta: `{"trace_tags":{` + strings.Join(pairs, ",") + `}}`})
	if len(tags) != maxTraceTags {
		t.Errorf("kept %d tags", len(tags))
	}

	long := strings.Repeat("é", 100)
	tags = traceTags(operation.Operation{Meta: `{"trace_tags":{"a":"` + long + `","n":3,"o":{},"":"x"}}`})
	if len(tags) != 1 || len(tags["a"]) > maxTraceTagLen || !utf8.ValidString(tags["a"]) {
		t.Errorf("tags = %v", tags)
	}
	if got := traceTags(operation.Operation{Meta: `{"trace_tags":"not an object"}`}); got != nil {
		t.Errorf("tags = %v", got)
	}
	if got := traceTags(operation.Operation{Meta: `{}`}); got != nil {
		t.Errorf("tags = %v", got)
	}
}

// With tracing off nothing downstream is fooled into thinking it is on: a
// wrapped NoopTracer would make a resumed pipeline build relays for nothing.
func TestBeginResumeTraceStaysNoopWhenTracingIsOff(t *testing.T) {
	g := newTraceRig(t)
	ctx, tr, _ := g.pu.BeginResumeTrace(context.Background(), trace.NoopSink{}, "run_x", "acme/100", ResumeTraceOptions{})
	if _, noop := tr.(trace.NoopTracer); !noop {
		t.Fatalf("tracer = %T", tr)
	}
	if _, noop := trace.FromContext(ctx).(trace.NoopTracer); !noop {
		t.Fatalf("ctx tracer = %T", trace.FromContext(ctx))
	}
	if _, tr, _ := g.pu.BeginResumeTrace(context.Background(), nil, "run_x", "acme/100", ResumeTraceOptions{}); tr != nil {
		t.Fatalf("a nil sink gave a tracer: %T", tr)
	}
}

// The resume trace is opened with the run's tenant and the suspended stack,
// which is what lets a stack's redaction hints find their slot.
func TestBeginResumeTraceCarriesTenantAndStack(t *testing.T) {
	g := newTraceRig(t)
	capture := &infoSink{}
	runID, _, err := g.pu.Runs.CreateRun(context.Background(), "acme-tenant", "first", "", "loop/502", "origin", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	_, tr, tenant := g.pu.BeginResumeTrace(context.Background(), capture, runID, "loop/502",
		ResumeTraceOptions{IdleTTL: time.Hour})
	if tenant != "acme-tenant" || tr == nil {
		t.Fatalf("tenant = %q, tracer = %v", tenant, tr)
	}
	got := capture.info
	if got.Tenant != "acme-tenant" || got.RedactStack != "loop" || got.Stack != "loop/502" ||
		got.IdleTTL != time.Hour || got.Src != "continuation" {
		t.Errorf("RequestInfo = %+v", got)
	}
}

type infoSink struct {
	trace.NoopSink
	info trace.RequestInfo
}

func (s *infoSink) Begin(info trace.RequestInfo) trace.RequestTracer {
	s.info = info
	return &recordingNoop{}
}

// recordingNoop is any tracer that is not trace.NoopTracer.
type recordingNoop struct{ trace.NoopTracer }
