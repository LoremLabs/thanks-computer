package processor

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// asyncStub is an httptest worker that captures the inbound continuation
// envelope + token and acks 202.
type asyncStub struct {
	mu      sync.Mutex
	opc     string
	token   string
	runCont string
	input   string
	srv     *httptest.Server
}

func newAsyncStub(t *testing.T) *asyncStub {
	t.Helper()
	s := &asyncStub{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.opc = gjson.GetBytes(body, "_txc.op_continuation_id").String()
		s.runCont = gjson.GetBytes(body, "_txc.run_continuation_id").String()
		s.input = gjson.GetBytes(body, "input").Raw
		s.token = r.Header.Get("X-Txco-Continuation-Token")
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"job-9"}`))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *asyncStub) captured() (opc, token, runCont, input string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opc, s.token, s.runCont, s.input
}

func seedOp(t *testing.T, pu *Unit, stack string, scope int, name, rule string) {
	t.Helper()
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', '')`,
		stack, scope, name, rule); err != nil {
		t.Fatalf("seed %s/%d: %v", stack, scope, err)
	}
}

func waitFor202(t *testing.T, resCh chan event.Payload) (rcid, loc string) {
	t.Helper()
	select {
	case p := <-resCh:
		if got := gjson.Get(p.Raw, "_txc.web.res.status").Int(); got != 202 {
			t.Fatalf("status = %d, want 202", got)
		}
		loc = gjson.Get(p.Raw, "_txc.web.res.headers.location.0").String()
		bj, _ := base64.StdEncoding.DecodeString(gjson.Get(p.Raw, "_txc.web.res.body").String())
		rcid = gjson.GetBytes(bj, "continuation").String()
		return rcid, loc
	case <-time.After(5 * time.Second):
		t.Fatal("no 202 within 5s")
		return "", ""
	}
}

// TestContinuationSuspendResumeEndToEnd: async op at acme/100 suspends
// (client 202), worker posts back, run resumes and advances to the sync
// render at acme/200, final result derivable as completed and merges the
// worker output + the EMIT.
func TestContinuationSuspendResumeEndToEnd(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t)

	seedOp(t, pu, "acme", 100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async"`)
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()
	rcid, loc := waitFor202(t, resCh)
	// Location is now the same-URL poll marker (dedicated GET removed).
	// Test envelope has no _txc.web.req.url.path → defaults to "/".
	if rcid == "" || loc != "/?_txc.continuation="+rcid {
		t.Fatalf("bad 202: rcid=%q loc=%q", rcid, loc)
	}

	opc, token, rc, in := stub.captured()
	if opc == "" || token == "" {
		t.Fatalf("worker missing opc/token: %q %q", opc, token)
	}
	if rc != rcid {
		t.Fatalf("worker run_continuation_id=%q want %q", rc, rcid)
	}
	if in != "{}" {
		t.Fatalf("worker input=%s want {}", in)
	}

	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}
	if continuation.HashToken(token) != lk.TokenHash {
		t.Fatal("token hash mismatch")
	}
	if st, _ := pu.Runs.RunState(ctx, lk.RunID); st != continuation.StateWaiting {
		t.Fatalf("pre-callback state=%q want waiting", st)
	}

	rec, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "completed", []byte(`{"summary":"S"}`))
	if err != nil || !rec {
		t.Fatalf("RecordTerminal rec=%v err=%v", rec, err)
	}
	if rec2, _ := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "completed", []byte(`{"summary":"X"}`)); rec2 {
		t.Fatal("duplicate RecordTerminal must not re-record")
	}

	ss, _ := pu.Runs.ReadStageSuspended(ctx, lk.RunID, lk.Stage)
	if st, _ := pu.Runs.StageState(ctx, lk.RunID, lk.Stage, ss.Manifest); st != continuation.StateResumable {
		t.Fatalf("state=%q want resumable", st)
	}
	if won, _ := pu.Runs.ClaimResume(ctx, lk.RunID, lk.Stage); !won {
		t.Fatal("ClaimResume should win")
	}
	if err := pu.Resume(ctx, lk.RunID, lk.Stage); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if st, err := pu.Runs.RunState(ctx, lk.RunID); err != nil || st != continuation.StateCompleted {
		t.Fatalf("post-resume state=%q err=%v want completed", st, err)
	}
	res, ok, _ := pu.Runs.ReadResult(ctx, lk.RunID)
	if !ok {
		t.Fatal("no result.json")
	}
	if gjson.GetBytes(res, "summary").String() != "S" {
		t.Fatalf("result missing worker output: %s", res)
	}
	if !gjson.GetBytes(res, "resumed").Bool() {
		t.Fatalf("result missing acme/200 EMIT: %s", res)
	}
	if won2, _ := pu.Runs.ClaimResume(ctx, lk.RunID, lk.Stage); won2 {
		t.Fatal("second ClaimResume must lose (single resumer)")
	}
}

