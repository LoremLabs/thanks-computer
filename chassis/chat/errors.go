package chat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Error codes surfaced to operators via response envelope's `chat.error`
// field and trace events. The `txco_chat_*` prefix mirrors the
// budget-exhaustion codes (`txco_fuel_exhausted`, `txcl_scope_ttl_exhausted`)
// so they all sort together in logs and dashboards.
const (
	CodeMissingSecret  = "txco_chat_missing_secret"
	CodeNoBackend      = "txco_chat_no_backend"
	CodeInvalidWith    = "txco_chat_invalid_with"
	CodeAuthFailed     = "txco_chat_auth_failed"
	CodeProviderHTTP   = "txco_chat_provider_http"
	CodeProviderNet    = "txco_chat_provider_net"
	CodeProviderParse  = "txco_chat_provider_parse"
	CodeSchemaFailed   = "txco_chat_schema_failed"
	CodeUnsupportedSub = "txco_chat_unsupported_sub_op"
)

// MissingSecretError signals one of the backend's RequiredSecrets() could
// not be found in either the per-tenant store or (when enabled) the
// chassis-wide env-var fallback. Names are the secret NAME and the
// backend NAME — never any cleartext.
type MissingSecretError struct {
	Backend string `json:"backend"`
	Secret  string `json:"secret"`
}

func (e *MissingSecretError) Error() string {
	return fmt.Sprintf("chat: backend %q missing required secret %q", e.Backend, e.Secret)
}

func (e *MissingSecretError) Code() string { return CodeMissingSecret }

func (e *MissingSecretError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":    CodeMissingSecret,
		"backend": e.Backend,
		"secret":  e.Secret,
	})
	return string(b)
}

// NoBackendError signals no backend matches the requested provider hint.
// Lists the registered backends so the operator can diagnose typos /
// missed blank imports.
type NoBackendError struct {
	ProviderHint string   `json:"provider_hint,omitempty"`
	Registered   []string `json:"registered"`
}

func (e *NoBackendError) Error() string {
	if e.ProviderHint != "" {
		return fmt.Sprintf("chat: no backend registered as %q (registered: %s)",
			e.ProviderHint, strings.Join(e.Registered, ", "))
	}
	return "chat: no backends registered"
}

func (e *NoBackendError) Code() string { return CodeNoBackend }

func (e *NoBackendError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":          CodeNoBackend,
		"provider_hint": e.ProviderHint,
		"registered":    e.Registered,
	})
	return string(b)
}

// InvalidWithError signals a malformed WITH clause set (e.g. both `prompt`
// and `messages` were supplied, or neither). Reason is operator-facing
// English; Detail is structured for trace.
type InvalidWithError struct {
	Reason string            `json:"reason"`
	Detail map[string]string `json:"detail,omitempty"`
}

func (e *InvalidWithError) Error() string { return "chat: invalid WITH: " + e.Reason }
func (e *InvalidWithError) Code() string  { return CodeInvalidWith }

func (e *InvalidWithError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":   CodeInvalidWith,
		"reason": e.Reason,
		"detail": e.Detail,
	})
	return string(b)
}

// UnsupportedSubOpError signals an ai://<op> the chassis doesn't dispatch
// in v1 (only "chat" is wired). Returned by ExecAI before backend
// selection.
type UnsupportedSubOpError struct {
	SubOp string `json:"sub_op"`
}

func (e *UnsupportedSubOpError) Error() string {
	return fmt.Sprintf("chat: unsupported ai:// sub-op %q (v1 supports: chat)", e.SubOp)
}
func (e *UnsupportedSubOpError) Code() string { return CodeUnsupportedSub }

func (e *UnsupportedSubOpError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":   CodeUnsupportedSub,
		"sub_op": e.SubOp,
	})
	return string(b)
}

// ErrAuthFailed is the sanitized auth-failed error returned by backends
// after a 401. The provider's body — which may quote a prefix of the
// submitted API key — is discarded BEFORE this error is constructed.
// Surfaced to operators as a fixed generic; the real diagnosis happens
// from the trace event's `retries` and `latency_ms` fields plus the
// inbound `_txc.chat.error` projection.
var ErrAuthFailed = &authFailedError{}

type authFailedError struct{}

func (e *authFailedError) Error() string { return "chat: authentication failed" }
func (e *authFailedError) Code() string  { return CodeAuthFailed }
func (e *authFailedError) AsJSON() string {
	return `{"code":"` + CodeAuthFailed + `","error":"authentication failed"}`
}

// ProviderHTTPError signals a non-401 HTTP error from the provider. Body
// is the (potentially-sanitized) response body for diagnosis.
type ProviderHTTPError struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body,omitempty"`
}

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("chat: provider returned HTTP %d", e.StatusCode)
}
func (e *ProviderHTTPError) Code() string { return CodeProviderHTTP }
func (e *ProviderHTTPError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":        CodeProviderHTTP,
		"status_code": e.StatusCode,
		"body":        e.Body,
	})
	return string(b)
}

// ProviderNetError signals a network / DNS / timeout failure reaching
// the provider.
type ProviderNetError struct {
	Reason string `json:"reason"`
}

func (e *ProviderNetError) Error() string { return "chat: provider network error: " + e.Reason }
func (e *ProviderNetError) Code() string  { return CodeProviderNet }
func (e *ProviderNetError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":   CodeProviderNet,
		"reason": e.Reason,
	})
	return string(b)
}

// ProviderParseError signals the provider returned a 2xx with a body
// the backend could not decode (malformed JSON, empty body, missing
// required fields). Seen most often when a `:free`-tier provider hits
// a quota and returns an empty 200 instead of a clean 429 — upstream
// flake the chassis can't repair, but should surface with a specific
// code so rule authors can WHEN-handle (e.g., fall back to a paid
// model on @chat.error.code == "txco_chat_provider_parse").
type ProviderParseError struct {
	Reason  string `json:"reason"`
	BodyLen int    `json:"body_len"` // 0 = empty body; positive = malformed
}

func (e *ProviderParseError) Error() string {
	if e.BodyLen == 0 {
		return "chat: provider returned an empty body"
	}
	return "chat: provider returned unparseable body: " + e.Reason
}
func (e *ProviderParseError) Code() string { return CodeProviderParse }
func (e *ProviderParseError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":     CodeProviderParse,
		"reason":   e.Reason,
		"body_len": e.BodyLen,
	})
	return string(b)
}

// SchemaFailedError signals the model's response failed JSON-schema
// validation. The request still completes (the raw text is preserved in
// the envelope); rule authors handle via `WHEN @chat.error EXEC ...`.
type SchemaFailedError struct {
	Reason string `json:"reason"`
}

func (e *SchemaFailedError) Error() string { return "chat: schema validation failed: " + e.Reason }
func (e *SchemaFailedError) Code() string  { return CodeSchemaFailed }
func (e *SchemaFailedError) AsJSON() string {
	b, _ := json.Marshal(map[string]any{
		"code":   CodeSchemaFailed,
		"reason": e.Reason,
	})
	return string(b)
}

// CodedError lets callers extract a stable `txco_chat_*` code from any
// chat-package error without a type switch over every variant.
type CodedError interface {
	error
	Code() string
	AsJSON() string
}
