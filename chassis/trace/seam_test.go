package trace

import (
	"context"
	"testing"
	"time"
)

// TestSeamWriteThenRead proves the write seam (Sink via Open) and the
// read seam (Reader via OpenReader) compose: a trace written through
// the registered "file" Sink is listable/gettable through the
// registered "file" Reader, and the ETag drives a 304. This exercises
// the contract independent of the admin HTTP layer.
func TestSeamWriteThenRead(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	sink, err := Open("file", StoreConfig{Dir: dir, Mode: ModeFull})
	if err != nil {
		t.Fatalf("Open file sink: %v", err)
	}
	rid := "CbSeamTest0001"
	tr := sink.Begin(RequestInfo{
		RID: rid, Src: "web POST /x", Tenant: "acme", Stack: "default",
		StartedAt: time.Now(), Payload: []byte(`{"hello":"world"}`),
	})
	tr.Event(TimelineEvent{Ts: time.Now(), Event: "stage.jump", Fields: map[string]any{"to": "hello/0"}})
	tr.Step(StepInfo{
		Stack: "default", Scope: 100, Name: "parse", Operation: "txcl://parse",
		Transport: "txcl", StartedAt: time.Now(), FinishedAt: time.Now(),
		Status: "ok", Input: []byte(`{"in":1}`), Output: []byte(`{"out":1}`),
	})
	tr.End("ok", "", []byte(`{"done":true}`))
	if err := sink.Close(ctx); err != nil {
		t.Fatalf("sink close: %v", err)
	}

	rdr, err := OpenReader("file", StoreConfig{Dir: dir, Mode: ModeFull})
	if err != nil {
		t.Fatalf("OpenReader file: %v", err)
	}

	// List: the trace shows up, with an ETag.
	res, err := rdr.List(ctx, ListQuery{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if res.Total != 1 || len(res.Traces) != 1 || res.Traces[0].RID != rid {
		t.Fatalf("List = total %d traces %d first %q; want 1/1/%s",
			res.Total, len(res.Traces), firstSummaryRID(res.Traces), rid)
	}
	if res.ETag == "" {
		t.Fatal("List returned empty ETag")
	}
	if res.Traces[0].Route != "hello/0" || res.Traces[0].Status != "ok" {
		t.Errorf("summary route/status = %q/%q; want hello/0/ok",
			res.Traces[0].Route, res.Traces[0].Status)
	}

	// Same ETag ⇒ NotModified (the long-poll 304 path).
	again, err := rdr.List(ctx, ListQuery{Limit: 10, IfNoneMatch: res.ETag})
	if err != nil {
		t.Fatalf("List(if-none-match): %v", err)
	}
	if !again.NotModified {
		t.Error("expected NotModified when re-sending the ETag")
	}

	// Get: aggregated detail with the step and embedded payloads.
	d, err := rdr.Get(ctx, rid, true)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.RID != rid || d.Status != "ok" || len(d.Steps) != 1 {
		t.Fatalf("detail rid/status/steps = %q/%q/%d; want %s/ok/1",
			d.RID, d.Status, len(d.Steps), rid)
	}
	if d.Steps[0].Name != "parse" || d.In == nil || d.Out == nil {
		t.Errorf("detail step/in/out wrong: step=%q in=%v out=%v",
			d.Steps[0].Name, d.In, d.Out)
	}

	// Missing rid ⇒ ErrNotFound.
	if _, err := rdr.Get(ctx, "CbNoSuchTrace", false); err != ErrNotFound {
		t.Errorf("Get(missing) err = %v; want ErrNotFound", err)
	}

	// noop reader: empty list, every rid NotFound.
	nr, _ := OpenReader("noop", StoreConfig{})
	nres, _ := nr.List(ctx, ListQuery{})
	if len(nres.Traces) != 0 {
		t.Errorf("noop reader list not empty: %d", len(nres.Traces))
	}
	if _, err := nr.Get(ctx, rid, false); err != ErrNotFound {
		t.Errorf("noop Get err = %v; want ErrNotFound", err)
	}
}

func firstSummaryRID(s []Summary) string {
	if len(s) == 0 {
		return ""
	}
	return s[0].RID
}
