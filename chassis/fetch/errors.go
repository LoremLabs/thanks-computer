package fetch

import (
	"fmt"
	"net/http"
)

// Error codes. Callers prefix them with their op family (txco_html_…).
const (
	CodeInvalidURL          = "invalid_url"
	CodeDestinationDenied   = "destination_denied"
	CodeDNSFailed           = "dns_failed"
	CodeConnectionFailed    = "connection_failed"
	CodeTimeout             = "timeout"
	CodeRedirectLimit       = "redirect_limit"
	CodeResponseTooLarge    = "response_too_large"
	CodeUnsupportedType     = "unsupported_content_type"
	CodeUnsupportedEncoding = "unsupported_encoding"
	CodeHTTPError           = "http_error"
)

// Error is a failed fetch. Message is fixed text written for the rule
// author: it never names an address, a resolver or a connection detail.
// Status and ContentType are set when a response arrived.
type Error struct {
	Code        string
	Message     string
	Status      int
	ContentType string
	// Bytes is how many body bytes were read before the failure (for
	// metering a refused oversize body).
	Bytes int64
	// Header is the response's, when one arrived: a caller reading a
	// provider's rate-limit headers off a 429 finds them here.
	Header http.Header
}

func (e *Error) Error() string { return fmt.Sprintf("fetch: %s: %s", e.Code, e.Message) }

func errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
