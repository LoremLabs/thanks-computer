package processor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/calldecl"
	"github.com/loremlabs/thanks-computer/chassis/capdecl"
)

// `WITH entry` names a declared entry of the callee (CALLS/<name>.yaml,
// chassis/calldecl): docs/todo-stack-call.md §4 in the service repo.

type callLookupFunc func(ctx context.Context, tenant, stack, name string) (*capdecl.Decl, error)

func (f callLookupFunc) Lookup(ctx context.Context, tenant, stack, name string) (*capdecl.Decl, error) {
	return f(ctx, tenant, stack, name)
}

// entryUnit is a tenant-pinned unit whose stack `svc` declares the entry
// `orient` (at 100, input job required, output prompt required, 150 ms).
func entryUnit(t *testing.T) (*Unit, context.Context) {
	t.Helper()
	pu, _ := newTestUnit(t)
	decls := map[string]*capdecl.Decl{
		"svc/orient": {Entry: 100, Timeout: 150,
			Input:  map[string]capdecl.Field{"job": {Required: true}, "text": {}},
			Output: map[string]capdecl.Field{"prompt": {Required: true}, "sources": {}}},
		"svc/plain": {Entry: 200},
	}
	pu.CallDecls = callLookupFunc(func(_ context.Context, tenant, stack, name string) (*capdecl.Decl, error) {
		if tenant != "acme" {
			return nil, calldecl.ErrNotDeclared
		}
		if d, ok := decls[stack+"/"+name]; ok {
			return d, nil
		}
		return nil, calldecl.ErrNotDeclared
	})
	if _, err := pu.Dbc.Db.Exec(`INSERT INTO tenants (tenant_id, slug, name, created_at) VALUES ('tnt_acme', 'acme', 'Acme', '')`); err != nil {
		t.Fatal(err)
	}
	return pu, WithTenant(context.Background(), "acme")
}

func runEntry(t *testing.T, pu *Unit, ctx context.Context, in, stage string) string {
	t.Helper()
	got, err := callRun(t, pu, ctx, in, stage)
	if err != nil || len(got) != 1 {
		t.Fatalf("Run(%s): %v %+v", stage, err, got)
	}
	return got[0].Raw
}

const entryIn = `{"_txc":{"stack":"c","src":"http","tenant":"acme"}}`

// A declared entry says where the call enters and what it promises: the
// callee starts at the entry's scope, sees `@call.entry`, and its required
// output comes back.
func TestCallEntryEntersAtTheDeclaredScope(t *testing.T) {
	pu, ctx := entryUnit(t)
	seedAcmeOp(t, pu, "svc", 50, "before", `EMIT ._call.output.leaked = true`)
	seedAcmeOp(t, pu, "svc", 100, "compile", `EMIT ._call.output = &object("prompt", @call.input.job, "entry", @call.entry)`)
	seedAcmeOp(t, pu, "c", 100, "call", `WITH input = &object("job", "j1"), entry = "orient" EXEC "call://svc"`)
	final := runEntry(t, pu, ctx, entryIn, "c/0")
	res := gjson.Get(final, "_call")
	if !res.Get("ok").Bool() || res.Get("output.prompt").String() != "j1" || res.Get("output.entry").String() != "orient" {
		t.Errorf("result = %s", res.Raw)
	}
	if res.Get("output.leaked").Exists() {
		t.Errorf("the callee ran from its start, not the entry: %s", res.Raw)
	}
}

