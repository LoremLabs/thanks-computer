package authn

import "context"

// Authenticated is who a request acts as: the principal a head signed in,
// and the credential that proved it.
//
// It travels in Go state, never in the envelope. The head that verified the
// password pins it on the context it dispatches with (WithAuthenticated);
// the processor makes `_txc.principal` a read-only copy of that pin, and
// removes any `_txc.principal` a request arrived with when there is none.
// Access decisions read the context (processor.PrincipalScope), never the
// envelope.
type Authenticated struct {
	Principal Principal
	// Credential is the id (crd_…) of the credential that signed in: which
	// device, which printer. Not a secret.
	Credential string
}

// IsZero reports whether no one is authenticated.
func (a Authenticated) IsZero() bool { return a.Principal.IsZero() }

type ctxKeyAuthenticated struct{}

// WithAuthenticated pins a on ctx. A zero a pins "no one", which is not the
// same as no pin at all only to code that cares; AuthenticatedFrom reports
// both as ok=false.
func WithAuthenticated(ctx context.Context, a Authenticated) context.Context {
	return context.WithValue(ctx, ctxKeyAuthenticated{}, a)
}

// AuthenticatedFrom returns the principal pinned on ctx.
func AuthenticatedFrom(ctx context.Context) (Authenticated, bool) {
	a, ok := ctx.Value(ctxKeyAuthenticated{}).(Authenticated)
	return a, ok && !a.IsZero()
}
