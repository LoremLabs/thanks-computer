package processor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/trace"
)

// The registry on its own: register, pin, list, abort by rid, abort by
// stack, tenant scoping, removal.
func TestLiveRunsRegistry(t *testing.T) {
	r := NewLiveRuns()
	ctxA, doneA := r.Register(context.Background(), "rid-a", "http")
	ctxB, doneB := r.Register(context.Background(), "rid-b", "cron")
	ctxC, doneC := r.Register(context.Background(), "rid-c", "http")
	defer doneA()
	defer doneB()
	defer doneC()

	// The pin sites, as the processor calls them.
	WithTenant(ctxA, "acme")
	liveRunFromContext(ctxA).setEntry("web")
	liveRunFromContext(ctxA).setStage("web", "web/50")
	WithTenant(ctxB, "acme")
	liveRunFromContext(ctxB).setEntry("worker")
	liveRunFromContext(ctxB).setStage("tools", "tools/10")
	WithTenant(ctxC, "other")
	liveRunFromContext(ctxC).setEntry("web")

	if n := r.Count(); n != 3 {
		t.Fatalf("Count = %d, want 3", n)
	}
	acme := r.List("acme")
	if len(acme) != 2 || acme[0].RID != "rid-a" || acme[1].RID != "rid-b" {
		t.Fatalf("List(acme) = %+v, want rid-a, rid-b", acme)
	}
	if acme[0].Entry != "web" || acme[0].Stage != "web/50" || acme[0].Src != "http" {
		t.Errorf("rid-a snapshot = %+v", acme[0])
	}
	if all := r.List(""); len(all) != 3 {
		t.Errorf("List(\"\") = %d runs, want 3", len(all))
	}

	// Another tenant's run is not reachable by rid.
	if r.Abort("acme", "rid-c", "me", "") {
		t.Fatal("Abort across tenants succeeded")
	}
	if ctxC.Err() != nil {
		t.Fatal("rid-c was cancelled by a foreign abort")
	}
	// An unknown rid.
	if r.Abort("acme", "rid-zzz", "me", "") {
		t.Fatal("Abort of an unknown rid succeeded")
	}

	// By rid: the context ends with the AbortError as its cause.
	if !r.Abort("acme", "rid-a", "matt", "closed the tab") {
		t.Fatal("Abort(rid-a) = false")
	}
	select {
	case <-ctxA.Done():
	case <-time.After(time.Second):
		t.Fatal("rid-a context not cancelled")
	}
	ab, ok := AbortCause(ctxA)
	if !ok || ab.By != "matt" || ab.Reason != "closed the tab" {
		t.Fatalf("AbortCause(rid-a) = %+v, %v", ab, ok)
	}
	if ab.Error() != "aborted by matt: closed the tab" {
		t.Errorf("AbortError.Error() = %q", ab.Error())
	}
	// A child context (an op's own timeout) inherits the cause.
	child, cancelChild := context.WithTimeout(ctxA, time.Minute)
	defer cancelChild()
	if _, ok := AbortCause(child); !ok {
		t.Error("child context does not report the abort cause")
	}
	// Listed as aborting until its goroutine returns; a second abort is a
	// true no-op.
	if got := r.List("acme"); got[0].AbortedBy != "matt" {
		t.Errorf("aborted run not marked: %+v", got[0])
	}
	if !r.Abort("acme", "rid-a", "someone-else", "") {
		t.Error("second Abort = false")
	}
	if ab, _ := AbortCause(ctxA); ab.By != "matt" {
		t.Errorf("second abort rewrote the cause: %+v", ab)
	}

	// By stack: matches the entry stack or the current one; the other
	// tenant's run that entered at "web" is untouched.
	if n := r.AbortStack("acme", "tools", "deploy", ""); n != 1 {
		t.Fatalf("AbortStack(tools) = %d, want 1 (rid-b, now in tools/10)", n)
	}
	if ctxB.Err() == nil {
		t.Fatal("rid-b not cancelled by stack abort")
	}
	if n := r.AbortStack("acme", "web", "deploy", ""); n != 1 {
		// rid-a again (already aborted, still registered) — not rid-c.
		t.Fatalf("AbortStack(web) = %d, want 1", n)
	}
	if ctxC.Err() != nil {
		t.Fatal("rid-c (tenant other) cancelled by acme's stack abort")
	}

	// Removal: done is idempotent, and a cancel cause is not reported as an
	// abort for a run that merely finished.
	doneA()
	doneA()
	if r.Count() != 2 {
		t.Errorf("Count after done = %d, want 2", r.Count())
	}
	doneC()
	if _, ok := AbortCause(ctxC); ok {
		t.Error("a finished run reports an abort cause")
	}

	// Nil-safety: a Unit built without New.
	var none *LiveRuns
	ctx, done := none.Register(context.Background(), "x", "http")
	done()
	if ctx == nil || none.Count() != 0 || none.Abort("", "x", "", "") || none.AbortStack("", "s", "", "") != 0 || len(none.List("")) != 0 {
		t.Error("nil registry is not inert")
	}
}