// TestContinuationResumeAdvancesPastBarrierTenantScoped reproduces the
// real-chassis bug: ops are tenant-scoped (tenant_id), the request pins
// `_txc.tenant`, and resume MUST re-pin that tenant or OpsForStage finds
// nothing and the run completes stuck at the barrier scope (no advance
// to the render scope). The pre-existing e2e tests used NULL-tenant ops
// and an empty _txc.tenant, so they never exercised this path.
func TestContinuationResumeAdvancesPastBarrierTenantScoped(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, _ := filestore.New(t.TempDir())
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t)

	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme','acme','acme','')`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	mustOp := func(scope int, name, rule string) {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES ('tnt_acme', ?, ?, ?, ?, '', '')`,
			"site", scope, name, rule); err != nil {
			t.Fatalf("seed op %d/%s: %v", scope, name, err)
		}
	}
	mustOp(100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async"`)
	mustOp(200, "render", `EMIT .rendered = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, "site/100", resCh) }()
	waitFor202(t, resCh)

	opc, _, _, _ := stub.captured()
	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}
	if _, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "completed", []byte(`{"summary":"S"}`)); err != nil {
		t.Fatalf("RecordTerminal: %v", err)
	}
	if won, _ := pu.Runs.ClaimResume(ctx, lk.RunID, lk.Stage); !won {
		t.Fatal("ClaimResume should win")
	}
	if err := pu.Resume(ctx, lk.RunID, lk.Stage); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if st, _ := pu.Runs.RunState(ctx, lk.RunID); st != continuation.StateCompleted {
		t.Fatalf("state=%q, want completed", st)
	}
	res, ok, _ := pu.Runs.ReadResult(ctx, lk.RunID)
	if !ok {
		t.Fatal("no result.json")
	}
	if gjson.GetBytes(res, "summary").String() != "S" {
		t.Fatalf("result missing worker output: %s", res)
	}
	// The decisive assertion: the run advanced PAST the barrier scope
	// (site/100) to the render scope (site/200). Pre-fix this is absent
	// because tenant-scoped OpsForStage returned nothing on resume.
	if !gjson.GetBytes(res, "rendered").Bool() {
		t.Fatalf("run did not advance past barrier to render scope: %s", res)
	}
}

// TestContinuationResumeUsesOpstackSnapshot proves strict cross-version
// resume: the opstack is frozen into an immutable continuation doc at
// suspend, so a `txco apply` (here simulated by deleting the live ops
// after suspend) cannot change what the in-flight run executes. Without
// the snapshot, resume would read the now-empty live ops and never reach
// the render scope.
func TestContinuationResumeUsesOpstackSnapshot(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, _ := filestore.New(t.TempDir())
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t)

	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme','acme','acme','')`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	mustOp := func(scope int, name, rule string) {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES ('tnt_acme', ?, ?, ?, ?, '', '')`,
			"site", scope, name, rule); err != nil {
			t.Fatalf("seed op %d/%s: %v", scope, name, err)
		}
	}
	mustOp(100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async"`)
	mustOp(200, "render", `EMIT .rendered = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, "site/100", resCh) }()
	waitFor202(t, resCh)

	opc, _, _, _ := stub.captured()
	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}

	// The snapshot hash is recorded on both the run and the stage docs.
	rc, err := pu.Runs.ReadRunCreated(ctx, lk.RunID)
	if err != nil || rc.StackVersionID == "" {
		t.Fatalf("run-created snapshot hash empty (err=%v)", err)
	}
	ss, _ := pu.Runs.ReadStageSuspended(ctx, lk.RunID, lk.Stage)
	if ss.StackVersion != rc.StackVersionID {
		t.Fatalf("stage hash %q != run hash %q", ss.StackVersion, rc.StackVersionID)
	}
	if _, err := pu.Runs.ReadOpstackSnapshot(ctx, lk.RunID); err != nil {
		t.Fatalf("ReadOpstackSnapshot: %v", err)
	}

	// Simulate a `txco apply` between suspend and resume: the live opstack
	// for this tenant is wiped. A live-opstack resume would now find no
	// render rule and never advance.
	if _, err := pu.Dbc.Db.Exec(`DELETE FROM ops WHERE tenant_id = 'tnt_acme'`); err != nil {
		t.Fatalf("wipe live ops: %v", err)
	}

	if _, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "completed", []byte(`{"summary":"S"}`)); err != nil {
		t.Fatalf("RecordTerminal: %v", err)
	}
	if won, _ := pu.Runs.ClaimResume(ctx, lk.RunID, lk.Stage); !won {
		t.Fatal("ClaimResume should win")
	}
	if err := pu.Resume(ctx, lk.RunID, lk.Stage); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if st, _ := pu.Runs.RunState(ctx, lk.RunID); st != continuation.StateCompleted {
		t.Fatalf("state=%q, want completed", st)
	}
	res, ok, _ := pu.Runs.ReadResult(ctx, lk.RunID)
	if !ok {
		t.Fatal("no result.json")
	}
	if gjson.GetBytes(res, "summary").String() != "S" {
		t.Fatalf("result missing worker output: %s", res)
	}
	// Decisive: render fired from the SNAPSHOT even though the live ops
	// were deleted before resume.
	if !gjson.GetBytes(res, "rendered").Bool() {
		t.Fatalf("resume did not use opstack snapshot (render absent): %s", res)
	}
}

// TestContinuationResumeIgnoresWorkerTenant is the security regression:
// a worker whose callback output injects `_txc.tenant` pointing at
// another tenant MUST NOT redirect the resume. The tenant pin on resume
// comes only from the chassis-stamped suspended envelope, never from
// worker-supplied output (mirrors Run's immutable-tenant invariant).
func TestContinuationResumeIgnoresWorkerTenant(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, _ := filestore.New(t.TempDir())
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t)

	for _, tn := range []struct{ id, slug string }{
		{"tnt_acme", "acme"}, {"tnt_intruder", "intruder"},
	} {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES (?, ?, ?, '')`,
			tn.id, tn.slug, tn.slug); err != nil {
			t.Fatalf("seed tenant %s: %v", tn.slug, err)
		}
	}
	mustOpT := func(tid string, scope int, name, rule string) {
		if _, err := pu.Dbc.Db.Exec(
			`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, ?, '', '')`,
			tid, "site", scope, name, rule); err != nil {
			t.Fatalf("seed op %s %d/%s: %v", tid, scope, name, err)
		}
	}
	mustOpT("tnt_acme", 100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async"`)
	mustOpT("tnt_acme", 200, "render", `EMIT .rendered = true`)
	// Same stack/scope under the OTHER tenant — a pin escape would run
	// this and leave a "pwned" marker.
	mustOpT("tnt_intruder", 200, "render", `EMIT .pwned = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{"_txc":{"tenant":"acme"}}`, "site/100", resCh) }()
	waitFor202(t, resCh)

	opc, _, _, _ := stub.captured()
	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}
	// Hostile worker output: tries to flip the run into "intruder".
	if _, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op,
		"completed", []byte(`{"_txc":{"tenant":"intruder"},"summary":"S"}`)); err != nil {
		t.Fatalf("RecordTerminal: %v", err)
	}
	if won, _ := pu.Runs.ClaimResume(ctx, lk.RunID, lk.Stage); !won {
		t.Fatal("ClaimResume should win")
	}
	if err := pu.Resume(ctx, lk.RunID, lk.Stage); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	res, ok, _ := pu.Runs.ReadResult(ctx, lk.RunID)
	if !ok {
		t.Fatal("no result.json")
	}
	if !gjson.GetBytes(res, "rendered").Bool() {
		t.Fatalf("acme render did not run (tenant pin lost): %s", res)
	}
	if gjson.GetBytes(res, "pwned").Exists() {
		t.Fatalf("SECURITY: worker-injected _txc.tenant escaped into intruder's stack: %s", res)
	}
}

