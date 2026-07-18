package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/txcl/ast"
)

func TestAuthorMayWriteTxc(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Non-_txc keys are always the author's own data.
		{"data", true},
		{"summary.text", true},
		{"_op.body_text", true}, // any non-_txc namespace is fine
		// Allowlisted _txc subtrees.
		{"_txc.web.res.body", true},
		{"_txc.web.res.headers.content-type", true},
		{"_txc.lmtp.res.code", true},
		{"_txc.goto", true},
		{"_txc.halt", true},
		{"_txc.delete", true},
		{"_txc.telemetry", true},
		{"_txc.telemetry.metrics", true},
		{"_txc.llm.reject", true},
		{"_txc.llm.reject.status", true},
		{"_txc.llm.upstream.url", true},
		{"_txc.llm.headers.x-policy", true},
		{"_txc.llm.context", true},
		{"_txc.llm.context.0.content", true},
		// Reserved control fields — never author-writable.
		{"_txc", false},
		{"_txc.tenant", false},
		{"_txc.src", false},
		{"_txc.rid", false},
		{"_txc.route.tenant", false},
		{"_txc.cron.node", false},
		{"_txc.computed.sig_valid", false},
		{"_txc.chat.tokens.in", false},
		{"_txc.fuel_used", false},
		{"_txc._seen", false},
		{"_txc.ttl", false}, // EMIT-only (lower-only); not output-writable
		{"_txc.web.req.body", false},
		// AI gateway: chassis-stamped identity/phase fields stay reserved —
		// only the verdict subtrees (reject/upstream/headers) are writable.
		{"_txc.llm.phase", false},
		{"_txc.llm.tenant", false},
		{"_txc.llm.request_id", false},
		{"_txc.llm.completion.status", false},
		{"_txc.llm.completion.usage.input_tokens", false},
		{"_txc.llm.context_result", false}, // gateway ground truth; a stack must not forge it
		// A reserved prefix must not be defeated by a lookalike sibling.
		{"_txc.web.response", false}, // not "web.res"
		{"_txc.gotoxyz", false},      // not "goto"
		{"_txc.telemetryX", false},   // not "telemetry"
		{"_txc.llm.rejected", false}, // not "llm.reject"
	}
	for _, c := range cases {
		if got := authorMayWriteTxc(c.path); got != c.want {
			t.Errorf("authorMayWriteTxc(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestTransportAuthorControlled(t *testing.T) {
	cases := []struct {
		transport string
		want      bool // true == author-controlled (untrusted), must sanitize
	}{
		{"txco", false}, // built-in core handler (Mux registry)
		{"ai", false},   // chassis-owned namespace
		{"goto", false}, // chassis-synthesized stage jump
		{"noop", false},
		{"mock", true}, // txco://mock / pattern-mock — author literal
		{"http", true},
		{"https", true},
		{"compute", true},
		{"mcp+http", true},
		{"unsupported", true},
		{"", true}, // goto:// TODO leaves transport empty — fail closed
	}
	for _, c := range cases {
		if got := transportAuthorControlled(c.transport); got != c.want {
			t.Errorf("transportAuthorControlled(%q) = %v, want %v", c.transport, got, c.want)
		}
	}
}

// TestExecSurfacesTrustByTransport proves the trust bit Exec returns is keyed
// off the real dispatch transport (Mux registry for txco://), not the scheme
// string: built-in core handlers (copy/noop) are trusted; a rule-author mock
// and an unsupported scheme are not — even though txco://mock shares the
// txco:// scheme. Handler errors are irrelevant (trust is computed from the
// transport that ran), so no real I/O is needed.
func TestExecSurfacesTrustByTransport(t *testing.T) {
	pu, _ := newTestUnit(t)
	cases := []struct {
		exec                 string
		wantAuthorControlled bool
	}{
		{"txco://noop", false},  // chassis-synthesized
		{"txco://copy", false},  // built-in core handler
		{"txco://mock", true},   // author mock despite txco:// scheme
		{"gopher://nope", true}, // unsupported scheme → fail closed
	}
	for _, c := range cases {
		op := operation.Operation{
			Stack: "t", Scope: 0, Name: "n",
			Resonator: &resonator.Resonator{Exec: c.exec},
		}
		_, authorControlled, _ := pu.Exec(context.Background(), op)
		if authorControlled != c.wantAuthorControlled {
			t.Errorf("Exec(%q) authorControlled = %v, want %v",
				c.exec, authorControlled, c.wantAuthorControlled)
		}
	}
}

// TestSanitizeNestedPartialAllow is the canonical projection case: a forged
// reserved sibling is dropped while an allowed sibling in the SAME _txc object
// is preserved, with no empty reserved parent left behind.
func TestSanitizeNestedPartialAllow(t *testing.T) {
	in := `{"_txc":{"tenant":"victim","web":{"res":{"status":201}},"computed":{"sig_valid":true}}}`
	out := sanitizeAuthorOutput(in)

	if gjson.Get(out, "_txc.tenant").Exists() {
		t.Errorf("forged _txc.tenant survived: %s", out)
	}
	if gjson.Get(out, "_txc.computed").Exists() {
		t.Errorf("forged _txc.computed survived: %s", out)
	}
	if got := gjson.Get(out, "_txc.web.res.status").Int(); got != 201 {
		t.Errorf("allowed _txc.web.res.status dropped: got %d (raw=%s)", got, out)
	}
}

// TestSanitizeDropsReservedKeepsData covers a remote/compute/mock producer
// trying to forge tenant + computed-auth + budget alongside legitimate data
// and response/flow fields.
func TestSanitizeDropsReservedKeepsData(t *testing.T) {
	in := `{"data":{"n":1},"_txc":{"tenant":"victim","computed":{"sig_valid":true},` +
		`"fuel_used":0,"_seen":{},"web":{"res":{"body":"hi"}},"goto":"x/100","delete":["data.scratch"]}}`
	out := sanitizeAuthorOutput(in)

	for _, reserved := range []string{"_txc.tenant", "_txc.computed", "_txc.fuel_used", "_txc._seen"} {
		if gjson.Get(out, reserved).Exists() {
			t.Errorf("reserved %s survived sanitize: %s", reserved, out)
		}
	}
	if got := gjson.Get(out, "data.n").Int(); got != 1 {
		t.Errorf("non-_txc data dropped: %s", out)
	}
	if got := gjson.Get(out, "_txc.web.res.body").String(); got != "hi" {
		t.Errorf("allowed _txc.web.res.body dropped: %s", out)
	}
	if got := gjson.Get(out, "_txc.goto").String(); got != "x/100" {
		t.Errorf("allowed _txc.goto dropped: %s", out)
	}
	if got := gjson.Get(out, "_txc.delete.0").String(); got != "data.scratch" {
		t.Errorf("allowed _txc.delete dropped: %s", out)
	}
}

// TestSanitizeNullMergeCannotClearReserved: a reserved key set to null is not
// in the allowlist, so projection never carries it to the merge — it cannot
// null an existing control value.
func TestSanitizeNullMergeCannotClearReserved(t *testing.T) {
	out := sanitizeAuthorOutput(`{"_txc":{"tenant":null}}`)
	if gjson.Get(out, "_txc.tenant").Exists() {
		t.Errorf("null _txc.tenant survived projection: %s", out)
	}
	// Nothing allowed remained → _txc is omitted entirely.
	if gjson.Get(out, "_txc").Exists() {
		t.Errorf("empty _txc should be omitted: %s", out)
	}
}

func TestSanitizeNoTxcUnchanged(t *testing.T) {
	in := `{"data":1,"summary":{"text":"ok"}}`
	if out := sanitizeAuthorOutput(in); out != in {
		t.Errorf("output without _txc was altered: %s", out)
	}
}

// TestEmitCannotSetReservedTxc pins the EMIT overlay guard: a reserved target
// (post-@-expansion) is dropped, an allowlisted response field is written, and
// _txc.ttl is honored only downward.
func TestEmitCannotSetReservedTxc(t *testing.T) {
	pu, _ := newTestUnit(t)
	output := `{"text":"victim"}`

	// EMIT @tenant = .text  (Path is the post-@-expansion form).
	overrides := []resonator.BranchValue{
		{Path: "._txc.tenant", Value: ast.PathRef{Path: "text"}},
		{Path: "._txc.fuel_used", Value: ast.PathRef{Path: "text"}},
		{Path: "._txc.web.res.body", Value: ast.PathRef{Path: "text"}},
	}
	got, err := pu.OverlayResponse(`{}`, output, overrides)
	if err != nil {
		t.Fatalf("OverlayResponse: %v", err)
	}
	if gjson.Get(got, "_txc.tenant").Exists() {
		t.Errorf("EMIT forged _txc.tenant: %s", got)
	}
	if gjson.Get(got, "_txc.fuel_used").Exists() {
		t.Errorf("EMIT forged _txc.fuel_used: %s", got)
	}
	if g := gjson.Get(got, "_txc.web.res.body").String(); g != "victim" {
		t.Errorf("allowlisted _txc.web.res.body not written: %s", got)
	}
}

// TestEmitTelemetryMetricsWritable is the allowlist-growth proof for
// "telemetry": a stack emits metric intents at _txc.telemetry.metrics
// (EMIT overlay), while reserved siblings stay blocked in the same call.
func TestEmitTelemetryMetricsWritable(t *testing.T) {
	pu, _ := newTestUnit(t)

	overrides := []resonator.BranchValue{
		{Path: "._txc.telemetry.metrics", Value: ast.Literal{V: []interface{}{
			map[string]interface{}{"name": "book.queued", "kind": "counter", "value": int64(1)},
		}}},
		{Path: "._txc.tenant", Value: ast.Literal{V: "victim"}},
	}
	got, err := pu.OverlayResponse(`{}`, `{}`, overrides)
	if err != nil {
		t.Fatalf("OverlayResponse: %v", err)
	}
	if g := gjson.Get(got, "_txc.telemetry.metrics.0.name").String(); g != "book.queued" {
		t.Errorf("EMIT _txc.telemetry.metrics not written: %s", got)
	}
	if gjson.Get(got, "_txc.tenant").Exists() {
		t.Errorf("EMIT forged _txc.tenant alongside telemetry: %s", got)
	}
}

// TestSanitizeKeepsTelemetry: an untrusted producer (http/compute/mock
// transport) contributing metric intents keeps them through the output
// sanitizer's projection, while forged control fields are dropped.
func TestSanitizeKeepsTelemetry(t *testing.T) {
	in := `{"data":1,"_txc":{"tenant":"victim",` +
		`"telemetry":{"metrics":[{"name":"book.queued","kind":"counter","value":1}]}}}`
	out := sanitizeAuthorOutput(in)

	if gjson.Get(out, "_txc.tenant").Exists() {
		t.Errorf("forged _txc.tenant survived: %s", out)
	}
	if g := gjson.Get(out, "_txc.telemetry.metrics.0.name").String(); g != "book.queued" {
		t.Errorf("_txc.telemetry dropped by sanitizer: %s", out)
	}
}

// TestTelemetryMetricsAccumulateAcrossMerges pins the accumulator
// semantic the telemetry feature depends on: two ops each contributing
// one element to _txc.telemetry.metrics merge to an array of two
// (MergeJSON appends arrays), in emission order.
func TestTelemetryMetricsAccumulateAcrossMerges(t *testing.T) {
	pu, _ := newTestUnit(t)

	resp := `{"_txc":{"telemetry":{"metrics":[{"name":"a","kind":"counter","value":1}]}}}`
	out := `{"_txc":{"telemetry":{"metrics":[{"name":"b","kind":"counter","value":2}]}}}`
	merged, err := pu.MergeJSON(resp, out)
	if err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	arr := gjson.Get(merged, "_txc.telemetry.metrics").Array()
	if len(arr) != 2 {
		t.Fatalf("metrics array length = %d, want 2 (raw=%s)", len(arr), merged)
	}
	if arr[0].Get("name").String() != "a" || arr[1].Get("name").String() != "b" {
		t.Errorf("accumulator order wrong: %s", merged)
	}
}

// TestEmitTTLLowerOnly: EMIT may lower _txc.ttl but never raise it.
func TestEmitTTLLowerOnly(t *testing.T) {
	pu, _ := newTestUnit(t)
	env := `{"_txc":{"ttl":5}}`

	lower := []resonator.BranchValue{{Path: "._txc.ttl", Value: ast.Literal{V: int64(2)}}}
	got, err := pu.OverlayResponse(env, `{}`, lower)
	if err != nil {
		t.Fatalf("OverlayResponse lower: %v", err)
	}
	if g := gjson.Get(got, "_txc.ttl").Int(); g != 2 {
		t.Errorf("EMIT @ttl=2 with env ttl=5 → got %d, want 2 (raw=%s)", g, got)
	}

	raise := []resonator.BranchValue{{Path: "._txc.ttl", Value: ast.Literal{V: int64(100)}}}
	got2, err := pu.OverlayResponse(env, `{}`, raise)
	if err != nil {
		t.Fatalf("OverlayResponse raise: %v", err)
	}
	if g := gjson.Get(got2, "_txc.ttl").Int(); g > 5 {
		t.Errorf("EMIT @ttl=100 must be clamped to <=5; got %d (raw=%s)", g, got2)
	}
}
