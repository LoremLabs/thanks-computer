package processor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/continuation"
	"github.com/loremlabs/thanks-computer/chassis/continuation/filestore"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// TestDeferredJoinSuspendResume is the core deferred-join path: a long async
// op is dispatched at scope 100 with join_at_scope=200 WITHOUT suspending; an
// independent sync scope (150) runs in-request; the run reaches the join floor
// (200) before the worker has answered, so it suspends there (client 202).
// The worker then completes, the deferred terminal is recorded, and resume
// merges it AND RE-RUNS the join scope's own render op (the decisive
// divergence from the same-scope barrier — render at 200 never ran before).
func TestDeferredJoinSuspendResume(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)
	stub := newAsyncStub(t) // acks 202 only — does NOT call back

	seedOp(t, pu, "site", 100, "research",
		`EXEC "`+stub.srv.URL+`" WITH mode = "async", join_at_scope = 200`)
	seedOp(t, pu, "site", 150, "independent", `EMIT .independent = true`)
	seedOp(t, pu, "site", 200, "render", `EMIT .rendered = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "site/100", resCh) }()

	// The run dispatched the deferred op, ran scope 150 in-request, then hit
	// the join floor at 200 with no terminal yet → 202.
	rcid, loc := waitFor202(t, resCh)
	if rcid == "" || loc != "/?_txc.continuation="+rcid {
		t.Fatalf("bad 202: rcid=%q loc=%q", rcid, loc)
	}

	opc, token, _, _ := stub.captured()
	if opc == "" || token == "" {
		t.Fatalf("worker missing opc/token: %q %q", opc, token)
	}

	ctx := context.Background()
	lk, err := pu.Runs.ResolveOpContinuation(ctx, opc)
	if err != nil {
		t.Fatalf("ResolveOpContinuation: %v", err)
	}
	if !lk.Deferred {
		t.Fatal("op-continuation lookup not marked Deferred")
	}
	if lk.Stage != "" {
		t.Fatalf("deferred lookup Stage=%q, want empty (join scope is dynamic)", lk.Stage)
	}

	// The suspend recorded which dynamic stage we're waiting at.
	stage, exists, err := pu.Runs.ReadDeferredSuspendedAt(ctx, lk.RunID, opc)
	if err != nil || !exists || stage != "site/200" {
		t.Fatalf("ReadDeferredSuspendedAt = (%q,%v,%v), want (site/200,true,nil)", stage, exists, err)
	}
	if st, _ := pu.Runs.RunState(ctx, lk.RunID); st != continuation.StateWaiting {
		t.Fatalf("pre-callback state=%q want waiting", st)
	}

	// Worker completes. Record the deferred terminal (opc-keyed) and drive
	// resume exactly as the callback handler does.
	rec, err := pu.Runs.RecordDeferredTerminal(ctx, lk.RunID, opc, "completed", []byte(`{"summary":"S"}`))
	if err != nil || !rec {
		t.Fatalf("RecordDeferredTerminal rec=%v err=%v", rec, err)
	}
	pu.DriveDeferredResume(lk.RunID, stage)

	if st, err := pu.Runs.RunState(ctx, lk.RunID); err != nil || st != continuation.StateCompleted {
		t.Fatalf("post-resume state=%q err=%v want completed", st, err)
	}
	res, ok, _ := pu.Runs.ReadResult(ctx, lk.RunID)
	if !ok {
		t.Fatal("no result.json")
	}
	// In-request independent scope ran before the suspend.
	if !gjson.GetBytes(res, "independent").Bool() {
		t.Fatalf("independent scope (150) did not run in-request: %s", res)
	}
	// Worker output merged at the join.
	if gjson.GetBytes(res, "summary").String() != "S" {
		t.Fatalf("deferred terminal not merged at join: %s", res)
	}
	// DECISIVE: the join scope's OWN render op ran on resume. A same-scope
	// resume (advanceAfterScope) would NOT re-run it.
	if !gjson.GetBytes(res, "rendered").Bool() {
		t.Fatalf("join scope's own op (render @200) did not run on deferred resume: %s", res)
	}
}

// TestDeferredJoinInRequestMerge: when the worker completes BEFORE the run
// reaches the join floor, the in-request floor check merges the terminal and
// the request finishes synchronously (200, no suspend). The deferred run is
// finalized in-request so the sweeper won't later flag it expired.
func TestDeferredJoinInRequestMerge(t *testing.T) {
	pu, _ := newTestUnit(t)
	fs, err := filestore.New(t.TempDir())
	if err != nil {
		t.Fatalf("filestore: %v", err)
	}
	pu.Runs = continuation.NewRuns(fs)

	// A worker that records its deferred terminal synchronously before acking
	// — so by the time the run reaches the join, the result is already there.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		opc := gjson.GetBytes(body, "_txc.op_continuation_id").String()
		runID := gjson.GetBytes(body, "_txc.run_id").String()
		_, _ = pu.Runs.RecordDeferredTerminal(context.Background(), runID, opc,
			"completed", []byte(`{"summary":"INLINE"}`))
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_id":"j"}`))
	}))
	t.Cleanup(srv.Close)

	seedOp(t, pu, "site", 100, "research",
		`EXEC "`+srv.URL+`" WITH mode = "async", join_at_scope = 200`)
	seedOp(t, pu, "site", 200, "render", `EMIT .rendered = true`)

	resCh := make(chan event.Payload, 1)
	go func() { _ = pu.Run(context.Background(), `{}`, "site/100", resCh) }()

	var p event.Payload
	select {
	case p = <-resCh:
	case <-time.After(5 * time.Second):
		t.Fatal("no response within 5s")
	}
	// Synchronous completion: 0 (unset → no _txc.web) is fine; the key point
	// is it is NOT a 202 suspend.
	if got := gjson.Get(p.Raw, "_txc.web.res.status").Int(); got == 202 {
		t.Fatalf("got a 202 suspend; expected in-request completion: %s", p.Raw)
	}
	if gjson.Get(p.Raw, "summary").String() != "INLINE" {
		t.Fatalf("deferred terminal not merged in-request: %s", p.Raw)
	}
	if !gjson.Get(p.Raw, "rendered").Bool() {
		t.Fatalf("join scope render did not run: %s", p.Raw)
	}

	// The deferred run is finalized in-request (so the sweeper won't expire an
	// already-returned request). The finalize WriteResult runs in the Run
	// goroutine just AFTER the response is emitted, so poll for it.
	ctx := context.Background()
	var runID string
	var state string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ids, _ := pu.Runs.ListRunIDs(ctx)
		if len(ids) == 1 {
			runID = ids[0]
			if st, _ := pu.Runs.RunState(ctx, runID); st == continuation.StateCompleted {
				state = st
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runID == "" {
		t.Fatal("deferred run record not created")
	}
	if state != continuation.StateCompleted {
		t.Fatalf("in-request deferred run not finalized completed (else sweeper false-expires it); last runID=%q", runID)
	}
}