// TestContinuationSuspendBrowserRedirects: a browser (Accept: text/html)
// that suspends gets a 303 to the poll URL — not raw JSON it can't act
// on. Machines still get the 202+JSON (covered by the e2e test above).
func TestContinuationSuspendBrowserRedirects(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, _ := filestore.New(t.TempDir())
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t)
	seedOp(t, pu, "acme", 100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async"`)

	raw := `{}`
	raw, _ = sjson.Set(raw, "_txc.web.req.url.path", "/go")
	raw, _ = sjson.Set(raw, "_txc.web.req.headers.Accept.0",
		"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), raw, "acme/100", resCh) }()

	var p event.Payload
	select {
	case p = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no response within 5s")
	}
	if got := gjson.Get(p.Raw, "_txc.web.res.status").Int(); got != 303 {
		t.Fatalf("status=%d want 303 (browser suspend redirect)", got)
	}
	loc := gjson.Get(p.Raw, "_txc.web.res.headers.location.0").String()
	opc, _, _, _ := stub.captured()
	lk, err := pu.Runs.ResolveOpContinuation(context.Background(), opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}
	// Resolve rcid via run-created lookup is internal; assert the
	// Location shape + that it carries a rc_ marker on the right path.
	if !strings.HasPrefix(loc, "/go?_txc.continuation=rc_") {
		t.Fatalf("Location=%q want /go?_txc.continuation=rc_…", loc)
	}
	if gjson.Get(p.Raw, "_txc.web.res.headers.cache-control.0").String() != "no-store" {
		t.Fatalf("redirect missing Cache-Control: no-store")
	}
	_ = lk
}

// TestContinuationResumeIsTraced fixes the user-reported gap: the
// resumed pipeline (merge → advance → render) must appear in the trace
// list. When a trace sink is attached to the resume ctx (as the
// worker-callback handler now does, keyed `resume-<runID>`), Resume's
// stages are recorded. NoopTracer ⇒ nothing, exactly as before.
func TestContinuationResumeIsTraced(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, _ := filestore.New(t.TempDir())
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t)

	seedOp(t, pu, "acme", 100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async"`)
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)

	traceDir := t.TempDir()
	sink, err := trace.NewFileSink(traceDir, trace.ParseMode("full"))
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	hasStep := func(rid, prefix string) bool {
		ents, derr := os.ReadDir(filepath.Join(traceDir, "requests", rid, "steps"))
		if derr != nil {
			return false
		}
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), prefix) {
				return true
			}
		}
		return false
	}

	// Suspend side: trace the originating request. The barrier scope
	// (acme/300 research) must be visible as a `pending` async step —
	// not a gap between scope 200 and request.end.
	const origRID = "req-orig-1"
	otracer := sink.Begin(trace.RequestInfo{RID: origRID, Src: "http", Stack: "acme/100", StartedAt: time.Now()})
	octx := trace.WithContext(context.Background(), otracer)
	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(octx, `{}`, "acme/100", resCh) }()
	waitFor202(t, resCh)
	otracer.End("ok", nil)

	if !hasStep(origRID, "0100-research") {
		t.Fatalf("suspend trace missing the async barrier step (0300-research)")
	}

	opc, _, _, _ := stub.captured()
	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}
	if _, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "completed", []byte(`{"summary":"S"}`)); err != nil {
		t.Fatalf("RecordTerminal: %v", err)
	}
	if won, _ := pu.Runs.ClaimResume(ctx, lk.RunID, lk.Stage); !won {
		t.Fatal("ClaimResume should win")
	}

	// Resume side: a distinct, per-stage trace that shows BOTH the
	// barrier scope's async op (0300-research, completed with the worker
	// result) AND the post-resume render (1000-render).
	rid := continuation.ResumeTraceRID(lk.RunID, lk.Stage)
	tracer := sink.Begin(trace.RequestInfo{RID: rid, Src: "continuation", Stack: lk.Stage, StartedAt: time.Now()})
	rctx := trace.WithContext(context.Background(), tracer)
	if err := pu.Resume(rctx, lk.RunID, lk.Stage); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	res, _, _ := pu.Runs.ReadResult(context.Background(), lk.RunID)
	tracer.End("ok", res)

	if _, err := os.Stat(filepath.Join(traceDir, "requests", rid, "timeline.jsonl")); err != nil {
		t.Fatalf("resume not traced: %v", err)
	}
	if !hasStep(rid, "0100-research") {
		t.Fatalf("resume trace missing the barrier op step (0300-research) — user-reported gap")
	}
	if !hasStep(rid, "0200-render") {
		t.Fatalf("resume trace missing the post-resume render step (0200-render)")
	}
}

