package embed

import "fmt"

// CodedError is implemented by embed errors that carry a stable
// txco_embed_* code. The ExecAI handler surfaces the code on the response
// envelope's top-level `embed.error.code` so rule authors can dispatch
// uniformly with `WHEN @embed.error EXEC ...`.
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
		return fmt.Sprintf("embed: no backend %q (registered: %v)", e.ProviderHint, e.Registered)
	}
	return "embed: no embedding backend registered"
}
func (e *NoBackendError) Code() string { return "txco_embed_no_backend" }

// MissingSecretError is returned when a backend's RequiredSecrets() name is
// absent from the per-tenant store (and env fallback, when enabled).
type MissingSecretError struct {
	Backend string
	Secret  string
}

func (e *MissingSecretError) Error() string {
	return fmt.Sprintf("embed: backend %q missing required secret %q", e.Backend, e.Secret)
}
func (e *MissingSecretError) Code() string { return "txco_embed_missing_secret" }

// InvalidWithError flags a malformed WITH clause (the rule author's bug).
type InvalidWithError struct {
	Reason string
}

func (e *InvalidWithError) Error() string { return "embed: invalid WITH clause: " + e.Reason }
func (e *InvalidWithError) Code() string  { return "txco_embed_invalid_with" }

// ProviderHTTPError carries a non-2xx provider response (body already
// truncated/sanitized by the backend).
type ProviderHTTPError struct {
	StatusCode int
	Body       string
}

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("embed: provider HTTP %d: %s", e.StatusCode, e.Body)
}
func (e *ProviderHTTPError) Code() string { return "txco_embed_provider_http" }

// ProviderNetError carries a network/DNS/timeout failure reaching the
// provider (after the backend's retry budget is spent).
type ProviderNetError struct {
	Reason string
}

func (e *ProviderNetError) Error() string { return "embed: provider network error: " + e.Reason }
func (e *ProviderNetError) Code() string  { return "txco_embed_provider_net" }

// ProviderParseError flags an empty or malformed provider response body.
type ProviderParseError struct {
	Reason  string
	BodyLen int
}

func (e *ProviderParseError) Error() string {
	return fmt.Sprintf("embed: provider response parse failed (%d bytes): %s", e.BodyLen, e.Reason)
}
func (e *ProviderParseError) Code() string { return "txco_embed_provider_parse" }
