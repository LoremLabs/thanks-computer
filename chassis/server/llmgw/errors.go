package llmgw

import (
	"net/http"

	"github.com/loremlabs/thanks-computer/chassis/jsonx"
)

// Anthropic error `type` tokens the gateway emits for its own failures.
// Upstream error bodies are never rewritten — these cover only errors the
// gateway itself originates (auth, routing, stack verdicts, transport).
const (
	errTypeInvalidRequest  = "invalid_request_error"
	errTypeAuthentication  = "authentication_error"
	errTypePermission      = "permission_error"
	errTypeNotFound        = "not_found_error"
	errTypeRequestTooLarge = "request_too_large"
	errTypeRateLimit       = "rate_limit_error"
	errTypeAPI             = "api_error"
)

// anthropicErrorJSON shapes an Anthropic Messages API error body:
// {"type":"error","error":{"type":<errType>,"message":<msg>}}. Clients
// that speak the protocol (Claude Code et al.) surface these natively,
// so the gateway is indistinguishable from the real endpoint even when
// it is the one rejecting.
func anthropicErrorJSON(errType, msg string) []byte {
	b := jsonx.New()
	b.Set("type", "error")
	b.Set("error.type", errType)
	b.Set("error.message", msg)
	return []byte(b.String())
}

// writeAnthropicError writes a gateway-originated error response. The
// retryAfter seconds value (from the admission gate's suggestion) is
// optional; empty omits the header.
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg, retryAfter string) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.WriteHeader(status)
	_, _ = w.Write(anthropicErrorJSON(errType, msg))
}
