package decide

import "fmt"

// CodedError is implemented by decide errors that carry a stable
// txco_decide_* code. The ExecAI handler surfaces the code at
// `<into>.error.code` so a stack can dispatch on it.
type CodedError interface {
	error
	Code() string
}

// NoBackendError is returned by Resolve when a provider hint names an
// unregistered backend, or when no backend is registered at all.
type NoBackendError struct {
	ProviderHint string
	Registered   []string
}

func (e *NoBackendError) Error() string {
	if e.ProviderHint != "" {
		return fmt.Sprintf("decide: no backend %q (registered: %v)", e.ProviderHint, e.Registered)
	}
	return "decide: no decision backend registered"
}
func (e *NoBackendError) Code() string { return "txco_decide_no_backend" }

// MissingSecretError is returned when a backend's RequiredSecrets() name is
// absent from the per-tenant store (and env fallback, when enabled).
type MissingSecretError struct {
	Backend string
	Secret  string
}

func (e *MissingSecretError) Error() string {
	return fmt.Sprintf("decide: backend %q missing required secret %q", e.Backend, e.Secret)
}
func (e *MissingSecretError) Code() string { return "txco_decide_missing_secret" }

// InvalidWithError flags a request the chassis refuses before any provider
// call: a malformed WITH clause, or a question outside a backend's known
// limits. Nothing is billed.
type InvalidWithError struct {
	Reason string
}

func (e *InvalidWithError) Error() string { return "decide: invalid WITH clause: " + e.Reason }
func (e *InvalidWithError) Code() string  { return "txco_decide_invalid_with" }

// ProviderHTTPError carries a non-2xx provider response (message already
// extracted, sanitized and bounded by the backend).
type ProviderHTTPError struct {
	StatusCode int
	Body       string
}

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("decide: provider HTTP %d: %s", e.StatusCode, e.Body)
}
func (e *ProviderHTTPError) Code() string { return "txco_decide_provider_http" }

// ProviderNetError carries a network/DNS failure reaching the provider
// (after the backend's retry budget is spent).
type ProviderNetError struct {
	Reason string
}

func (e *ProviderNetError) Error() string { return "decide: provider network error: " + e.Reason }
func (e *ProviderNetError) Code() string  { return "txco_decide_provider_net" }

// TimeoutError reports that the op's deadline (WITH timeout, or
// ai-default-timeout) passed before the provider answered.
type TimeoutError struct {
	Reason string
}

func (e *TimeoutError) Error() string { return "decide: timed out: " + e.Reason }
func (e *TimeoutError) Code() string  { return "txco_decide_timeout" }

// ProviderParseError flags an empty or malformed provider response body.
type ProviderParseError struct {
	Reason  string
	BodyLen int
}

func (e *ProviderParseError) Error() string {
	return fmt.Sprintf("decide: provider response parse failed (%d bytes): %s", e.BodyLen, e.Reason)
}
func (e *ProviderParseError) Code() string { return "txco_decide_provider_parse" }

// InvalidAnswerError flags a well-formed provider response whose answers
// break the contract: a question left unanswered, an answer of the wrong
// type, a choice that was not offered, a probability out of range. The whole
// call fails — the op never returns a partial set of answers.
type InvalidAnswerError struct {
	Question string
	Reason   string
}

func (e *InvalidAnswerError) Error() string {
	return fmt.Sprintf("decide: invalid answer for question %q: %s", e.Question, e.Reason)
}
func (e *InvalidAnswerError) Code() string { return "txco_decide_invalid_answer" }
