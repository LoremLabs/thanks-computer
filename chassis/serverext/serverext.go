// Package serverext is the chassis seam for overlay-supplied HTTP routes on the
// admin server. It mirrors the clicmd / bgservice registry idiom: an overlay
// self-registers route mounters from its init() and the product binary
// activates them with a blank import. Open core registers none, so a
// self-hosted chassis exposes no extra surface and behaves identically.
//
// The seam is deliberately generic — no billing/quota/transport vocabulary. It
// knows only two mount points, matching the admin server's two relevant
// routers:
//
//   - Public: the raw mux, BEFORE the auth middleware. For endpoints that
//     authenticate themselves (e.g. a webhook verified by a third-party
//     signature). gorilla/mux matches in registration order and the auth
//     subrouter is a "/" catch-all, so public mounters MUST run before it.
//   - Tenant: the /v1/tenants/{tenant} subrouter, AFTER the auth + tenant
//     resolver middleware. Handlers read auth.FromContext for the resolved
//     slug/id and the caller's per-tenant capabilities, exactly like the
//     built-in tenant-scoped handlers.
package serverext

import "github.com/gorilla/mux"

// RouterMounter registers overlay routes onto an admin-server router.
type RouterMounter func(r *mux.Router)

// Registration happens during module init() (single-threaded), so plain slices
// without a mutex match the clicmd / bgservice seams.
var (
	publicMounters []RouterMounter
	tenantMounters []RouterMounter
)

// RegisterPublic adds a mounter for the raw (unauthenticated) mux. Called from
// a backend package's init(); the product binary activates it with a blank
// import.
func RegisterPublic(m RouterMounter) { publicMounters = append(publicMounters, m) }

// RegisterTenant adds a mounter for the /v1/tenants/{tenant} subrouter (auth +
// tenant resolver already applied). Called from a backend package's init().
func RegisterTenant(m RouterMounter) { tenantMounters = append(tenantMounters, m) }

// PublicMounters returns the registered public mounters, in registration order.
func PublicMounters() []RouterMounter { return publicMounters }

// TenantMounters returns the registered tenant-scoped mounters, in registration
// order.
func TenantMounters() []RouterMounter { return tenantMounters }