// A run registered the way the server does it, aborted while a sync op is
// in flight: the scope loop's cancel branch records the op and the run as
// `aborted` by whom, and answers the client with the code and a 503.
func TestRunAbortedWhileOpInFlight(t *testing.T) {
	pu, _ := newTestUnit(t)
	pu.Live = NewLiveRuns() // the test Unit is a literal; New wires one

	h := blockingHandler{started: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(func() { close(h.release) })
	pu.Handle([]byte("txco://block-stub"), h)

	// The run is pinned to a tenant, so the op must be that tenant's.
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme', 'acme', 'acme', '')`); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (tenant_id, stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, ?, '', '')`,
		"tnt_acme", "abtest", 0, "c", `EXEC "txco://block-stub"`,
	); err != nil {
		t.Fatalf("seed op: %v", err)
	}

	rt := &recordingTracer{}
	ctx, done := pu.Live.Register(context.Background(), "rid-1", "http")
	defer done()
	ctx = trace.WithContext(ctx, rt)
	resCh := make(chan event.Payload, 1)

	runErr := make(chan error, 1)
	go func() { runErr <- pu.Run(ctx, `{"_txc":{"tenant":"acme"}}`, "abtest/0", resCh) }()

	<-h.started
	// The run is listed, pinned to its tenant and its scope.
	runs := pu.Live.List("acme")
	if len(runs) != 1 || runs[0].RID != "rid-1" || runs[0].Stage != "abtest/0" || runs[0].Stack != "abtest" {
		t.Fatalf("List(acme) = %+v", runs)
	}
	if !pu.Live.Abort("acme", "rid-1", "matt", "test") {
		t.Fatal("Abort = false")
	}

	err := <-runErr
	if err == nil || !strings.HasPrefix(err.Error(), "aborted by matt: test while running abtest/0") {
		t.Fatalf("Run err = %v", err)
	}
	st := rt.stepByOpName("c")
	if st == nil {
		t.Fatalf("no step for the in-flight op; recorded=%+v", rt.steps)
	}
	if st.Status != "aborted" || !strings.Contains(st.Error, "aborted by matt") {
		t.Errorf("step = %q / %q, want aborted by matt", st.Status, st.Error)
	}
	select {
	case res := <-resCh:
		if res.Type != event.ErrorStr {
			t.Errorf("response type = %v, want ErrorStr", res.Type)
		}
		if gjson.Get(res.Raw, "error.code").String() != "txco_run_aborted" ||
			gjson.Get(res.Raw, "_txc.web.res.status").Int() != 503 ||
			gjson.Get(res.Raw, "err").String() != "aborted" ||
			gjson.Get(res.Raw, "_txc.tenant").String() != "acme" {
			t.Errorf("abort response = %s", res.Raw)
		}
	default:
		t.Fatal("no response on resCh")
	}
}

// Work that outlives its request (a continuable op's detached exec) shares
// the run's entry: the entry stays listed after the request returns, an
// abort cancels both contexts, and the entry goes when the last holder does.
func TestLiveRunsAttach(t *testing.T) {
	r := NewLiveRuns()
	reqCtx, reqDone := r.Register(context.Background(), "rid-1", "http")
	WithTenant(reqCtx, "acme")
	workCtx, workDone := r.Attach(context.Background(), "rid-1", "http")
	liveRunFromContext(workCtx).setStage("loop", "loop/500")

	reqDone() // the request answered its client; the work runs on
	if got := r.List("acme"); len(got) != 1 || got[0].RID != "rid-1" || got[0].Stage != "loop/500" {
		t.Fatalf("after the request returned: %+v", got)
	}
	if !r.Abort("acme", "rid-1", "matt", "stop") {
		t.Fatal("Abort = false")
	}
	if workCtx.Err() == nil || reqCtx.Err() == nil {
		t.Fatal("abort did not cancel every holder")
	}
	if ab, ok := AbortCause(workCtx); !ok || ab.By != "matt" {
		t.Fatalf("work cause = %+v, %v", ab, ok)
	}
	workDone()
	if r.Count() != 0 {
		t.Fatalf("entry still registered after the last holder: %d", r.Count())
	}

	// Attaching to a rid nobody registered (a resumed run) makes the entry,
	// pinned to the tenant on the context.
	ctx2, done2 := r.Attach(WithTenant(context.Background(), "acme"), "rid-2", "http")
	defer done2()
	if got := r.List("acme"); len(got) != 1 || got[0].RID != "rid-2" {
		t.Fatalf("attach without register: %+v", got)
	}
	// Attaching to a run already aborted is cancelled at once.
	r.Abort("acme", "rid-2", "matt", "")
	ctx3, done3 := r.Attach(context.Background(), "rid-2", "http")
	defer done3()
	if ctx3.Err() == nil || ctx2.Err() == nil {
		t.Fatal("late attachment to an aborted run is not cancelled")
	}
}
