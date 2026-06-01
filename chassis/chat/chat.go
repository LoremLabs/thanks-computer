// Package chat is the chassis-owned `ai://chat` exec dispatch surface.
//
// Like chassis/compute and chassis/egress, it is a thin registry: backends
// (OpenRouter, future OpenAI-direct, Anthropic-direct, local-vllm) self-
// register via init() in their subpackages; the chassis activates one with
// a blank import. Per-call selection is handled by Resolve.
//
// **Boundary of trust.** The chassis owns three concerns that raw HTTP
// wouldn't enforce:
//
//   - Secret materialization. RequiredSecrets() declares which standardized
//     names (OPENROUTER_KEY, OPENAI_KEY, …) a backend needs; the ExecAI
//     handler materializes them through the same per-tenant store + audit
//     counter every other op uses (with an optional env-var fallback gated
//     by AIChatEnvFallback for developer-machine convenience).
//   - Cleartext containment. Cleartext rides only in the *secrets.SecretBag
//     passed to Run; the bag panics on every standard encoder, so it cannot
//     reach trace, log, mock, or continuation by construction.
//   - Fuel + trace. Chat ops pay the standard 25-fuel EXEC + 100-fuel
//     per-secret charge — chassis-owned infrastructure cost. Token counts
//     are recorded in the trace event and `_txc.chat.tokens` envelope
//     metadata, NOT charged to fuel: tokens are provider compute, and
//     conflating them with the chassis's per-request meter would distort
//     the bound. USD-denominated cost reporting (deferred) sources from
//     token counts × per-model pricing, not from fuel.
//
// **v1 is intentionally boring.** No tool-call loop (Backend has no Tools()
// method; Request has no Tools field; Response has no ToolCalls). No
// capability-matched routing across multiple backends (Resolve is
// provider-override-or-first-registered). No sophisticated retry policy
// (the OpenRouter backend retries once on 5xx + network). No raw
// `{{!@field}}` template insertion (rejected with a clear error so v1.1
// can land it without silent semantic drift). Each escalation belongs to
// its own focused PR.
package chat

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// Backend is the chassis-facing interface every chat backend implements.
//
// Lifecycle: backends are registered by name via Register() in an init();
// the chassis Opens one at startup per Config; the resolved Backend is
// then long-lived across requests (Run is called once per ai://chat EXEC).
//
// Run must be safe for concurrent use — the chassis dispatches multiple
// chat ops in parallel when WHEN/EMIT fan-out hits the same backend.
type Backend interface {
	// Name returns the registered name (must match the Register key).
	Name() string

	// Capabilities returns descriptive labels recorded on the trace event
	// for observability. v1 does NOT match against these for routing;
	// capability-superset selection across multiple backends is its own
	// follow-up PR.
	Capabilities() []string

	// RequiredSecrets are the standardized secret names this backend
	// needs to function. ExecAI materializes them automatically from the
	// per-tenant secret store at backend-selection time (with optional
	// env-var fallback) and passes the cleartext via the SecretBag.
	// E.g., the OpenRouter backend returns []string{"OPENROUTER_KEY"};
	// a self-hosted backend that needs URL + auth might return both.
	RequiredSecrets() []string

	// Run executes one chat completion. The secrets bag carries
	// cleartext for every name in RequiredSecrets(); the implementation
	// reads via bag.Get(name) and uses the cleartext only in local
	// variables — never on the Backend struct, never on context, never
	// in trace fields, never in logs. See the package godoc for the
	// five leak-prevention guards.
	Run(ctx context.Context, req Request, bag *secrets.SecretBag) (Response, error)
}

// Config carries chat-package construction options resolved from chassis
// config. Backends extend it with their own fields without breaking
// existing callers (same convention as compute.EngineConfig and
// egress.Config).
type Config struct {
	// HTTPClient is the chassis-owned http.Client (constructed in
	// processor.New with egress.DialControl + otelhttp wrapping). Every
	// outbound HTTPS call from a backend MUST use this client so the
	// configured egress Guard applies. Backends never construct their
	// own transport.
	HTTPClient *http.Client
}

// Constructor builds a Backend from resolved config.
type Constructor func(Config) (Backend, error)

// --- request / response ---

// Request is the chassis-normalized chat completion request. The handler
// decodes op.Meta (WITH-clause materialization) into this shape and the
// backend translates to its on-wire format (OpenAI-compatible for the v1
// backend).
type Request struct {
	// Messages is the conversation. Either authored by hand via
	// WITH messages = [...], or built by the handler from
	// WITH system = "..." + WITH prompt = "..." after template render.
	Messages []Message `json:"messages"`

	// Schema, when non-nil, triggers structured-output mode. The handler
	// validates the model's response against this schema after the call;
	// failure populates `chat.error` in the response envelope. Repair
	// semantics (distinguishing "ok" / "repaired" / "failed") is deferred
	// to a follow-up; v1 is binary ok-or-failed.
	Schema json.RawMessage `json:"schema,omitempty"`

	// Model is the resolved model identifier handed to the provider.
	// v1: WITH model = "..." is passed through verbatim; if unset, the
	// backend picks its default.
	Model string `json:"model,omitempty"`

	// Limits scopes the per-call work the backend is allowed to do.
	Limits Limits `json:"limits"`

	// Intent is a trace-only label authors can stamp for diagnosability
	// (e.g. "classify_support_ticket"). Not sent on the wire.
	Intent string `json:"intent,omitempty"`
}

// Message is one turn in the conversation. JSON shapes match the
// OpenAI-compatible API every v1 provider speaks.
type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
	Name    string `json:"name,omitempty"`
}

// Limits caps per-call work. Zero values mean "backend default."
type Limits struct {
	TimeoutMs  int     `json:"timeout_ms,omitempty"`
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// Response is the chassis-normalized chat completion result. Backends
// translate the provider's on-wire shape into this struct.
//
// Tool-loop fields (AssistantMessage, ToolCalls) are intentionally absent
// in v1 — the tool-loop PR adds them as a non-breaking extension along
// with a Tools() method on the Backend interface.
type Response struct {
	// Text is the assistant's final message content.
	Text string `json:"text"`

	// SchemaValidated, when non-nil, is the schema-validated payload
	// (Text parsed as JSON against Request.Schema). Present when
	// Request.Schema was set AND validation succeeded.
	SchemaValidated json.RawMessage `json:"schema_validated_payload,omitempty"`

	// Provider, Model are observability fields surfaced in the trace
	// event and the response envelope.
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// TokensIn, TokensOut are reported by the provider. Recorded in
	// trace + `_txc.chat.tokens` metadata; NEVER charged to the chassis
	// fuel meter (provider compute is a separate dimension; see the
	// package godoc).
	TokensIn  int64 `json:"tokens_in"`
	TokensOut int64 `json:"tokens_out"`

	// LatencyMS is wall-clock from request build to response parse.
	LatencyMS int64 `json:"latency_ms"`

	// Retries is the count of provider retries the backend performed
	// (0 if the first attempt succeeded).
	Retries int `json:"retries"`
}
