package server

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// callRunAbort runs txco://run/abort as a rule of `stack` in `tenant`.
func callRunAbort(t *testing.T, live *processor.LiveRuns, tenant, stack, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	if stack != "" {
		ctx = processor.WithStack(ctx, stack)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	// The envelope claims another tenant: it may not be believed.
	pl, err := runAbort(ctx, live, []byte(`{"_txc":{"tenant":"other"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func TestRunAbortOp(t *testing.T) {
	live := processor.NewLiveRuns()
	ctxA, doneA := live.Register(context.Background(), "rid-a", "http")
	defer doneA()
	processor.WithTenant(ctxA, "acme")
	ctxB, doneB := live.Register(context.Background(), "rid-b", "http")
	defer doneB()
	processor.WithTenant(ctxB, "other")
	processor.SetLiveRunEntry(ctxB, "worker")
	ctxC, doneC := live.Register(context.Background(), "rid-c", "cron")
	defer doneC()
	processor.WithTenant(ctxC, "acme")
	processor.SetLiveRunEntry(ctxC, "worker")

	// Arguments.
	for _, meta := range []string{`{}`, `{"rid":"rid-a","stack":"worker"}`} {
		out := callRunAbort(t, live, "acme", "loop", meta)
		if gjson.Get(out, "_abort.error.code").String() != "txco_run_invalid_arg" {
			t.Errorf("meta %s: %s", meta, out)
		}
	}
	// No tenant in scope.
	if out := callRunAbort(t, live, "", "loop", `{"rid":"rid-a"}`); gjson.Get(out, "_abort.error.code").String() != "txco_run_no_tenant" {
		t.Errorf("untenanted: %s", out)
	}
	// No registry on this node.
	if out := callRunAbort(t, nil, "acme", "loop", `{"rid":"rid-a"}`); gjson.Get(out, "_abort.error.code").String() != "txco_run_disabled" {
		t.Errorf("no registry: %s", out)
	}
	// Another tenant's run — the envelope's claim of that tenant is not
	// believed — is not found.
	if out := callRunAbort(t, live, "acme", "loop", `{"rid":"rid-b"}`); gjson.Get(out, "_abort.error.code").String() != "txco_run_not_live" {
		t.Errorf("foreign rid: %s", out)
	}
	if ctxB.Err() != nil {
		t.Fatal("foreign run cancelled")
	}

	// By rid, into a chosen path, with the reason: who = tenant/stack.
	out := callRunAbort(t, live, "acme", "loop", `{"rid":"rid-a","reason":"stopped by the owner","into":"_stop"}`)
	if gjson.Get(out, "_stop.aborted").Int() != 1 || gjson.Get(out, "_stop.rid").String() != "rid-a" {
		t.Fatalf("abort = %s", out)
	}
	ab, ok := processor.AbortCause(ctxA)
	if !ok || ab.By != "acme/loop" || ab.Reason != "stopped by the owner" {
		t.Fatalf("cause = %+v, %v", ab, ok)
	}

	// By stack: the tenant's worker run, not the other tenant's.
	out = callRunAbort(t, live, "acme", "loop", `{"stack":"worker"}`)
	if gjson.Get(out, "_abort.aborted").Int() != 1 || gjson.Get(out, "_abort.stack").String() != "worker" {
		t.Fatalf("abort stack = %s", out)
	}
	if ctxC.Err() == nil || ctxB.Err() != nil {
		t.Error("stack abort ended the wrong runs")
	}
}
