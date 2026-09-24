package outlet

import "errors"

// Error codes a stack can dispatch on at `<into>.error.code`. Every failure
// of an outlet call is data at `into` with a nil Go error, so the op merges
// and the next scope can branch — the ai://decide shape.
const (
	// CodeNotDeclared: the outlet isn't in the stack's declaration (also
	// refused at apply).
	CodeNotDeclared = "txco_outlet_not_declared"
	// CodeMissingSecret: the declared secret isn't set for this stack or
	// tenant, or no secret store is configured.
	CodeMissingSecret = "txco_outlet_missing_secret"
	// CodeInvalidRequest: wrong argument shape, an argument the driver can't
	// bind, a bad `into`, or exec on a read outlet.
	CodeInvalidRequest = "txco_outlet_invalid_request"
	// CodeResultTooLarge: the row or byte ceiling was crossed. No rows are
	// returned, and an exec rolled back.
	CodeResultTooLarge = "txco_outlet_result_too_large"
	// CodeOutcomeUnknown: a mutating transaction's fate couldn't be
	// determined (the connection died while COMMIT was in flight). Never
	// retried automatically; the op must look before acting again.
	CodeOutcomeUnknown = "txco_outlet_outcome_unknown"
	// CodeConnectFailed: DNS, TLS, refused, or blocked by the egress guard.
	// The statement was never sent.
	CodeConnectFailed = "txco_outlet_connect_failed"
	// CodeAuthFailed: the database rejected the credentials.
	CodeAuthFailed = "txco_outlet_auth_failed"
	// CodeTimeout: the deadline passed, pool wait included.
	CodeTimeout = "txco_outlet_timeout"
	// CodeConstraint: a constraint violation (SQLSTATE class 23).
	CodeConstraint = "txco_outlet_constraint"
	// CodeQueryFailed: any other database error; SQLState is set when known.
	CodeQueryFailed = "txco_outlet_query_failed"
	// CodeUnavailable: shutdown, failover, too many connections, or the
	// outlet runtime itself isn't configured.
	CodeUnavailable = "txco_outlet_unavailable"
)

// Error is the classified failure of an outlet call. Message is a fixed
// string chosen by the chassis or the driver — never a driver's own error
// text, which names hosts, users and databases. SQLState carries the
// database's code when there is one. Rows and Bytes report how far a read
// got before a ceiling fired (CodeResultTooLarge), for the trace only.
type Error struct {
	Code     string
	Message  string
	SQLState string
	Rows     int
	Bytes    int64
}

func (e *Error) Error() string {
	if e.SQLState != "" {
		return "outlet: " + e.Code + " (" + e.SQLState + "): " + e.Message
	}
	return "outlet: " + e.Code + ": " + e.Message
}

// NewError builds a classified failure with a fixed message.
func NewError(code, message string) *Error { return &Error{Code: code, Message: message} }

// AsError extracts an *Error from err, or wraps an unclassified error under
// fallback with a fixed message so no driver text reaches stack state.
func AsError(err error, fallback, message string) *Error {
	var oe *Error
	if errors.As(err, &oe) {
		return oe
	}
	return &Error{Code: fallback, Message: message}
}

// ErrNotDeclared is returned by a DeclSource when the stack's active version
// has no declaration of that name.
var ErrNotDeclared = errors.New("outlet: not declared")