// A missing required input fails before the callee runs; a missing
// required output fails after it ends, with what the callee did write.
func TestCallEntryChecksInputAndOutput(t *testing.T) {
	pu, ctx := entryUnit(t)
	ran := captureHandler(t, pu, "txco://ran")
	seedAcmeOp(t, pu, "svc", 100, "compile", `EXEC "txco://ran"`)
	seedAcmeOp(t, pu, "svc", 150, "answer", `WHEN @call.input.partial == true EMIT ._call.output.sources = ["a"]`)
	seedAcmeOp(t, pu, "c", 100, "no-job", `WHEN .case == "input" WITH input = &object("text", "hi"), entry = "orient" EXEC "call://svc"`)
	seedAcmeOp(t, pu, "c", 100, "no-prompt", `WHEN .case == "output" WITH input = &object("job", "j1", "partial", true), entry = "orient" EXEC "call://svc"`)

	final := runEntry(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"},"case":"input"}`, "c/0")
	res := gjson.Get(final, "_call")
	if res.Get("ok").Bool() || res.Get("error.code").String() != "txco_call_invalid_input" ||
		!strings.Contains(res.Get("error.message").String(), "job") {
		t.Errorf("missing input: %s", res.Raw)
	}
	if v, _ := ran.Load().(string); v != "" {
		t.Errorf("the callee ran despite the missing input: %s", v)
	}

	final = runEntry(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"},"case":"output"}`, "c/0")
	res = gjson.Get(final, "_call")
	if res.Get("ok").Bool() || res.Get("error.code").String() != "txco_call_invalid_output" ||
		!strings.Contains(res.Get("error.message").String(), "prompt") {
		t.Errorf("missing output: %s", res.Raw)
	}
	if res.Get("output.sources.0").String() != "a" {
		t.Errorf("what the callee did write should still come back: %s", res.Raw)
	}
	if v, _ := ran.Load().(string); v == "" {
		t.Errorf("the callee should have run")
	}
}

// What is not an entry: an undeclared name, a chassis with no declarations,
// a bad name, and an entry named beside a scope.
func TestCallEntryRefusals(t *testing.T) {
	pu, ctx := entryUnit(t)
	seedAcmeOp(t, pu, "svc", 100, "s", `EMIT ._call.output.prompt = "p"`)
	for name, rule := range map[string]string{
		"undeclared": `WITH entry = "nope" EXEC "call://svc"`,
		"bad-name":   `WITH entry = "Not Valid" EXEC "call://svc"`,
		"both":       `WITH entry = "orient" EXEC "call://svc/100"`,
	} {
		seedAcmeOp(t, pu, "c-"+name, 100, "call", rule)
	}
	for name, code := range map[string]string{"undeclared": "txco_call_no_entry", "bad-name": "txco_call_target", "both": "txco_call_target"} {
		final := runEntry(t, pu, ctx, `{"_txc":{"stack":"c-`+name+`","src":"http","tenant":"acme"}}`, "c-"+name+"/0")
		if got := gjson.Get(final, "_call.error.code").String(); got != code {
			t.Errorf("%s: code = %q, want %s: %s", name, got, code, final)
		}
	}
	// No declarations on this chassis at all: no entry, not a crash.
	pu.CallDecls = nil
	seedAcmeOp(t, pu, "c-none", 100, "call", `WITH entry = "orient" EXEC "call://svc"`)
	final := runEntry(t, pu, ctx, `{"_txc":{"stack":"c-none","src":"http","tenant":"acme"}}`, "c-none/0")
	if got := gjson.Get(final, "_call.error.code").String(); got != "txco_call_no_entry" {
		t.Errorf("no lookup: %s", final)
	}
}

// The declared timeout bounds the call when shorter than the op's.
func TestCallEntryTimeoutShortensTheCall(t *testing.T) {
	pu, ctx := entryUnit(t)
	pu.Conf.CallStopGrace = "1s"
	callBlockingOp(t, pu, "block", 0)
	seedAcmeOp(t, pu, "svc", 100, "wait", `EXEC "txco://block" WITH timeout = 10000`)
	seedAcmeOp(t, pu, "c", 100, "call", `WITH input = &object("job", "j"), entry = "orient", timeout = 10000 EXEC "call://svc"`)
	start := time.Now()
	final := runEntry(t, pu, ctx, entryIn, "c/0")
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %s; the declared timeout was 150 ms", took)
	}
	if got := gjson.Get(final, "_call.error.code").String(); got != "txco_call_timeout" {
		t.Errorf("code = %q: %s", got, final)
	}
}

// A capability declared with outputs is held to them when answered here.
func TestExecCapDeclaredHereChecksOutput(t *testing.T) {
	pu, ctx := capUnit(t)
	pu.Caps = declares(map[string]struct {
		stack string
		decl  *capdecl.Decl
	}{
		"mail.send": {"svc", &capdecl.Decl{Entry: 7000, Output: map[string]capdecl.Field{"mid": {Required: true}}}},
	})
	seedAcmeOp(t, pu, "svc", 7000, "send", `WHEN @cap.input.ok == true EMIT ._cap.output.mid = "m1"`)
	seedAcmeOp(t, pu, "svc", 7000, "half", `WHEN @cap.input.ok != true EMIT ._cap.output.other = 1`)
	seedAcmeOp(t, pu, "c", 100, "good", `WHEN .ok == true WITH input = &object("ok", true) EXEC "cap://mail.send"`)
	seedAcmeOp(t, pu, "c", 100, "bad", `WHEN .ok != true WITH input = &object("ok", false) EXEC "cap://mail.send"`)
	got, err := callRun(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"},"ok":true}`, "c/0")
	if err != nil || len(got) != 1 || gjson.Get(got[0].Raw, "_cap.mid").String() != "m1" || gjson.Get(got[0].Raw, "cap.error").Exists() {
		t.Fatalf("declared output present: %v %+v", err, got)
	}
	got, err = callRun(t, pu, ctx, `{"_txc":{"stack":"c","src":"http","tenant":"acme"},"ok":false}`, "c/0")
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if gjson.Get(got[0].Raw, "cap.error.code").String() != "txco_cap_invalid_output" || gjson.Get(got[0].Raw, "cap.error.status").Int() != 502 ||
		!strings.Contains(gjson.Get(got[0].Raw, "cap.error.message").String(), "mid") {
		t.Errorf("declared output missing: %s", got[0].Raw)
	}
}
