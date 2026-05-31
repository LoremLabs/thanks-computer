// Package auth carries the request-scoped auth context plus its helpers.
// Subpackages (signature/, registry/, policy/, nonces) build on this.
package auth

import "context"

// Context is what the auth middleware attaches to each authenticated
// request. Handlers read it via FromContext and pass it to
// policy.RequireCapability.
//
// `Source` distinguishes how the caller authenticated:
//   - "signed": RFC 9421 signed request from an enrolled actor
//   - "basic":  legacy HTTP basic auth (synthetic admin:all)
//   - "open":   no auth required (dev mode with --admin-user="")
//
// Basic-auth and open callers carry a non-empty Source but an empty
// ActorID — there's no actor record on disk for them; the capabilities
// are synthesized in-memory. Signed callers always have ActorID set.
type Context struct {
	Source  string
	ActorID string
	KeyID   string
	// Tenant is the verified actor's legacy `actors.tenant` column.
	// As of phase 1 of the multi-tenancy work, scoping is mediated by
	// the URL's tenant prefix (TenantSlug/TenantID below) and by
	// memberships rather than this column. Kept readable for tooling
	// that hasn't migrated yet; do not introduce new readers.
	Tenant string
	// TenantSlug and TenantID are set by the admin server's tenant
	// resolver middleware when the request path is `/v1/tenants/{t}/…`.
	// Empty on chassis-wide endpoints (/auth/whoami, /healthz, etc.).
	// On those tenant-scoped routes the resolver also REPLACES
	// Capabilities with the signed caller's membership caps for THIS
	// tenant (no membership → empty → denied), so a signed actor is
	// confined to the tenant in the URL. See
	// server/admin/tenant_middleware.go.
	TenantSlug string
	TenantID   string
	// SuperAdmin mirrors `actors.super_admin`. When true,
	// RequireCapability short-circuits to allow regardless of
	// memberships.
	SuperAdmin   bool
	Capabilities []string
}

// IsSigned reports whether the caller authenticated with a signed
// request (as opposed to basic auth or open dev mode).
func (c *Context) IsSigned() bool { return c != nil && c.Source == "signed" }

type ctxKey struct{}

// WithContext returns a copy of parent carrying the given auth.Context.
func WithContext(parent context.Context, c *Context) context.Context {
	return context.WithValue(parent, ctxKey{}, c)
}

// FromContext returns the auth.Context attached by middleware, or nil
// if none is set (which means the request bypassed auth, e.g. /healthz).
func FromContext(ctx context.Context) *Context {
	c, _ := ctx.Value(ctxKey{}).(*Context)
	return c
}
