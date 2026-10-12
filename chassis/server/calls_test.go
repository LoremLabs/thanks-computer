package server

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

func callCallsList(t *testing.T, d callsDeps, tenant, metaJSON string) string {
	t.Helper()
	ctx := context.Background()
	if tenant != "" {
		ctx = processor.WithTenant(ctx, tenant)
	}
	ctx = operation.WithMeta(ctx, metaJSON)
	pl, err := callsList(ctx, d, []byte(`{"_txc":{"tenant":"gone"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

func TestCallsListOp(t *testing.T) {
	d := callsDeps{decls: callDeclFixture(t)}

	out := callCallsList(t, d, "acme", `{}`)
	calls := gjson.Get(out, "_calls")
	if calls.Get("count").Int() != 3 || len(calls.Get("items").Array()) != 3 {
		t.Fatalf("list = %s", out)
	}
	orient := calls.Get(`items.#(name=="orient")`)
	if orient.Get("stack").String() != "app/_orient" || orient.Get("entry").Int() != 100 || orient.Get("stage").String() != "app/_orient/100" ||
		!orient.Get("input.job.required").Bool() || !orient.Get("output.prompt.required").Bool() ||
		orient.Get("params").Raw != `["job"]` || orient.Get("returns").Raw != `["prompt"]` || orient.Get("timeout").Int() != 20000 {
		t.Errorf("orient = %s", orient.Raw)
	}
	// An entry with no input or output still answers objects and lists.
	onboard := calls.Get(`items.#(name=="onboard")`)
	if onboard.Get("input").Raw != `{}` || onboard.Get("output").Raw != `{}` || onboard.Get("params").Raw != `[]` || onboard.Get("returns").Raw != `[]` {
		t.Errorf("onboard = %s", onboard.Raw)
	}
	if b := calls.Get("broken"); len(b.Array()) != 1 || b.Get("0.name").String() != "broken" || b.Get("0.stack").String() != "app/_orient" {
		t.Errorf("broken = %s", b.Raw)
	}

	// `into` is honoured; `stack` narrows.
	out = callCallsList(t, d, "acme", `{"into":"_mine","stack":"app"}`)
	if gjson.Get(out, "_calls").Exists() || gjson.Get(out, "_mine.count").Int() != 1 || gjson.Get(out, "_mine.items.0.name").String() != "onboard" {
		t.Errorf("into + stack = %s", out)
	}
	// The tenant is the pinned scope's, never the envelope's; none is an error.
	if out := callCallsList(t, d, "", `{}`); gjson.Get(out, "_calls.error.code").String() != "txco_calls_no_tenant" {
		t.Errorf("no tenant = %s", out)
	}
	if out := callCallsList(t, callsDeps{}, "acme", `{}`); gjson.Get(out, "_calls.error.code").String() != "txco_calls_store" {
		t.Errorf("no store = %s", out)
	}
}
