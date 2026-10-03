package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"

	"github.com/loremlabs/thanks-computer/chassis/config"
	"github.com/loremlabs/thanks-computer/chassis/controlevent"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// The live-run endpoints against a registry with two tenants' runs in it:
// the list is the URL tenant's, an abort by rid ends the run and 404s a
// foreign or finished one, an abort by stack counts what it ended, and
// each needs its capability.
func TestLiveRunEndpoints(t *testing.T) {
	c := newTestController(t, config.Config{})
	c.pu.Live = processor.NewLiveRuns()

	ctxA, doneA := c.pu.Live.Register(context.Background(), "rid-a", "http")
	defer doneA()
	processor.WithTenant(ctxA, "default")
	ctxB, doneB := c.pu.Live.Register(context.Background(), "rid-b", "http")
	defer doneB()
	processor.WithTenant(ctxB, "other")

	vars := func(r *http.Request, kv ...string) *http.Request {
		m := map[string]string{"tenant": "default"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return mux.SetURLVars(r, m)
	}

	// List: the URL tenant's runs only.
	w := httptest.NewRecorder()
	c.handleListRuns(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/runs", nil)), "tnt_default", []string{"run:*:read"}))
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	var list listRunsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 || list.Runs[0].RID != "rid-a" || list.Runs[0].Tenant != "default" {
		t.Fatalf("list = %+v, want rid-a only", list.Runs)
	}

	// Without the capability.
	w = httptest.NewRecorder()
	c.handleListRuns(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodGet, "/v1/tenants/default/runs", nil)), "tnt_default", []string{"opstack:*:*"}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("list without run:*:read: %d", w.Code)
	}
	w = httptest.NewRecorder()
	c.handleAbortRun(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/runs/rid-a/abort", nil), "rid", "rid-a"), "tnt_default", []string{"run:*:read"}))
	if w.Code != http.StatusForbidden {
		t.Fatalf("abort with only run:*:read: %d", w.Code)
	}
	if ctxA.Err() != nil {
		t.Fatal("a refused abort cancelled the run")
	}

	// Another tenant's run: not found, not cancelled.
	w = httptest.NewRecorder()
	c.handleAbortRun(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/runs/rid-b/abort", nil), "rid", "rid-b"), "tnt_default", []string{"run:*:abort"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("abort of a foreign run: %d %s", w.Code, w.Body.String())
	}
	if ctxB.Err() != nil {
		t.Fatal("foreign run cancelled")
	}

	// By rid, with a reason: the run's context carries who and why.
	w = httptest.NewRecorder()
	c.handleAbortRun(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/runs/rid-a/abort", strings.NewReader(`{"reason":"closed the tab"}`)), "rid", "rid-a"), "tnt_default", []string{"run:*:abort"}))
	if w.Code != http.StatusOK {
		t.Fatalf("abort: %d %s", w.Code, w.Body.String())
	}
	var ar abortResponse
	if err := json.Unmarshal(w.Body.Bytes(), &ar); err != nil {
		t.Fatal(err)
	}
	if ar.Aborted != 1 || ar.RID != "rid-a" {
		t.Errorf("abort response = %+v", ar)
	}
	ab, ok := processor.AbortCause(ctxA)
	if !ok || ab.By != "actor_test" || ab.Reason != "closed the tab" {
		t.Fatalf("cause = %+v, %v", ab, ok)
	}

	// Finished: gone from the registry → 404.
	doneA()
	w = httptest.NewRecorder()
	c.handleAbortRun(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/runs/rid-a/abort", nil), "rid", "rid-a"), "tnt_default", []string{"run:*:abort"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("abort of a finished run: %d", w.Code)
	}

	// By stack: two of the tenant's runs in the stack, one of another's.
	ctxC, doneC := c.pu.Live.Register(context.Background(), "rid-c", "http")
	defer doneC()
	processor.WithTenant(ctxC, "default")
	ctxD, doneD := c.pu.Live.Register(context.Background(), "rid-d", "cron")
	defer doneD()
	processor.WithTenant(ctxD, "default")
	// The entry stack is set by the boot handoff; a test drives it through
	// the same exported seam the processor uses for its own tests.
	processor.SetLiveRunEntry(ctxC, "worker")
	processor.SetLiveRunEntry(ctxD, "worker")
	processor.SetLiveRunEntry(ctxB, "worker")

	w = httptest.NewRecorder()
	c.handleAbortStack(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/stacks/worker/abort", strings.NewReader(`{"reason":"deploy"}`)), "name", "worker"), "tnt_default", []string{"run:*:*"}))
	if w.Code != http.StatusOK {
		t.Fatalf("abort stack: %d %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ar); err != nil {
		t.Fatal(err)
	}
	if ar.Aborted != 2 || ar.Stack != "worker" {
		t.Errorf("abort stack response = %+v, want 2 of worker", ar)
	}
	if ctxC.Err() == nil || ctxD.Err() == nil {
		t.Error("stack abort left a run running")
	}
	if ctxB.Err() != nil {
		t.Error("stack abort crossed tenants")
	}

	// Nothing in flight is an answer, not an error.
	w = httptest.NewRecorder()
	c.handleAbortStack(w, withTenantCapsCtx(vars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/stacks/idle/abort", nil), "name", "idle"), "tnt_default", []string{"run:*:*"}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"aborted":0`) {
		t.Fatalf("abort of an idle stack: %d %s", w.Code, w.Body.String())
	}
}

// On a fleet (a feed sink and an artifact store), an abort is applied here
// and also published as a run.abort event for every node: a rid not in
// this process answers 202 rather than 404, and the outbox holds the event
// whose artifact names the tenant, the rid, who and why.
func TestAbortPublishesFleetEvent(t *testing.T) {
	c := newTestController(t, config.Config{FeedSink: "file"})
	withAStore(t, c)
	c.pu.Live = processor.NewLiveRuns()

	w := httptest.NewRecorder()
	r := mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/runs/rid-elsewhere/abort", strings.NewReader(`{"reason":"closed the tab"}`)), map[string]string{"tenant": "default", "rid": "rid-elsewhere"})
	c.handleAbortRun(w, withTenantCapsCtx(r, "tnt_default", []string{"run:*:abort"}))
	if w.Code != http.StatusAccepted {
		t.Fatalf("abort of a run not here on a fleet: %d %s", w.Code, w.Body.String())
	}
	var ar abortResponse
	if err := json.Unmarshal(w.Body.Bytes(), &ar); err != nil {
		t.Fatal(err)
	}
	if ar.Aborted != 0 || !ar.Published {
		t.Fatalf("response = %+v, want aborted 0, published", ar)
	}

	rows, err := c.pu.RuntimeDB.Query(`SELECT artifact_ref, tenant_id FROM control_events_outbox WHERE event_type = ? ORDER BY id`, controlevent.TypeRunAbort)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var ref, tid string
		if err := rows.Scan(&ref, &tid); err != nil {
			t.Fatal(err)
		}
		if tid != "tnt_default" {
			t.Errorf("outbox tenant_id = %q", tid)
		}
		refs = append(refs, ref)
	}
	if len(refs) != 1 {
		t.Fatalf("run.abort outbox rows = %d, want 1", len(refs))
	}
	data, _, err := c.astore.Get(t.Context(), refs[0])
	if err != nil {
		t.Fatal(err)
	}
	var art controlevent.RunAbortArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		t.Fatal(err)
	}
	if art.Tenant != "default" || art.RID != "rid-elsewhere" || art.By != "actor_test" || art.Reason != "closed the tab" || art.At == "" {
		t.Fatalf("artifact = %+v", art)
	}

	// By stack: published too, with the stack.
	w = httptest.NewRecorder()
	r = mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/v1/tenants/default/stacks/worker/abort", nil), map[string]string{"tenant": "default", "name": "worker"})
	c.handleAbortStack(w, withTenantCapsCtx(r, "tnt_default", []string{"run:*:abort"}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"published":true`) {
		t.Fatalf("stack abort on a fleet: %d %s", w.Code, w.Body.String())
	}
}
