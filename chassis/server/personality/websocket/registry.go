package websocket

import (
	"context"
	"errors"
	"time"
)

// MessageType is the application-level frame kind. Our own type so the
// op layer never sees the WebSocket library's.
type MessageType int

const (
	MessageText MessageType = iota + 1
	MessageBinary
)

// String renders the envelope spelling of the type.
func (t MessageType) String() string {
	if t == MessageBinary {
		return "binary"
	}
	return "text"
}

// Errors the Registry returns; the ops map them to `<into>.error.code`.
var (
	ErrDisabled        = errors.New("websocket personality is not enabled on this node")
	ErrSessionNotFound = errors.New("no such session")
	ErrSessionClosed   = errors.New("session is closed")
	ErrWriteTimeout    = errors.New("write timed out")
	ErrMessageTooLarge = errors.New("message exceeds the session's size limit")
	ErrPendingFull     = errors.New("too many unclaimed accepts")
)

// Accept is the decision a stack makes with txco://websocket/accept. The
// op fills it from its WITH clause and the pinned run identity; the web
// head consumes it at claim time.
type Accept struct {
	// Tenant is the pinned tenant slug of the upgrade run (unforgeable).
	Tenant string
	// Stack is the web stack that accepted; session runs enter
	// <Stack>/_websocket/0.
	Stack string
	// Ingress and HostnameVerified are copied from the upgrade run's
	// route facts onto every session run's route pre-stamp.
	Ingress          string
	HostnameVerified bool

	// State is a JSON object (≤ StateMaxBytes) stamped verbatim as
	// `_txc.websocket.session.state` on every message; "" = none.
	State string
	// Subprotocols the stack is willing to speak, in preference order;
	// SubprotocolRequired refuses the upgrade when none is offered.
	Subprotocols        []string
	SubprotocolRequired bool
	// Origins are host patterns allowed cross-origin; AnyOrigin disables
	// the check. With neither, only same-host origins (or no Origin
	// header — a non-browser client) are accepted.
	Origins   []string
	AnyOrigin bool
	// Events the stack wants as runs beyond messages (EventClose).
	Events map[string]bool
	// Per-session overrides, 0 = chassis default; the controller clamps
	// them to the chassis ceilings.
	IdleTimeout     time.Duration
	MaxMessageBytes int64
}

// Registry is the surface the txco://websocket/* ops depend on. The
// Controller implements it for this node; a fleet session directory wraps
// it later without touching the ops.
type Registry interface {
	Enabled() bool
	// RecordAccept stores a stack's decision for the session id the web head
	// minted on the upgrade request.
	RecordAccept(sid string, a Accept) error
	// Send writes one message to a live session of the tenant.
	Send(ctx context.Context, tenant, id string, typ MessageType, data []byte) error
	// CloseSession starts the close handshake on a live session of the tenant.
	CloseSession(ctx context.Context, tenant, id string, code int, reason string) error
}

// ValidCloseCode reports whether a stack may close with this status code:
// the RFC 6455 codes a peer may send plus the private range. 1005/1006/1015
// are reserved for the transport itself.
func ValidCloseCode(code int) bool {
	switch code {
	case 1000, 1001, 1003, 1007, 1008, 1009, 1010, 1011:
		return true
	}
	return code >= 3000 && code <= 4999
}
