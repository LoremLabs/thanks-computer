package processor

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/tenants"
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
		{"_txc.dns.res.rcode", true},
		{"_txc.dns.res.answer", true},
		{"_txc.imap.res.ok", true},
		{"_txc.imap.res.flags", true},
		{"_txc.calendar.res.ok", true},
		{"_txc.calendar.res.event.start", true},
		{"_txc.calendar.event.summary", false}, // a client's object facts are read-only…
		{"_txc.calendar.tenant", false},        // …and the route hint especially
		{"_txc.imap.msg.text", false},          // an appended message's facts are read-only…
		{"_txc.imap.tenant", false},            // …and the route hint especially
		{"_txc.dns.proposed.answer", false},    // the head's proposal is read-only
		{"_txc.dns.q.name", false},             // inbound facts are reserved
		{"_txc.dns.tenant", false},             // the route hint especially
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
		// WebSocket sessions: every fact is chassis-stamped and there is no
		// verdict subtree at all (a stack talks back with txco://websocket/*).
		{"_txc.websocket.upgrade", false},
		{"_txc.websocket.session.id", false},
		{"_txc.websocket.session.state.email", false},
		{"_txc.websocket.msg.text", false},
		{"_txc.websocket.res.text", false},
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
		_, transport, _ := pu.Exec(context.Background(), op)
		if got := transportAuthorControlled(transport); got != c.wantAuthorControlled {
			t.Errorf("Exec(%q) transport = %q → authorControlled = %v, want %v",
				c.exec, transport, got, c.wantAuthorControlled)
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

// TestEmitRouteSystemAuthoredOnly — the lockdown-regression fix for the
// _sys/boot operator-hook pattern: a SYSTEM-authored rule (a run pinned to
// the `_sys` tenant) may EMIT `_txc.route.*` proposals, while author
// provenance still drops them, reserved siblings still drop even for
// system, and lookalike paths stay reserved.
func TestEmitRouteSystemAuthoredOnly(t *testing.T) {
	pu, _ := newTestUnit(t)
	sysCtx := WithTenant(context.Background(), tenants.SystemTenantSlug)

	overrides := []resonator.BranchValue{
		{Path: "._txc.route.tenant", Value: ast.Literal{V: "default"}},
		{Path: "._txc.route.stack", Value: ast.Literal{V: "mcp-server"}},
		{Path: "._txc.route.to", Value: ast.Literal{V: "mcp-server/0"}},
		{Path: "._txc.tenant", Value: ast.Literal{V: "intruder"}}, // reserved sibling
		{Path: "._txc.routes", Value: ast.Literal{V: "lookalike"}},
	}

	got, err := pu.OverlayResponseFor(sysCtx, `{}`, `{}`, overrides)
	if err != nil {
		t.Fatalf("OverlayResponseFor: %v", err)
	}
	if g := gjson.Get(got, "_txc.route.to").String(); g != "mcp-server/0" {
		t.Errorf("system EMIT _txc.route.to not written: %s", got)
	}
	if g := gjson.Get(got, "_txc.route.tenant").String(); g != "default" {
		t.Errorf("system EMIT _txc.route.tenant not written: %s", got)
	}
	if gjson.Get(got, "_txc.tenant").Exists() {
		t.Errorf("reserved _txc.tenant forged alongside system route EMIT: %s", got)
	}
	if gjson.Get(got, "_txc.routes").Exists() {
		t.Errorf("lookalike _txc.routes written for system: %s", got)
	}

	// Author provenance — the exported default AND a concrete-tenant pin —
	// still drops route.* (fail-closed).
	for name, run := range map[string]func() (string, error){
		"exported default": func() (string, error) {
			return pu.OverlayResponse(`{}`, `{}`, overrides)
		},
		"concrete tenant pin": func() (string, error) {
			return pu.OverlayResponseFor(WithTenant(context.Background(), "acme"), `{}`, `{}`, overrides)
		},
		"unpinned ctx": func() (string, error) {
			return pu.OverlayResponseFor(context.Background(), `{}`, `{}`, overrides)
		},
	} {
		out, oerr := run()
		if oerr != nil {
			t.Fatalf("%s: %v", name, oerr)
		}
		if gjson.Get(out, "_txc.route").Exists() {
			t.Errorf("%s: route proposal survived author provenance: %s", name, out)
		}
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

// TestAuthorMayDeleteTxc — the `_txc.delete` guard admits the delete-only
// inbound facts (a consumed @web.req.body) on top of the write allowlist,
// and nothing else. The same paths stay NON-writable (TestAuthorMayWriteTxc
// pins `_txc.web.req.body` false) — deleting a stamped fact can't forge one.
func TestAuthorMayDeleteTxc(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"_txc.web.req.body", true}, // delete-only
		{"_txc.web.res.body", true}, // writable ⇒ deletable
		{"data.blob", true},         // author's own data
		{"_txc.web.req", false},     // the parent subtree stays reserved
		{"_txc.web.req.headers", false},
		{"_txc.web.req.bodyx", false}, // lookalike sibling
		{"_txc.tenant", false},
		{"_txc.fuel_used", false},
		{"_txc", false},
	}
	for _, c := range cases {
		if got := authorMayDeleteTxc(c.path); got != c.want {
			t.Errorf("authorMayDeleteTxc(%q) = %v, want %v", c.path, got, c.want)
		}
	}
	if authorMayWriteTxc("_txc.web.req.body") {
		t.Fatal("_txc.web.req.body must stay non-writable (delete-only)")
	}
	// The appended message's bodies/headers are delete-only too: a stack
	// omits them from the trace after it has consumed them.
	for _, p := range []string{"_txc.imap.msg.text", "_txc.imap.msg.html", "_txc.imap.msg.headers", "_txc.imap.msg.headers.subject"} {
		if !authorMayDeleteTxc(p) || authorMayWriteTxc(p) {
			t.Errorf("%s: delete=%v write=%v, want delete-only", p, authorMayDeleteTxc(p), authorMayWriteTxc(p))
		}
	}
	// A client's calendar object and its parse are delete-only the same
	// way; the route hint beside them is not even deletable.
	for _, p := range []string{"_txc.calendar.ical", "_txc.calendar.event", "_txc.calendar.event.recur", "_txc.calendar.prior", "_txc.calendar.prior.event"} {
		if !authorMayDeleteTxc(p) || authorMayWriteTxc(p) {
			t.Errorf("%s: delete=%v write=%v, want delete-only", p, authorMayDeleteTxc(p), authorMayWriteTxc(p))
		}
	}
	if authorMayDeleteTxc("_txc.calendar.tenant") || authorMayDeleteTxc("_txc.calendar.object") {
		t.Error("calendar route hint / object identity must not be deletable")
	}
	// An inbound WebSocket payload is delete-only the same way; the session
	// facts beside it are not even deletable.
	for _, p := range []string{"_txc.websocket.msg.text", "_txc.websocket.msg.data"} {
		if !authorMayDeleteTxc(p) || authorMayWriteTxc(p) {
			t.Errorf("%s: delete=%v write=%v, want delete-only", p, authorMayDeleteTxc(p), authorMayWriteTxc(p))
		}
	}
	for _, p := range []string{"_txc.websocket.msg", "_txc.websocket.msg.type", "_txc.websocket.session.id", "_txc.websocket.msg.textual"} {
		if authorMayDeleteTxc(p) {
			t.Errorf("%s must not be deletable", p)
		}
	}
	if authorMayDeleteTxc("_txc.imap.msg.sha256") || authorMayDeleteTxc("_txc.imap.tenant") {
		t.Fatal("imap identity facts must not be deletable")
	}
}