// TestContinuationFailedOpFailsStage: a worker-reported failure fails the
// stage; the run derives failed and does not advance.
func TestContinuationFailedOpFailsStage(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, _ := filestore.New(t.TempDir())
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t)

	seedOp(t, pu, "acme", 100, "research", `EXEC "`+stub.srv.URL+`" WITH mode = "async"`)
	seedOp(t, pu, "acme", 200, "render", `EMIT .resumed = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "acme/100", resCh) }()
	waitFor202(t, resCh)

	opc, _, _, _ := stub.captured()
	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}

	if _, err := pu.Runs.RecordTerminal(ctx, lk.RunID, lk.Stage, lk.Ordinal, lk.Op, "failed", []byte(`{"error":{"message":"model timeout"}}`)); err != nil {
		t.Fatalf("RecordTerminal failed: %v", err)
	}
	if won, _ := pu.Runs.ClaimResume(ctx, lk.RunID, lk.Stage); !won {
		t.Fatal("ClaimResume should win")
	}
	if err := pu.Resume(ctx, lk.RunID, lk.Stage); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if st, err := pu.Runs.RunState(ctx, lk.RunID); err != nil || st != continuation.StateFailed {
		t.Fatalf("state=%q err=%v, want failed", st, err)
	}
	if _, ok, _ := pu.Runs.ReadResult(ctx, lk.RunID); ok {
		t.Fatal("failed run must not have result.json")
	}
}
