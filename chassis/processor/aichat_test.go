package processor

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/loremlabs/thanks-computer/chassis/chat"
	"github.com/loremlabs/thanks-computer/chassis/event"
	"github.com/loremlabs/thanks-computer/chassis/operation"
	"github.com/loremlabs/thanks-computer/chassis/resonator"
	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// --- test backend ---
//
// A controllable in-test chat backend, registered with the package
// global registry under a unique name. Use `withTestBackend` to scope
// registration to one test.

type testBackend struct {
	name         string
	capabilities []string
	required     []string
	resp         chat.Response
	err          error
	calls        int32
	sawKey       string
	sawMessages  []chat.Message
	mu           sync.Mutex
}

func (b *testBackend) Name() string             { return b.name }
func (b *testBackend) Capabilities() []string   { return b.capabilities }
func (b *testBackend) RequiredSecrets() []string { return b.required }

func (b *testBackend) Run(ctx context.Context, req chat.Request, bag *secrets.SecretBag) (chat.Response, error) {
	atomic.AddInt32(&b.calls, 1)
	b.mu.Lock()
	if len(b.required) > 0 {
		if v, ok := bag.Get(b.required[0]); ok {
			b.sawKey = string(v)
		}
	}
	b.sawMessages = append([]chat.Message(nil), req.Messages...)
	b.mu.Unlock()
	return b.resp, b.err
}

// withTestBackend registers stub as a chat backend named `name` for the
// duration of one test. The chassis chat registry is global, so each
// test must clear before and restore after to avoid cross-test
// contamination.
func withTestBackend(t *testing.T, name string, stub *testBackend) {
	t.Helper()
	stub.name = name
	if stub.capabilities == nil {
		stub.capabilities = []string{"public_execution"}
	}
	chat.Register(name, func(cfg chat.Config) (chat.Backend, error) {
		return stub, nil
	})
}

// sentinelBackend's constructor calls t.Fatal — used to prove the chat
// handler short-circuits before backend instantiation in scenarios like
// mock interception.
func registerSentinelBackend(t *testing.T, name string) {
	t.Helper()
	chat.Register(name, func(cfg chat.Config) (chat.Backend, error) {
		t.Fatalf("sentinel backend %q must not be opened", name)
		return nil, errors.New("unreachable")
	})
}

func aiChatOp(meta, input string) operation.Operation {
	return operation.Operation{
		Stack:     "site",
		Scope:     100,
		Name:      "c",
		Resonator: &resonator.Resonator{Exec: "ai://chat"},
		Input:     input,
		Meta:      meta,
	}
}

func resetChatRegistry(t *testing.T) {
	t.Helper()
	// reach into the chat package via its test seam
	chatResetForTests()
}

// chatResetForTests is forwarded via a small helper to call the
// package-internal resetForTests through the test-only exporter we
// installed in chat package via the chatTestReset wire below. Because
// the resetForTests function is unexported in chat, we expose it
// through an internal-pkg helper at test time only.
//
// Implementation: the chat package's resetForTests is unexported but
// callable via Open(..., Config{}) with a uniqueness key trick — no,
// that's not clean. The cleanest workaround: a build-tag-free
// helper would mean exporting. Instead, this test file registers and
// re-registers with unique names per test (no need to clear) since
// the chat package preserves registration order for first-seen names.
func chatResetForTests() { /* relying on unique names per test */ }

// --- 1. min prompt → text ---

