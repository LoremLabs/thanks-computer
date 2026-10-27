package processor

import (
	"context"
	"net/http"
	"net/url"

	"github.com/tidwall/sjson"

	"github.com/loremlabs/thanks-computer/chassis/connection"
	"github.com/loremlabs/thanks-computer/chassis/event"
)

// A connection is a principal's identity at an outside service
// (chassis/connection). The https op uses one when a WITH clause says
//
//	secrets.headers.authorization.connection = "github"
//
// and the chassis decides whose token that is from what is pinned on the
// call, never from data: the principal and capability the gateway decided
// (WithCapAuthority), and the op's own stack.

// ConnectionUse is the connection service as the https op sees it. Set by
// the server (chassis/server) when the identity store and a master key are
// both present; nil otherwise, and a ref answers txco_connection_unavailable.
type ConnectionUse interface {
	Use(ctx context.Context, in connection.UseInput) (connection.UseResult, *connection.Error)
	// HTTPClient is the client a request that carries the token is made
	// with: guarded at the dial, following no redirect.
	HTTPClient(u *url.URL) *http.Client
}

type ctxKeyCapAuthority struct{}

type capAuthority struct{ principal, capability string }

// WithCapAuthority pins the authority a capability's frame runs under: the
// principal the call was decided for and the capability's name. The frame
// and every call:// frame below it inherit the context, so the principal
// stays fixed down the whole chain and nothing a rule writes can change
// it. What may USE a connection is decided per op, against the op's own
// stack (connection.Service.Use): the pin is the principal, not a right.
func WithCapAuthority(ctx context.Context, principal, capability string) context.Context {
	if principal == "" || capability == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyCapAuthority{}, capAuthority{principal: principal, capability: capability})
}

// CapAuthorityFrom is the pinned principal and capability, or "" and ""
// outside a capability call.
func CapAuthorityFrom(ctx context.Context) (principal, capability string) {
	a, _ := ctx.Value(ctxKeyCapAuthority{}).(capAuthority)
	return a.principal, a.capability
}

// connectionFailure is a connection ref the chassis would not honour, as
// data under `into` (`_connection` when the op names none), so a rule can
// say "connect GitHub first" and go on. Never a Go error: the request was
// not made, and nothing about the connection but the code is said.
func connectionFailure(into, provider string, e *connection.Error) event.Payload {
	if into == "" {
		into = "_connection"
	}
	raw, _ := sjson.Set(`{}`, into+".error.code", "txco_connection_"+e.Code)
	raw, _ = sjson.Set(raw, into+".error.message", e.Message)
	raw, _ = sjson.Set(raw, into+".error.connection", provider)
	return event.Payload{Raw: raw, Type: event.JSON}
}
