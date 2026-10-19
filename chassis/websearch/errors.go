package websearch

import "fmt"

// Error codes. The handler prefixes them txco_web_. The fetcher's own
// codes (destination_denied, dns_failed, connection_failed, timeout,
// response_too_large) pass through under the same prefix.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeMissingSecret    = "missing_secret"
	CodeDisabled         = "disabled"
	CodeRateLimited      = "rate_limited"
	CodeBusy             = "busy"
	CodeTimeout          = "timeout"
	CodeProviderAuth     = "provider_auth"
	CodeProviderError    = "provider_error"
	CodeResponseTooLarge = "response_too_large"
	CodeInvalidResponse  = "invalid_response"
	CodeNoTenant         = "no_tenant"
	CodeInternal         = "internal_error"
)

// Error is a failed search, as the rule sees it at `into`. Message is
// fixed text for the rule author: it never carries the provider's body,
// a header or an address. Status is the provider's, when it answered.
// RetryAfter is seconds, on rate_limited from the provider.
type Error struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Status     int    `json:"status,omitempty"`
	RetryAfter int    `json:"retry_after,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("websearch: %s: %s", e.Code, e.Message) }

// Errorf builds an Error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
