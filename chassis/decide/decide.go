// Package decide is the chassis-owned `ai://decide` exec dispatch surface.
//
// Where ai://chat turns context into generated language and ai://embed turns
// content into a vector, ai://decide turns state plus bounded, typed
// questions into probabilistic judgments:
//
//	noul    a proposition           → P(true)
//	choice  one of the given names  → the name, plus a distribution over names
//	score   an ordered rubric       → an interpolated score, plus a distribution
//
// The answer space is always supplied by the caller, so a backend can never
// return an option that was not offered.
//
// **Judgments, not policy.** decide returns probabilities and nothing else.
// Thresholds, fallbacks, allow/deny and escalation belong to the stack, in
// ordinary WHEN clauses over the result. There is deliberately no threshold
// parameter anywhere in this package.
//
// Like chat and embed it is a thin registry: backends self-register via
// init() in their subpackages, and the chassis activates one with a blank
// import. The contract types below name no provider; a backend translates
// them to its own wire shape (e.g. `noul` may travel as `boolean`).
//
// **Boundary of trust.** Same as chat and embed: RequiredSecrets() declares
// the standardized secret names a backend needs; the ExecAI handler
// materializes them through the per-tenant store (with the optional env
// fallback); cleartext rides only in the *secrets.SecretBag passed to Decide.
package decide

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/secrets"
)

// Question types. These are the provider-independent names; a backend maps
// them onto its wire vocabulary.
const (
	TypeNoul   = "noul"
	TypeChoice = "choice"
	TypeScore  = "score"
)

// Backend is the chassis-facing interface every decision backend implements.
//
// Lifecycle: backends register by name via Register() in an init(); the
// chassis resolves one per ai://decide EXEC. Decide must be safe for
// concurrent use — the chassis dispatches ops in parallel on fan-out.
type Backend interface {
	// Name returns the registered name (must match the Register key).
	Name() string

	// DefaultModel is the model used when a request omits WITH model.
	DefaultModel() string

	// RequiredSecrets are the standardized secret names this backend needs.
	RequiredSecrets() []string

	// Decide asks every question in req against req.State in one call. It
	// translates the provider's answers into Response and returns them as
	// reported; Normalize (called by the handler) enforces the contract —
	// every question answered, choices from the offered set, probabilities
	// in range. The bag carries cleartext for every name in
	// RequiredSecrets(); implementations keep it in local variables only.
	Decide(ctx context.Context, req Request, bag *secrets.SecretBag) (Response, error)
}

// Config carries decide-package construction options resolved from chassis
// config. Backends extend it with their own fields without breaking callers
// (same convention as chat.Config and embed.Config).
type Config struct {
	// HTTPClient is the chassis-owned http.Client (egress-guarded). Every
	// outbound call from a backend MUST use it.
	HTTPClient *http.Client

	// ZeroDataRetention asks the provider not to retain request data, where
	// the backend can express that (chassis config
	// decide-zero-data-retention).
	ZeroDataRetention bool
}

// Constructor builds a Backend from resolved config.
type Constructor func(Config) (Backend, error)

// Request is the chassis-normalized decision request.
type Request struct {
	// State is the evidence every question is asked against: a JSON string,
	// object, or array, passed to the provider as data.
	State json.RawMessage

	// Questions in author order. Keys are unique.
	Questions []Question

	// Model is the resolved model identifier.
	Model string

	// Intent is a trace-only label (e.g. "pony_ingest_place").
	Intent string
}

// Question is one bounded question. Which criteria field is set depends on
// Type.
type Question struct {
	// Key is the caller's id for the question; the answer comes back under
	// the same key.
	Key string

	// Type is TypeNoul, TypeChoice, or TypeScore.
	Type string

	// Instructions is the question itself.
	Instructions string

	// Choices are the offered options for a choice question, in author
	// order.
	Choices []Option

	// Levels are a score question's rubric, lowest to highest.
	Levels []string

	// True and False optionally describe what each answer to a noul
	// question means. Empty = not supplied.
	True  string
	False string
}

// Option is one choice: the name the answer returns, and a description of
// what it covers.
type Option struct {
	Name        string
	Description string
}

// Answer is one question's typed judgment. Which fields are meaningful
// depends on Type; after Normalize they are:
//
//	noul    Probability = P(true)
//	choice  Choice, Probability = P(Choice), Probabilities by option name
//	score   Score, Probabilities by level index ("0" = lowest)
//
// Score is interpolated on the level-index scale: 0 is the lowest level and
// len(Levels)-1 the highest. A score answer carries no Probability — the
// distribution and the score describe different things.
//
// Confidence is the provider's own confidence in the answer, carried only
// when the provider reports one (Jev reports it for choice and score, not
// noul). It is not a probability and is never derived from one.
type Answer struct {
	Key           string
	Type          string
	Probability   *float64
	Choice        string
	Score         *float64
	Probabilities map[string]float64
	Confidence    *float64
}

// Response is the chassis-normalized decision result. Backends translate the
// provider's on-wire shape into this struct.
type Response struct {
	// Answers, one per question. After Normalize they are in question order.
	Answers []Answer

	// Provider is the backend name; Model is the model requested.
	Provider string
	Model    string

	// ResolvedModel is the model the provider reports having run, verbatim
	// ("" when it reports none). It may be an alias rather than a version;
	// the chassis records it and invents nothing.
	ResolvedModel string

	// Provider-reported token usage (0 if unreported). Recorded in trace and
	// the envelope; never charged to fuel.
	InputTokens  int64
	OutputTokens int64

	// Cost is the provider-reported cost in USD; nil when unreported.
	Cost *float64

	// GenerationID is the provider's id for this call, for reconciliation.
	GenerationID string

	// LatencyMS is wall-clock from request build to response parse.
	LatencyMS int64

	// Retries is the count of provider retries performed.
	Retries int
}