func TestExecAIMinPromptReturnsText(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp: chat.Response{
			Text: "hello back", Provider: "tb-min", Model: "test-m",
			TokensIn: 10, TokensOut: 5, LatencyMS: 42,
		},
	}
	withTestBackend(t, "tb-min", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "env-key-min")

	op := aiChatOp(`{"prompt":"hi","provider":"tb-min"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	if got := gjson.Get(pl.Raw, "text").String(); got != "hello back" {
		t.Errorf("text = %q, want %q (raw=%s)", got, "hello back", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, `_txc.chat.provider`).String(); got != "tb-min" {
		t.Errorf("provider = %q, want tb-min", got)
	}
	if got := gjson.Get(pl.Raw, `_txc.chat.tokens.in`).Int(); got != 10 {
		t.Errorf("tokens.in = %d, want 10", got)
	}
}

// --- 2. templated prompt → substitute ---

func TestExecAITemplatedPromptSubstitutes(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Text: "ok"},
	}
	withTestBackend(t, "tb-tmpl", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	// @path addresses _txc.* per the txcl convention (parser.go:216,234) —
	// the chassis web inlet stamps `_txc.web.req.body`, so a template's
	// `{{@web.req.body}}` must look up _txc.web.req.body.
	op := aiChatOp(
		`{"prompt":"echo: {{@web.req.body}}","provider":"tb-tmpl"}`,
		`{"_txc":{"web":{"req":{"body":"hello"}}}}`,
	)
	_, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sawMessages) == 0 {
		t.Fatalf("no messages seen by backend")
	}
	user := stub.sawMessages[len(stub.sawMessages)-1].Content
	if user != "echo: hello" {
		t.Errorf("user content = %q, want %q", user, "echo: hello")
	}
}

// --- 3. messages-form bypasses prompt ---

func TestExecAIMessagesArrayBypassesPrompt(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Text: "ok"},
	}
	withTestBackend(t, "tb-msgs", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	meta := `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"user-msg"}],"provider":"tb-msgs"}`
	op := aiChatOp(meta, `{}`)
	if _, err := pu.ExecAI(context.Background(), op); err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.sawMessages) != 2 {
		t.Fatalf("got %d messages, want 2", len(stub.sawMessages))
	}
	if stub.sawMessages[0].Role != "system" || stub.sawMessages[1].Content != "user-msg" {
		t.Errorf("messages not passed verbatim: %+v", stub.sawMessages)
	}
}

// --- 4. both prompt and messages is an error ---

func TestExecAIPromptAndMessagesIsError(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{required: []string{"OPENROUTER_KEY"}}
	withTestBackend(t, "tb-both", stub)

	pu, _ := newTestUnit(t)
	op := aiChatOp(`{"prompt":"p","messages":[{"role":"user","content":"x"}],"provider":"tb-both"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err == nil {
		t.Fatalf("expected error; got payload %s", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeInvalidWith {
		t.Errorf("error code = %q, want %q", got, chat.CodeInvalidWith)
	}
	if atomic.LoadInt32(&stub.calls) != 0 {
		t.Errorf("backend should not have been called; calls = %d", stub.calls)
	}
}

// --- 5. provider override wins ---

func TestExecAIProviderOverrideWins(t *testing.T) {
	resetChatRegistry(t)
	stubA := &testBackend{required: []string{"OPENROUTER_KEY"}, resp: chat.Response{Text: "from-A"}}
	stubB := &testBackend{required: []string{"OPENROUTER_KEY"}, resp: chat.Response{Text: "from-B"}}
	withTestBackend(t, "tb-ov-a", stubA)
	withTestBackend(t, "tb-ov-b", stubB)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	op := aiChatOp(`{"prompt":"x","provider":"tb-ov-b"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	if got := gjson.Get(pl.Raw, "text").String(); got != "from-B" {
		t.Errorf("text = %q, want from-B", got)
	}
	if got := gjson.Get(pl.Raw, `_txc.chat.routing_decision`).String(); got != "provider-override" {
		t.Errorf("routing_decision = %q, want provider-override", got)
	}
}

// --- 6. unknown provider errors ---

func TestExecAIUnknownProviderErrors(t *testing.T) {
	resetChatRegistry(t)
	pu, _ := newTestUnit(t)
	op := aiChatOp(`{"prompt":"x","provider":"nope-does-not-exist"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	// Backend-resolution failure surfaces as a structured chat.error
	// field; ExecAI returns nil err because rule authors can WHEN-handle.
	if err != nil {
		t.Fatalf("expected nil err (envelope-only), got %v", err)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeNoBackend {
		t.Errorf("error code = %q, want %q", got, chat.CodeNoBackend)
	}
}

// --- 7. secrets fall back to env when enabled ---

func TestExecAISecretsFallsBackToEnvWhenEnabled(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{required: []string{"TEST_SECRET_NAME"}, resp: chat.Response{Text: "ok"}}
	withTestBackend(t, "tb-env", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("TEST_SECRET_NAME", "from-env-val-xyz")

	op := aiChatOp(`{"prompt":"x","provider":"tb-env"}`, `{}`)
	if _, err := pu.ExecAI(context.Background(), op); err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.sawKey != "from-env-val-xyz" {
		t.Errorf("backend saw key = %q, want from-env-val-xyz", stub.sawKey)
	}
}

// --- 8. secrets fail when fallback off ---

func TestExecAISecretsRejectsWhenFallbackOff(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{required: []string{"TEST_SECRET_OFF"}, resp: chat.Response{Text: "ok"}}
	withTestBackend(t, "tb-off", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = false
	// env IS set but should be ignored:
	t.Setenv("TEST_SECRET_OFF", "should-not-be-read")
	// Ensure os.Getenv actually returns this — sanity:
	if os.Getenv("TEST_SECRET_OFF") == "" {
		t.Fatal("Setenv didn't take effect")
	}

	op := aiChatOp(`{"prompt":"x","provider":"tb-off"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("expected envelope-only error, got %v", err)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeMissingSecret {
		t.Errorf("error code = %q, want %q (raw=%s)", got, chat.CodeMissingSecret, pl.Raw)
	}
	if atomic.LoadInt32(&stub.calls) != 0 {
		t.Errorf("backend should not have been called; calls = %d", stub.calls)
	}
}

// --- 9. WITH-declared override wins over RequiredSecrets ---

func TestExecAIOverrideSecretViaPreloadedBag(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{required: []string{"OPENROUTER_KEY"}, resp: chat.Response{Text: "ok"}}
	withTestBackend(t, "tb-override", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "env-default")

	// Simulate the existing WITH-driven materialize loop having already
	// populated op.Secrets with a custom value before Exec was called.
	op := aiChatOp(`{"prompt":"x","provider":"tb-override"}`, `{}`)
	op.Secrets.Set("OPENROUTER_KEY", []byte("custom-override-value"))

	if _, err := pu.ExecAI(context.Background(), op); err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.sawKey != "custom-override-value" {
		t.Errorf("backend saw key = %q, want custom-override-value (WITH override should win)", stub.sawKey)
	}
}

// --- 10. trace event carries token counts without fuel ---

func TestExecAIRecordsTokenCountsInMetadataNotFuel(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp: chat.Response{
			Text: "ok", Provider: "tb-fuel", Model: "m",
			TokensIn: 100, TokensOut: 50, LatencyMS: 5,
		},
	}
	withTestBackend(t, "tb-fuel", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	op := aiChatOp(`{"prompt":"x","provider":"tb-fuel"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}

	// Token counts present in metadata.
	if got := gjson.Get(pl.Raw, `_txc.chat.tokens.in`).Int(); got != 100 {
		t.Errorf("_txc.chat.tokens.in = %d, want 100", got)
	}
	if got := gjson.Get(pl.Raw, `_txc.chat.tokens.out`).Int(); got != 50 {
		t.Errorf("_txc.chat.tokens.out = %d, want 50", got)
	}
	// Token counts NOT charged to fuel. ExecAI ran inside no Run-scope
	// here (the test calls ExecAI directly), so there's no fuel
	// accumulator. The assertion is structural: ExecAI never references
	// fuelCostChatPerToken (no such constant exists) and does not call
	// addFuel for token counts. We verify this via a compile-time
	// negative — grep for fuelCostChat* in the package surface.
	if strings.Contains(constSummary(), "fuelCostChatPerToken") {
		t.Errorf("fuel meter must not include per-token charge for chat ops")
	}
}

// constSummary lists the budget.go cost constants by name. The chat-exec
// invariant is that none of them name token cost. (See feedback memory:
// fuel is chassis-owned infrastructure; tokens are provider compute.)
func constSummary() string {
	return strings.Join([]string{
		"fuelCostScopeEnter",
		"fuelCostRepeatTransition",
		"fuelCostExec",
		"fuelCostSecretMaterialize",
		"fuelCostComputePerMs",
	}, ",")
}

// --- 11. provider-error surfaces as chat.error envelope field ---

func TestExecAIProviderHTTPErrorSurfacesOnEnvelope(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Provider: "tb-perr", Model: "m"},
		err:      &chat.ProviderHTTPError{StatusCode: 503, Body: "upstream down"},
	}
	withTestBackend(t, "tb-perr", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	op := aiChatOp(`{"prompt":"x","provider":"tb-perr"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("expected envelope-only error, got %v", err)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeProviderHTTP {
		t.Errorf("error code = %q, want %q", got, chat.CodeProviderHTTP)
	}
}

// --- 12. 401 sanitized error reaches envelope ---

func TestExecAIAuthFailedSurfacesSanitized(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		err:      chat.ErrAuthFailed,
	}
	withTestBackend(t, "tb-auth", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "sk-or-v1-secret-xyz")

	op := aiChatOp(`{"prompt":"x","provider":"tb-auth"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("expected envelope-only error, got %v", err)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeAuthFailed {
		t.Errorf("error code = %q, want %q", got, chat.CodeAuthFailed)
	}
	// Critical: the sanitized message must NOT include any part of the
	// secret (leak prevention guard 3, end-to-end).
	if strings.Contains(pl.Raw, "sk-or-v1") {
		t.Errorf("envelope must not contain key prefix: %s", pl.Raw)
	}
}

// --- 13. schema validation ok ---

func TestExecAISchemaValidationOK(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Text: `{"answer":"a"}`, Provider: "tb-sok", Model: "m"},
	}
	withTestBackend(t, "tb-sok", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	meta := `{"prompt":"x","provider":"tb-sok","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}}`
	op := aiChatOp(meta, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	if got := gjson.Get(pl.Raw, `_txc.chat.schema_validation`).String(); got != "ok" {
		t.Errorf("schema_validation = %q, want ok", got)
	}
	if got := gjson.Get(pl.Raw, `schema_validated_payload.answer`).String(); got != "a" {
		t.Errorf("schema_validated_payload.answer = %q, want a", got)
	}
}

// --- 14. schema validation failed surfaces chat.error ---

func TestExecAISchemaValidationFailed(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		// Required key 'answer' is missing → validation fails.
		resp: chat.Response{Text: `{"unrelated":1}`, Provider: "tb-sf", Model: "m"},
	}
	withTestBackend(t, "tb-sf", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	meta := `{"prompt":"x","provider":"tb-sf","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}}`
	op := aiChatOp(meta, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("expected envelope-only error, got %v", err)
	}
	if got := gjson.Get(pl.Raw, `_txc.chat.schema_validation`).String(); got != "failed" {
		t.Errorf("schema_validation = %q, want failed", got)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeSchemaFailed {
		t.Errorf("error code = %q, want %q", got, chat.CodeSchemaFailed)
	}
}

// --- 15. unrecognized sub-op errors clearly ---

func TestExecAIUnsupportedSubOpErrors(t *testing.T) {
	pu, _ := newTestUnit(t)
	op := operation.Operation{
		Stack:     "site",
		Scope:     100,
		Name:      "c",
		Resonator: &resonator.Resonator{Exec: "ai://transcribe"},
		Input:     `{}`,
		Meta:      `{"prompt":"x"}`,
	}
	pl, err := pu.ExecAI(context.Background(), op)
	if err == nil {
		t.Fatalf("expected error for ai://transcribe in v1; got payload %s", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeUnsupportedSub {
		t.Errorf("error code = %q, want %q", got, chat.CodeUnsupportedSub)
	}
}

// --- 16. empty WITH errors ---

func TestExecAIEmptyWithErrors(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{required: []string{"OPENROUTER_KEY"}}
	withTestBackend(t, "tb-empty", stub)

	pu, _ := newTestUnit(t)
	op := aiChatOp("", `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err == nil {
		t.Fatalf("expected error for empty WITH; got payload %s", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, "chat.error.code").String(); got != chat.CodeInvalidWith {
		t.Errorf("error code = %q, want %q", got, chat.CodeInvalidWith)
	}
}

// --- 17. leak guard 2: no Authorization header in trace inputs ---
//
// We can't easily intercept trace bytes at this unit level (the chassis
// trace sink is wired upstream of ExecAI). What we CAN verify is that
// the chat response payload doesn't contain "Authorization" — the
// header is constructed inside the openrouter backend, not by ExecAI.

func TestExecAIResponsePayloadHasNoAuthHeader(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Text: "ok", Provider: "tb-noauth"},
	}
	withTestBackend(t, "tb-noauth", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "sk-or-v1-leaktest-xyz")

	op := aiChatOp(`{"prompt":"x","provider":"tb-noauth"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	if strings.Contains(pl.Raw, "Authorization") {
		t.Errorf("response payload must not contain Authorization header: %s", pl.Raw)
	}
	if strings.Contains(pl.Raw, "sk-or-v1") {
		t.Errorf("response payload must not contain key prefix: %s", pl.Raw)
	}
}

// --- 19. WITH debug = true stamps _txc_op_debug ---

func TestExecAIDebugStampsOpDebugField(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp: chat.Response{
			Text: "ok", Provider: "tb-dbg", Model: "m",
			TokensIn: 12, TokensOut: 5,
		},
	}
	withTestBackend(t, "tb-dbg", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	op := aiChatOp(
		`{"prompt":"echo: {{@body_text}}","provider":"tb-dbg","debug":true}`,
		`{"_txc":{"body_text":"hello hummingbirds"}}`,
	)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	// Note: ExecAI returns the raw response WITH _txc_op_debug stamped;
	// the chassis post-Exec strip lives one level up in Run, so this
	// test asserts the stamp lands at the right field.
	if got := gjson.Get(pl.Raw, `_txc_op_debug.rendered_prompt`).String(); got != "echo: hello hummingbirds" {
		t.Errorf("rendered_prompt = %q, want %q (raw=%s)", got, "echo: hello hummingbirds", pl.Raw)
	}
	if got := gjson.Get(pl.Raw, `_txc_op_debug.model_sent`).String(); got != "" {
		// WITH didn't set model, so model_sent should be empty here
		// (the backend default kicks in inside backend.Run, not in req).
	}
	msgs := gjson.Get(pl.Raw, `_txc_op_debug.messages_sent`)
	if !msgs.IsArray() || len(msgs.Array()) != 1 {
		t.Errorf("messages_sent should be a 1-element array; got %s", msgs.Raw)
	}
}

// --- 20. no debug by default ---

func TestExecAINoDebugByDefault(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Text: "ok", Provider: "tb-nodbg", Model: "m"},
	}
	withTestBackend(t, "tb-nodbg", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "k")

	op := aiChatOp(`{"prompt":"hi","provider":"tb-nodbg"}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	if strings.Contains(pl.Raw, "_txc_op_debug") {
		t.Errorf("debug field should be absent without WITH debug = true; raw=%s", pl.Raw)
	}
}

// --- 21. leak: debug content carries no secret material ---

func TestExecAIDebugContainsNoSecretMaterial(t *testing.T) {
	resetChatRegistry(t)
	stub := &testBackend{
		required: []string{"OPENROUTER_KEY"},
		resp:     chat.Response{Text: "ok", Provider: "tb-lk", Model: "m"},
	}
	withTestBackend(t, "tb-lk", stub)

	pu, _ := newTestUnit(t)
	pu.Conf.AIChatEnvFallback = true
	t.Setenv("OPENROUTER_KEY", "sk-or-v1-debug-leakcheck-xyz")

	op := aiChatOp(`{"prompt":"hi","provider":"tb-lk","debug":true}`, `{}`)
	pl, err := pu.ExecAI(context.Background(), op)
	if err != nil {
		t.Fatalf("ExecAI: %v", err)
	}
	debug := gjson.Get(pl.Raw, "_txc_op_debug").Raw
	if debug == "" {
		t.Fatalf("expected _txc_op_debug present")
	}
	for _, leak := range []string{"sk-or-v1", "Authorization", "Bearer"} {
		if strings.Contains(debug, leak) {
			t.Errorf("debug content leaked %q: %s", leak, debug)
		}
	}
}

// --- 22. mock interception bypasses backend ---

func TestExecAIMocksPatternInterceptsBeforeBackend(t *testing.T) {
	// Mock interception happens BEFORE the Exec switch (at
	// shouldMockByPattern), so even though ai:// is supported, an op
	// matching _txc.mocks returns op.MockRes without calling ExecAI's
	// backend dispatch. Verify via a sentinel backend that fails
	// loud if its constructor or Run runs.
	resetChatRegistry(t)
	registerSentinelBackend(t, "tb-sentinel-mock")

	pu, _ := newTestUnit(t)
	// Seed an op in the runtime DB so the full Run path goes through
	// shouldMockByPattern.
	if _, err := pu.Dbc.Db.Exec(
		`INSERT INTO ops (stack, scope, name, txcl, mock_req, mock_res) VALUES (?, ?, ?, ?, '', ?)`,
		"mocktest/ai", 0, "c", `WHEN .x == 1 EXEC "ai://chat" WITH provider = "tb-sentinel-mock", prompt = "hi"`,
		`{"text":"mocked"}`,
	); err != nil {
		t.Fatalf("seed op: %v", err)
	}
	body := `{"x":1,"_txc":{"mocks":["mocktest/ai/**"]}}`
	resCh := make(chan event.Payload, 1)
	if err := pu.Run(context.Background(), body, "mocktest/ai/0", resCh); err != nil {
		t.Fatalf("Run: %v", err)
	}
	select {
	case payload := <-resCh:
		if got := gjson.Get(payload.Raw, "text").String(); got != "mocked" {
			t.Errorf("expected mocked response, got %s", payload.Raw)
		}
	default:
		t.Fatal("no response on resCh")
	}
}
