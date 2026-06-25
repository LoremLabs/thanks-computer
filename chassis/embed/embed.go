// Package embed is the chassis-owned `ai://embed` exec dispatch surface.
//
// Like chassis/chat, it is a thin registry: backends (ollama-local first,
// OpenAI-direct next) self-register via init() in their subpackages; the
// chassis activates one with a blank import. Per-call selection is handled
// by Resolve (provider override, else first-registered).
//
// **Separation of concerns.** embed turns text into a vector and persists
// NOTHING. Storing and searching vectors is a separate primitive
// (txco://vector). This mirrors the design doc's split: embedding belongs to
// AI; vector storage/retrieval belongs to infrastructure.
//
// **Boundary of trust.** Same as chat: RequiredSecrets() declares which
// standardized names a backend needs (the ollama backend needs none — it
// talks to a local, keyless endpoint); the ExecAI handler materializes them
// through the per-tenant store with optional env fallback; cleartext rides
// only in the *secrets.SecretBag passed to Embed.
//
// **v1 is intentionally boring.** Batch is first-class (one round-trip embeds
// many texts), but there is no caching, no automatic chunking of
// over-long inputs, and no cross-backend capability routing. Each escalation
// is its own focused change.
package embed

import (
	"context"
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// Backend is the chassis-facing interface every embedding backend implements.
//
// Lifecycle: backends register by name via Register() in an init(); the
// chassis resolves one per ai://embed EXEC. Embed must be safe for concurrent
// use — the chassis dispatches embed ops in parallel on WHEN/EMIT fan-out.
type Backend interface {
	// Name returns the registered name (must match the Register key).
	Name() string

	// Capabilities returns descriptive labels recorded on the trace event
	// for observability. v1 does NOT route on them.
	Capabilities() []string

	// DefaultModel is the model used when a request omits WITH model.
	DefaultModel() string

	// RequiredSecrets are the standardized secret names this backend needs.
	// The ollama backend returns nil (local, unauthenticated); the OpenAI
	// backend will return []string{"OPENAI_KEY"}.
	RequiredSecrets() []string

	// Embed returns one vector per input text, in the same order. The bag
	// carries cleartext for every name in RequiredSecrets(); implementations
	// read via bag.Get and keep cleartext in local variables only.
	Embed(ctx context.Context, req Request, bag *secrets.SecretBag) (Response, error)
}

// Config carries embed-package construction options resolved from chassis
// config. Backends extend it with their own fields without breaking callers
// (same convention as chat.Config).
type Config struct {
	// HTTPClient is the chassis-owned http.Client (egress-guarded). Every
	// outbound call from a backend MUST use it; backends never build their
	// own transport.
	HTTPClient *http.Client

	// OllamaBaseURL is the base URL for the ollama backend (e.g.
	// http://localhost:11434). Empty → the backend's localhost default.
	OllamaBaseURL string
}

// Constructor builds a Backend from resolved config.
type Constructor func(Config) (Backend, error)

// Request is the chassis-normalized embedding request. The handler decodes
// op.Meta (WITH-clause materialization) into this shape.
type Request struct {
	// Texts are the inputs to embed, in order. Single-text callers pass a
	// length-1 slice; batch is first-class.
	Texts []string `json:"texts"`

	// Model is the resolved model identifier; empty → backend default.
	Model string `json:"model,omitempty"`

	// Dimensions optionally requests a truncated embedding (Matryoshka).
	// Zero → the model's native dimension. Backends that can't truncate
	// ignore it; the chassis records the actual dimension returned.
	Dimensions int `json:"dimensions,omitempty"`

	// Intent is a trace-only label (e.g. "embed_book_profile").
	Intent string `json:"intent,omitempty"`
}

// Response is the chassis-normalized embedding result. Backends translate the
// provider's on-wire shape into this struct.
type Response struct {
	// Vectors holds one embedding per input text, in input order.
	Vectors [][]float32 `json:"vectors"`

	// Provider, Model are observability fields surfaced in trace + envelope.
	Provider string `json:"provider"`
	Model    string `json:"model"`

	// Dimensions is the actual length of each returned vector (0 when no
	// vectors were produced, e.g. on error).
	Dimensions int `json:"dimensions"`

	// Tokens is the provider-reported input token count (0 if unreported).
	// Recorded in trace + `_embed` metadata; never charged to fuel.
	Tokens int64 `json:"tokens"`

	// LatencyMS is wall-clock from request build to response parse.
	LatencyMS int64 `json:"latency_ms"`

	// Retries is the count of provider retries performed (0 on first-try
	// success).
	Retries int `json:"retries"`
}
