package server

import (
	"context"
	"errors"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/processor"
)

// callCaps runs txco://caps/list as a rule of `stack` in `tenant`.
func callCaps(t *testing.T, d capsDeps, tenant, stack, metaJSON string) string {
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
	pl, err := capsList(ctx, d, []byte(`{"_txc":{"tenant":"gone","stack":"evil"}}`))
	if err != nil {
		t.Fatalf("handler returned a Go error: %v", err)
	}
	return pl.Raw
}

type failingCaps struct{}

func (failingCaps) List(context.Context, string) ([]capEntry, error) {
	return nil, errors.New("list capabilities: snapshot closed")
}

func TestCapsListOp(t *testing.T) {
	d := capsDeps{decls: capDeclFixture(t)}

	out := callCaps(t, d, "acme", "loop", `{}`)
	caps := gjson.Get(out, "_caps")
	if caps.Get("count").Int() != 3 || len(caps.Get("items").Array()) != 3 {
		t.Fatalf("list = %s", out)
	}
	if names := caps.Get("items.#.name").Raw; names != `["local.web.fetch","mail.send","run.finish"]` {
		t.Errorf("names = %s", names)
	}
	mail := caps.Get(`items.#(name=="mail.send")`)
	if mail.Get("stack").String() != "loop" || mail.Get("entry").Int() != 7000 || mail.Get("stage").String() != "loop/7000" ||
		mail.Get("description").String() != "Send a message." || !mail.Get("input.to.required").Bool() ||
		mail.Get("params").Raw != `["to"]` || mail.Get("timeout").Int() != 60000 {
		t.Errorf("mail.send = %s", mail.Raw)
	}
	// A capability with no input still answers an object and a list, so a
	// compute reads them without a guard.
	fetch := caps.Get(`items.#(name=="local.web.fetch")`)
	if fetch.Get("stack").String() != "pony-web" || fetch.Get("input").Raw != `{}` || fetch.Get("params").Raw != `[]` {
		t.Errorf("local.web.fetch = %s", fetch.Raw)
	}
	// The row that does not parse is named, not listed as callable.
	if b := caps.Get("broken"); len(b.Array()) != 1 || b.Get("0.name").String() != "broken" || b.Get("0.stack").String() != "loop" || b.Get("0.error").String() == "" {
		t.Errorf("broken = %s", b.Raw)
	}

	// `into` is honoured; `prefix` narrows by name.
	out = callCaps(t, d, "acme", "pony-agent", `{"into":"_local","prefix":"local."}`)
	if gjson.Get(out, "_caps").Exists() || gjson.Get(out, "_local.count").Int() != 1 ||
		gjson.Get(out, "_local.items.0.name").String() != "local.web.fetch" || gjson.Get(out, "_local.broken").Exists() {
		t.Errorf("into + prefix = %s", out)
	}

	// A tenant with none answers an empty list, not an error.
	out = callCaps(t, d, "other", "x", `{}`)
	if gjson.Get(out, "_caps.count").Int() != 0 || gjson.Get(out, "_caps.items").Raw != `[]` || gjson.Get(out, "_caps.error").Exists() {
		t.Errorf("empty tenant = %s", out)
	}
	// The tenant is the pinned scope's, never the envelope's.
	out = callCaps(t, d, "gone", "x", `{}`)
	if gjson.Get(out, "_caps.count").Int() != 0 {
		t.Errorf("revoked tenant = %s", out)
	}

	// No tenant in scope, a store that fails, no source at all.
	wantIdentityCode(t, "no tenant", callCaps(t, d, "", "x", `{}`), "_caps", "txco_caps_no_tenant")
	wantIdentityCode(t, "store", callCaps(t, capsDeps{decls: failingCaps{}}, "acme", "x", `{"into":"_c"}`), "_c", "txco_caps_store")
	wantIdentityCode(t, "no source", callCaps(t, capsDeps{}, "acme", "x", `{}`), "_caps", "txco_caps_store")
}
